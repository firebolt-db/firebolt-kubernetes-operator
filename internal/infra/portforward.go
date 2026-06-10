package infra

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// portForwardReadyTimeout bounds how long we wait for kubectl port-forward to
// bind before surfacing a clear error (e.g. service has no endpoints).
const portForwardReadyTimeout = 15 * time.Second

// errEarlyExit signals that kubectl closed its stdout (it exited) before ever
// printing a "Forwarding from ..." line — i.e. it failed before binding.
var errEarlyExit = errors.New("kubectl port-forward exited before it bound")

// PortForward is a running `kubectl port-forward` child. Close (or a finished
// Wait) tears the child down.
type PortForward struct {
	localPort int
	cmd       *exec.Cmd
}

// LocalPort is the bound local port (the one kubectl picked, or the requested one).
func (p *PortForward) LocalPort() int { return p.localPort }

// Wait blocks until the underlying kubectl port-forward exits. Ctrl+C in the
// foreground delivers SIGINT to the child (same process group) and unblocks it.
func (p *PortForward) Wait() error { return p.cmd.Wait() }

// Close best-effort terminates the child (it may already have exited).
func (p *PortForward) Close() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
}

func (c *Client) gatewayCmd(instance string, localPort int) KubectlCmd {
	return c.kubectl.portForward(c.namespace, "svc/"+instance+"-gateway", 80, localPort)
}

func (c *Client) engineCmd(engine string, localPort int) KubectlCmd {
	return c.kubectl.portForward(c.namespace, "svc/"+engine+"-service", 3473, localPort)
}

// PortForwardGateway forwards to an instance's <instance>-gateway service (port
// 80). Queries must carry `X-Firebolt-Engine: <engine-name>` to route to an engine.
func (c *Client) PortForwardGateway(ctx context.Context, instance string, localPort int) (*PortForward, error) {
	return spawnPortForward(ctx, c.gatewayCmd(instance, localPort))
}

// PortForwardEngine forwards to an engine's <engine>-service (port 3473).
func (c *Client) PortForwardEngine(ctx context.Context, engine string, localPort int) (*PortForward, error) {
	return spawnPortForward(ctx, c.engineCmd(engine, localPort))
}

// PortForwardGatewayScript renders the gateway port-forward (--print-commands).
func (c *Client) PortForwardGatewayScript(instance string, localPort int) string {
	return c.gatewayCmd(instance, localPort).Render()
}

// PortForwardEngineScript renders the engine port-forward (--print-commands).
func (c *Client) PortForwardEngineScript(engine string, localPort int) string {
	return c.engineCmd(engine, localPort).Render()
}

// spawnPortForward starts the kubectl port-forward and blocks until it binds.
// The bound local port is parsed from kubectl's stdout; stderr inherits so the
// user sees connection errors mid-session. It returns early if the child exits
// before binding (e.g. missing service or RBAC denial — stdout closes with no
// "Forwarding from ..." line), if ctx is canceled (interrupt/deadline), or if
// the bind times out — in every case the child is reaped.
func spawnPortForward(ctx context.Context, kc KubectlCmd) (*PortForward, error) {
	// args are built from fixed kubectl verbs + validated k8s names, never a shell.
	cmd := exec.CommandContext(ctx, "kubectl", kc.Args()...) //nolint:gosec // G204: see above
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("wiring kubectl port-forward stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting kubectl port-forward: %w", err)
	}

	portCh := make(chan int, 1)
	// stdoutClosed is closed when the scanner loop ends, i.e. kubectl closed
	// stdout (it exited). If that happens before a port is seen, the forward
	// failed before binding and we must not keep waiting for a line that will
	// never come.
	stdoutClosed := make(chan struct{})
	go func() {
		defer close(stdoutClosed)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if port, ok := parsePort(sc.Text()); ok {
				select {
				case portCh <- port:
				default:
				}
				// Keep draining so kubectl doesn't block on a full pipe;
				// subsequent connection logs are ignored.
			}
		}
	}()

	port, err := awaitBoundPort(ctx, portCh, stdoutClosed, portForwardReadyTimeout, kc.Args())
	if err != nil {
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		if errors.Is(err, errEarlyExit) {
			// The child is already gone; waitErr carries its exit status, and
			// the real cause already streamed to stderr above.
			base := fmt.Sprintf("kubectl port-forward exited before it bound (kubectl %s)", strings.Join(kc.Args(), " "))
			if waitErr != nil {
				return nil, fmt.Errorf("%s: %w (see the kubectl error above)", base, waitErr)
			}
			return nil, errors.New(base + " (see the kubectl error above)")
		}
		return nil, err
	}
	return &PortForward{localPort: port, cmd: cmd}, nil
}

// awaitBoundPort waits for the bound local port on portCh, returning early if
// the child exits before binding (stdoutClosed), if ctx is canceled, or if the
// bind timeout elapses. Selecting on stdoutClosed and ctx (not just the
// timeout) means an immediate kubectl failure or a canceled foreground command
// fails fast with the right cause instead of blocking the full timeout and
// reporting a misleading "timed out" error.
func awaitBoundPort(ctx context.Context, portCh <-chan int, stdoutClosed <-chan struct{}, timeout time.Duration, args []string) (int, error) {
	bindCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case port := <-portCh:
		return port, nil
	case <-stdoutClosed:
		// kubectl closed stdout — it exited. If it had already printed a
		// "Forwarding from ..." line, that port is buffered in portCh (the
		// scanner sends it before closing stdoutClosed), so prefer it: select
		// picks a ready case at random, so without this a parsed port could be
		// lost to a false errEarlyExit. Otherwise it exited before binding.
		return portOrErr(portCh, errEarlyExit)
	case <-bindCtx.Done():
		// ctx canceled (interrupt/deadline) takes precedence over our bind
		// timeout, which is just bindCtx's own DeadlineExceeded.
		if ctx.Err() != nil {
			return portOrErr(portCh, fmt.Errorf("kubectl port-forward canceled before it bound (kubectl %s): %w",
				strings.Join(args, " "), ctx.Err()))
		}
		return portOrErr(portCh, fmt.Errorf("timed out waiting for kubectl port-forward to bind (kubectl %s)",
			strings.Join(args, " ")))
	}
}

// portOrErr prefers an already-parsed port over err. The top-level select picks
// a ready case at random, so a port queued at the same instant kubectl exited
// (or the timeout fired) must not be reported as a failure.
func portOrErr(portCh <-chan int, err error) (int, error) {
	select {
	case port := <-portCh:
		return port, nil
	default:
		return 0, err
	}
}

// parsePort parses `Forwarding from 127.0.0.1:8080 -> 80` to 8080. It skips the
// IPv6 line and any unrelated output.
func parsePort(line string) (int, bool) {
	rest, ok := strings.CutPrefix(line, "Forwarding from 127.0.0.1:")
	if !ok {
		return 0, false
	}
	digits := rest
	if i := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = rest[:i]
	}
	port, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return port, true
}
