// Command kubectl-firebolt is a kubectl plugin that manages FireboltEngine and
// FireboltInstance resources via the Firebolt Kubernetes operator. Installed on
// PATH as `kubectl-firebolt`, it is invoked as `kubectl firebolt ...`. CRs are
// built from this repo's api/v1alpha1 types and applied with kubectl against
// the host's kubeconfig context.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/infra"
)

// Output formats accepted by the list commands' -o/--output flag.
const (
	outTable = "table"
	outWide  = "wide"
	outJSON  = "json"
	outYAML  = "yaml"
	outName  = "name"
)

// kubectl resource names used in -o name output (`<resource>/<name>`).
const (
	resourceEngine   = "fireboltengine"
	resourceInstance = "fireboltinstance"
)

// version is the plugin version, overridden at build time via
// -ldflags "-X main.version=...". See the Makefile's LDFLAGS.
var version = "dev"

var (
	flagNamespace     string
	flagContext       string
	flagKubeconfig    string
	flagPrintCommands bool
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Cancel in-flight kubectl (e.g. a long `engine create` wait) on Ctrl+C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return newRootCmd().ExecuteContext(ctx)
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "firebolt",
		Short:         "Manage FireboltEngine resources on a Firebolt compute cluster via the Firebolt Kubernetes operator",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&flagNamespace, "namespace", "n", "",
		"Kubernetes namespace to operate against (defaults to the current context's namespace)")
	pf.StringVar(&flagContext, "context", "", "kubeconfig context to use (defaults to the current context)")
	pf.StringVar(&flagKubeconfig, "kubeconfig", "", "path to the kubeconfig file (defaults to $KUBECONFIG or ~/.kube/config)")
	pf.BoolVar(&flagPrintCommands, "print-commands", false,
		"Print the kubectl command(s) the chosen command would run instead of running them")
	// --debug is an alias for --print-commands: both bind the same variable, so
	// passing either one prints the kubectl commands instead of running them.
	pf.BoolVar(&flagPrintCommands, "debug", false, "Alias for --print-commands")

	root.AddCommand(newInstanceCmd(), newEngineCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}
}

// newClient builds the client. An empty --namespace is allowed: the kubectl
// calls then omit -n and fall back to the current context's namespace, matching
// native kubectl behavior.
func newClient() *infra.Client {
	return infra.NewClient(flagNamespace, flagContext, flagKubeconfig)
}

// ── instance ─────────────────────────────────────────────────────────────────

func newInstanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "instance",
		Short: "List and port-forward FireboltInstances",
	}
	cmd.AddCommand(newInstanceListCmd(), newInstancePortForwardCmd())
	return cmd
}

func newInstanceListCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List FireboltInstances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutput(output); err != nil {
				return err
			}
			c := newClient()
			if flagPrintCommands {
				fmt.Println(c.ListInstancesScript())
				return nil
			}
			if output == outJSON || output == outYAML {
				instances, err := c.ListInstanceObjects(cmd.Context())
				if err != nil {
					return err
				}
				return printObjects(output, instances)
			}
			instances, err := c.ListInstances(cmd.Context())
			if err != nil {
				return err
			}
			return printInstanceSummaries(instances, output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", outTable, "Output format: table, wide, json, yaml, or name")
	return cmd
}

func newInstancePortForwardCmd() *cobra.Command {
	var localPort int
	cmd := &cobra.Command{
		Use:   "port-forward <instance-name>",
		Short: "Port-forward to a FireboltInstance's gateway service",
		Long: `Port-forward to the gateway service of the named FireboltInstance.

The argument is the FireboltInstance name; the command forwards to
svc/<instance-name>-gateway. To route a query to a specific engine behind the
gateway, set the HTTP header "X-Firebolt-Engine: <engine-name>" — that engine
name is not this argument.`,
		Example: "  kubectl firebolt instance port-forward my-instance -n my-ns --local-port 8123",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			if flagPrintCommands {
				fmt.Println(c.PortForwardGatewayScript(args[0], localPort))
				return nil
			}
			pf, err := c.PortForwardGateway(cmd.Context(), args[0], localPort)
			if err != nil {
				return err
			}
			return runForeground(pf)
		},
	}
	cmd.Flags().IntVar(&localPort, "local-port", 0, "Bind kubectl to this local port instead of letting it pick a free one")
	return cmd
}

// ── engine ───────────────────────────────────────────────────────────────────

func newEngineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "engine",
		Short: "List, create, delete, and port-forward FireboltEngines",
	}
	cmd.AddCommand(newEngineListCmd(), newEngineCreateCmd(), newEngineDeleteCmd(), newEnginePortForwardCmd())
	return cmd
}

func newEngineListCmd() *cobra.Command {
	var instance string
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List FireboltEngines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutput(output); err != nil {
				return err
			}
			c := newClient()
			if flagPrintCommands {
				fmt.Println(c.ListEnginesScript())
				return nil
			}
			if output == outJSON || output == outYAML {
				engines, err := c.ListEngineObjects(cmd.Context(), instance)
				if err != nil {
					return err
				}
				return printObjects(output, engines)
			}
			engines, err := c.ListEngines(cmd.Context(), instance)
			if err != nil {
				return err
			}
			return printEngineSummaries(engines, output)
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", "Only list engines belonging to this FireboltInstance")
	cmd.Flags().StringVarP(&output, "output", "o", outTable, "Output format: table, wide, json, yaml, or name")
	return cmd
}

func newEngineCreateCmd() *cobra.Command {
	var (
		instance    string
		replicas    int32
		engineType  string
		image       string
		bucket      string
		storageType string
		hostPath    string
		timeout     string
	)
	cmd := &cobra.Command{
		Use:   "create <engine-name>",
		Short: "Create a FireboltEngine and wait for it to become Ready",
		Example: "  kubectl firebolt engine create my-engine --instance my-instance -n my-ns \\\n" +
			"    --type my-engine-class --bucket my-bucket --replicas 2",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := time.ParseDuration(timeout); err != nil {
				return fmt.Errorf("invalid --timeout %q (use a Go duration like 3m, 180s, or 1h): %w", timeout, err)
			}
			c := newClient()
			spec := &infra.EngineSpec{
				Name:         args[0],
				InstanceRef:  instance,
				Replicas:     replicas,
				Image:        image,
				Bucket:       bucket,
				StorageType:  storageType,
				HostPath:     hostPath,
				EngineType:   engineType,
				ReadyTimeout: timeout,
			}
			if flagPrintCommands {
				// Dry-run: don't touch the cluster, so fall back to the shallow
				// flag heuristic (can't resolve whether the class supplies storage).
				if bucket == "" && engineType == "" {
					warnNoStorage(engineType)
				}
				script, err := c.CreateEngineScript(spec)
				if err != nil {
					return err
				}
				fmt.Println(script)
				return nil
			}
			// Object storage can come from --bucket or from the referenced
			// FireboltEngineClass's customEngineConfig (which the engine inherits).
			// Naming a class isn't enough — it must actually carry storage — so
			// resolve the effective config rather than only checking the flags.
			// Warn (don't block): the operator doesn't require storage, and it may
			// be supplied another way.
			if bucket == "" && !classProvidesStorage(cmd.Context(), c, engineType) {
				warnNoStorage(engineType)
			}
			if err := c.CreateEngine(cmd.Context(), spec); err != nil {
				return err
			}
			if replicas == 0 {
				fmt.Println("Created with --replicas 0 (scale-to-zero): the engine is Stopped, " +
					"not Ready, so the readiness wait was skipped.")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&instance, "instance", "", "FireboltInstance the engine belongs to (spec.instanceRef)")
	f.Int32Var(&replicas, "replicas", 1, "Number of engine pods (spec.replicas)")
	f.StringVar(&engineType, "type", "",
		"FireboltEngineClass to reference by name (engineClassRef); omit for no class (operator defaults apply)")
	f.StringVar(&image, "image", "", "Container image in repository:tag form; omit to use the operator's default image")
	f.StringVar(&bucket, "bucket", "",
		"Object-storage bucket for managed storage; omit to inherit it from the FireboltEngineClass (--type)")
	f.StringVar(&storageType, "storage-type", "s3",
		"Managed-table storage backend for --bucket: s3 (default), gcs, or abs")
	f.StringVar(&hostPath, "host-path", "",
		"Back the engine data volume with a node hostPath at this path; omit for the operator default (emptyDir)")
	f.StringVar(&timeout, "timeout", infra.DefaultReadyTimeout,
		"How long to wait for the engine to become Ready, as a Go duration (e.g. 3m, 180s, 1h)")
	// MarkFlagRequired only errors on a nonexistent flag — a programmer bug, so
	// panic rather than discard it.
	if err := cmd.MarkFlagRequired("instance"); err != nil {
		panic(fmt.Sprintf("marking --instance required: %v", err))
	}
	return cmd
}

func newEngineDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <engine-name>",
		Short: "Delete a FireboltEngine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			if flagPrintCommands {
				fmt.Println(c.DeleteEngineScript(args[0]))
				return nil
			}
			out, err := c.DeleteEngine(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if out != "" {
				fmt.Println(out)
			} else {
				fmt.Printf("%s/%s not found; nothing to delete\n", resourceEngine, args[0])
			}
			return nil
		},
	}
}

func newEnginePortForwardCmd() *cobra.Command {
	var localPort int
	cmd := &cobra.Command{
		Use:   "port-forward <engine-name>",
		Short: "Port-forward to a FireboltEngine's service",
		Long: `Port-forward to the service of the named FireboltEngine.

The argument is the FireboltEngine name; the command forwards to
svc/<engine-name>-service.`,
		Example: "  kubectl firebolt engine port-forward my-engine -n my-ns --local-port 8123",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			if flagPrintCommands {
				fmt.Println(c.PortForwardEngineScript(args[0], localPort))
				return nil
			}
			pf, err := c.PortForwardEngine(cmd.Context(), args[0], localPort)
			if err != nil {
				return err
			}
			return runForeground(pf)
		},
	}
	cmd.Flags().IntVar(&localPort, "local-port", 0, "Bind kubectl to this local port instead of letting it pick a free one")
	return cmd
}

// classProvidesStorage best-effort reports whether the named engine class
// supplies object storage that a bucket-less engine would inherit. A probe
// failure (missing class, RBAC) is not fatal — it only feeds a warning — so it
// surfaces the failure and returns false; a genuinely missing class is then
// caught by the operator's webhook when the engine is applied.
func classProvidesStorage(ctx context.Context, c *infra.Client, engineType string) bool {
	if engineType == "" {
		return false
	}
	ok, err := c.EngineClassProvidesStorage(ctx, engineType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not verify whether engine class %q supplies object storage: %v\n", engineType, err)
		return false
	}
	return ok
}

// warnNoStorage warns that the engine has no object storage source — neither a
// --bucket nor a storage-providing class — so it may never reach Ready.
func warnNoStorage(engineType string) {
	if engineType != "" {
		fmt.Fprintf(os.Stderr, "warning: no object storage configured — neither --bucket nor engine class %q "+
			"(customEngineConfig.storage) provides a bucket; the engine may not become Ready unless storage is provided another way\n", engineType)
		return
	}
	fmt.Fprintln(os.Stderr, "warning: no object storage configured — neither --bucket nor --type given; "+
		"the engine may not become Ready unless storage is provided another way")
}

// ── helpers ──────────────────────────────────────────────────────────────────

func runForeground(pf *infra.PortForward) error {
	defer pf.Close()
	fmt.Printf("Forwarding on http://localhost:%d\n", pf.LocalPort())
	fmt.Println("Ctrl+C to stop.")
	return pf.Wait()
}

func fmtReady(b *bool) string {
	switch {
	case b == nil:
		return "-"
	case *b:
		return "true"
	default:
		return "false"
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ── output formatting ────────────────────────────────────────────────────────

// validateOutput rejects unknown -o/--output values up front, before any
// cluster call, so the user gets a clear error rather than a fetch + failure.
func validateOutput(o string) error {
	switch o {
	case outTable, outWide, outJSON, outYAML, outName, "":
		return nil
	default:
		return fmt.Errorf("unsupported output format %q (use table, wide, json, yaml, or name)", o)
	}
}

// marshalObjectList renders the listed objects as a kubectl-style v1 List
// (`kind: List` with an `items` array) in JSON or YAML, so scripts can rely on
// `.items[]` / `kind: List` exactly as with `kubectl get -o json|yaml` — rather
// than a bare array. items should be a non-nil slice so an empty result
// marshals to `[]`, not `null`.
func marshalObjectList(format string, items any) ([]byte, error) {
	list := map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"metadata":   map[string]any{},
		"items":      items,
	}
	if format == outYAML {
		return yaml.Marshal(list)
	}
	return json.MarshalIndent(list, "", "  ")
}

// printObjects writes the listed objects as a kubectl-style List in the given
// machine-readable format.
func printObjects(format string, items any) error {
	out, err := marshalObjectList(format, items)
	if err != nil {
		return err
	}
	if format == outYAML {
		fmt.Print(string(out)) // yaml.Marshal already ends with a newline
	} else {
		fmt.Println(string(out))
	}
	return nil
}

func printEngineSummaries(engines []infra.EngineSummary, output string) error {
	if output == outName {
		for _, e := range engines {
			fmt.Printf("%s/%s\n", resourceEngine, e.Name)
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	defer w.Flush()
	if output == outWide {
		fmt.Fprintln(w, "NAME\tINSTANCE\tCLASS\tREPLICAS\tPHASE\tREADY")
		for _, e := range engines {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
				e.Name, dash(e.InstanceRef), dash(e.ClassRef), e.Replicas, dash(e.Phase), fmtReady(e.Ready))
		}
		return nil
	}
	fmt.Fprintln(w, "NAME\tINSTANCE\tPHASE\tREADY")
	for _, e := range engines {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Name, dash(e.InstanceRef), dash(e.Phase), fmtReady(e.Ready))
	}
	return nil
}

func printInstanceSummaries(instances []infra.InstanceSummary, output string) error {
	if output == outName {
		for _, s := range instances {
			fmt.Printf("%s/%s\n", resourceInstance, s.Name)
		}
		return nil
	}
	// Instances carry no extra columns, so wide and table render identically.
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "NAME\tPHASE\tREADY")
	for _, s := range instances {
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, dash(s.Phase), fmtReady(s.Ready))
	}
	return nil
}
