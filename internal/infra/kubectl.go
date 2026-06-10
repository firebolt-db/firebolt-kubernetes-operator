// Package infra provides the kubectl-firebolt plugin's engine/instance
// management against a Firebolt operator cluster. CRs are built from the
// operator's own api/v1alpha1 types (so a CRD field change is a compile error
// here) and applied by shelling out to kubectl against the host's kubeconfig
// context.
package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"sigs.k8s.io/yaml"
)

// KubectlCmd is a single kubectl invocation modeled as data, so the same
// command can either be executed (Run/Capture) or rendered as a copy-pasteable
// shell command (Render). Building the argv in one place is what guarantees
// --print-commands output matches what the plugin actually runs.
type KubectlCmd struct {
	args []string
	// stdinManifest is YAML piped to stdin (only apply uses it); nil otherwise.
	stdinManifest []byte
	// comment is the leading `#` line in rendered output.
	comment string
	// hint, when set, is appended to the error if Run fails — a human-facing
	// next step (e.g. "run kubectl describe ..."). It is not part of Render(),
	// so --print-commands output is unaffected.
	hint string
}

// Args returns the arguments following `kubectl` — used to spawn long-running
// commands (port-forward) where the caller owns the child process.
func (c KubectlCmd) Args() []string { return c.args }

// Run executes the command, inheriting stdout/stderr. For apply, the manifest
// is piped to stdin as YAML. Canceling ctx (e.g. Ctrl+C) terminates kubectl.
func (c KubectlCmd) Run(ctx context.Context) error {
	// args are built from fixed kubectl verbs + validated k8s names, never a shell.
	cmd := exec.CommandContext(ctx, "kubectl", c.args...) //nolint:gosec // G204: see above
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if c.stdinManifest != nil {
		cmd.Stdin = bytes.NewReader(c.stdinManifest)
	}
	if err := cmd.Run(); err != nil {
		if c.hint != "" {
			return fmt.Errorf("kubectl %s: %w\n%s", strings.Join(c.args, " "), err, c.hint)
		}
		return fmt.Errorf("kubectl %s: %w", strings.Join(c.args, " "), err)
	}
	return nil
}

// Capture executes the command and returns its stdout (for read-only gets).
// stderr is folded into the error on failure.
func (c KubectlCmd) Capture(ctx context.Context) (string, error) {
	// args are built from fixed kubectl verbs + validated k8s names, never a shell.
	cmd := exec.CommandContext(ctx, "kubectl", c.args...) //nolint:gosec // G204: see above
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl %s failed: %w\n%s", strings.Join(c.args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// Render returns a shell command that, run verbatim, has the same effect as
// Run. An apply is rendered with its manifest fed via a quoted heredoc
// (<<'EOF'), so nothing in the YAML is shell-expanded.
func (c KubectlCmd) Render() string {
	cmd := "kubectl " + strings.Join(c.args, " ")
	if c.stdinManifest != nil {
		// sigs.k8s.io/yaml terminates the document with a newline, so EOF lands
		// on its own line.
		return fmt.Sprintf("# %s\n%s <<'EOF'\n%sEOF", c.comment, cmd, string(c.stdinManifest))
	}
	return fmt.Sprintf("# %s\n%s", c.comment, cmd)
}

// renderScript renders a plan as a single shell script, commands separated by a
// blank line. Suitable for printing for --print-commands.
func renderScript(cmds []KubectlCmd) string {
	parts := make([]string, len(cmds))
	for i, c := range cmds {
		parts[i] = c.Render()
	}
	return strings.Join(parts, "\n\n")
}

// kubectl carries the global flags (--context/--kubeconfig) prepended to every
// invocation, so the namespace-scoped Client can build commands without
// threading them through each call site.
type kubectl struct {
	global []string
}

func (k kubectl) base(extra ...string) []string {
	out := make([]string, 0, len(k.global)+len(extra))
	out = append(out, k.global...)
	out = append(out, extra...)
	return out
}

// withNamespace appends `-n <namespace>` when namespace is non-empty; an empty
// namespace lets kubectl fall back to the current context's namespace, matching
// native kubectl behavior.
func withNamespace(args []string, namespace string) []string {
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return args
}

// apply is `kubectl apply [-n <namespace>] -f -`, with manifest piped to stdin.
func (k kubectl) apply(namespace string, manifest []byte, comment string) KubectlCmd {
	args := append(withNamespace(k.base("apply"), namespace), "-f", "-")
	return KubectlCmd{args: args, stdinManifest: manifest, comment: comment}
}

// delete is `kubectl delete [-n <namespace>] <resource> <name> --ignore-not-found`.
func (k kubectl) delete(namespace, resource, name string) KubectlCmd {
	args := append(withNamespace(k.base("delete"), namespace), resource, name, "--ignore-not-found")
	return KubectlCmd{args: args, comment: fmt.Sprintf("delete %s/%s (ignore if absent)", resource, name)}
}

// wait is `kubectl wait [-n <namespace>] --for=condition=Ready <resource>/<name> --timeout=<timeout>`.
// On failure (typically a timeout) the error carries a hint to describe the
// resource, since `kubectl wait` itself only reports "timed out".
func (k kubectl) wait(namespace, resource, name, timeout string) KubectlCmd {
	args := append(withNamespace(k.base("wait"), namespace),
		"--for=condition=Ready", resource+"/"+name, "--timeout="+timeout)
	// Build the describe hint with the same global flags (--context /
	// --kubeconfig) and namespace the wait used, so it points at the cluster
	// being debugged rather than the ambient context.
	describeArgs := withNamespace(k.base("describe", resource+"/"+name), namespace)
	return KubectlCmd{
		args:    args,
		comment: fmt.Sprintf("wait for %s/%s to become Ready", resource, name),
		hint: fmt.Sprintf("%s/%s did not become Ready in time; run `kubectl %s` to see why",
			resource, name, strings.Join(describeArgs, " ")),
	}
}

// getNamed is `kubectl get [-n <namespace>] <resource> <name> -o json`, fetching
// a single named object.
func (k kubectl) getNamed(namespace, resource, name string) KubectlCmd {
	args := append(withNamespace(k.base("get"), namespace), resource, name, "-o", "json")
	return KubectlCmd{args: args, comment: fmt.Sprintf("get %s/%s", resource, name)}
}

// get is `kubectl get [-n <namespace>] <resource> -o json`, listing the resource.
func (k kubectl) get(namespace, resource string) KubectlCmd {
	args := append(withNamespace(k.base("get"), namespace), resource, "-o", "json")
	return KubectlCmd{args: args, comment: "list " + resource}
}

// portForward is `kubectl port-forward [-n <namespace>] <target> <local?>:<remote>`.
// A localPort of 0 renders `:<remote>` and lets kubectl pick a free local port.
func (k kubectl) portForward(namespace, target string, remotePort, localPort int) KubectlCmd {
	portArg := fmt.Sprintf(":%d", remotePort)
	if localPort != 0 {
		portArg = fmt.Sprintf("%d:%d", localPort, remotePort)
	}
	args := append(withNamespace(k.base("port-forward"), namespace), target, portArg)
	return KubectlCmd{args: args, comment: fmt.Sprintf("port-forward to %s (Ctrl+C to stop)", target)}
}

// toYAML marshals a typed object into the YAML fed to `kubectl apply -f -`
// (both when executing and when rendering, so the two cannot drift). It then
// drops fields that have no place in an apply manifest: the operator-owned
// status subresource (kubectl ignores status on apply anyway) and the empty
// objects a fully-typed struct leaves behind (e.g. an unset pod-template
// metadata or container resources). The round-trip uses the Kubernetes types'
// json tags so the output matches what the API server expects.
func toYAML(obj any) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "status")
	pruneEmpty(m)
	return yaml.Marshal(m)
}

// pruneEmpty recursively removes map keys whose value is null or an empty map,
// depth first so a map emptied by pruning its children is removed too. It
// descends into slices so empties nested in lists (e.g. a container's
// resources) are cleaned as well. It only removes nulls and empty objects —
// never empty strings, zero numbers, or empty slices — so no meaningful value
// is dropped. (A null in an apply manifest just means "unset"; e.g. an omitted
// engine image leaves spec.template.spec.containers serialized as null.)
func pruneEmpty(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if child == nil {
				delete(t, k)
				continue
			}
			pruneEmpty(child)
			if cm, ok := child.(map[string]any); ok && len(cm) == 0 {
				delete(t, k)
			}
		}
	case []any:
		for _, child := range t {
			pruneEmpty(child)
		}
	}
}
