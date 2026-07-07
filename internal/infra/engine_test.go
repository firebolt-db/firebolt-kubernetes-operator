package infra

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func sampleSpec() *EngineSpec {
	return &EngineSpec{
		Name:        "my-engine",
		InstanceRef: "my-instance",
		Replicas:    4,
		Image:       "registry.example.com/engine:v1.2.3",
		Bucket:      "my-test-bucket",
		StorageType: "s3",
		HostPath:    "/mnt/data/my-engine",
		EngineType:  "my-engine-class",
	}
}

func buildEngine(t *testing.T) *v1alpha1.FireboltEngine {
	t.Helper()
	e, err := buildFireboltEngine("test-ns", sampleSpec())
	if err != nil {
		t.Fatalf("build engine: %v", err)
	}
	return e
}

func createScript(t *testing.T) string {
	t.Helper()
	c := NewClient("test-ns", "", "")
	plan, err := c.planCreateEngine(sampleSpec())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return renderScript(plan)
}

func TestEngineReferencesSharedClass(t *testing.T) {
	e := buildEngine(t)
	if e.Spec.EngineClassRef == nil || *e.Spec.EngineClassRef != "my-engine-class" {
		t.Errorf("engineClassRef = %v, want \"my-engine-class\"", e.Spec.EngineClassRef)
	}
}

func TestEngineTemplateCarriesImageAndNoInjectedScheduling(t *testing.T) {
	e := buildEngine(t)
	if e.Spec.Template == nil {
		t.Fatal("spec.template must be set when --image is given")
	}
	var img string
	for _, ct := range e.Spec.Template.Spec.Containers {
		if ct.Name == "engine" {
			img = ct.Image
		}
	}
	if img != "registry.example.com/engine:v1.2.3" {
		t.Errorf("engine image = %q", img)
	}
	// The plugin must not inject scheduling — that belongs in the EngineClass
	// (and self-referential required podAffinity deadlocked the first pod).
	if e.Spec.Template.Spec.Affinity != nil {
		t.Errorf("plugin must not inject affinity, got %+v", e.Spec.Template.Spec.Affinity)
	}
}

func TestEngineCarriesPerEngineFields(t *testing.T) {
	e := buildEngine(t)
	if e.Kind != "FireboltEngine" || e.Name != "my-engine" {
		t.Errorf("kind/name = %s/%s", e.Kind, e.Name)
	}
	if e.Spec.InstanceRef != "my-instance" {
		t.Errorf("instanceRef = %q", e.Spec.InstanceRef)
	}
	if e.Spec.Replicas != 4 {
		t.Errorf("replicas = %d", e.Spec.Replicas)
	}
	if e.Spec.Storage.HostPath == nil || e.Spec.Storage.HostPath.Path != "/mnt/data/my-engine" {
		t.Errorf("hostPath = %v", e.Spec.Storage.HostPath)
	}
	if e.Spec.CustomEngineConfig == nil || !strings.Contains(string(e.Spec.CustomEngineConfig.Raw), "my-test-bucket") {
		t.Errorf("customEngineConfig = %v", e.Spec.CustomEngineConfig)
	}
}

func TestCreateScriptIsApplyThenWaitNoPerEngineClass(t *testing.T) {
	script := createScript(t)
	if got := strings.Count(script, "kubectl apply -n test-ns -f - <<'EOF'"); got != 1 {
		t.Errorf("want exactly 1 apply (just the FireboltEngine), got %d:\n%s", got, script)
	}
	if strings.Contains(script, "kind: FireboltEngineClass") {
		t.Errorf("create must not apply a per-engine FireboltEngineClass:\n%s", script)
	}
	if !strings.Contains(script, "kind: FireboltEngine") {
		t.Errorf("missing FireboltEngine apply:\n%s", script)
	}
	if !strings.Contains(script, "kubectl wait -n test-ns --for=condition=Ready fireboltengine/my-engine --timeout=5m") {
		t.Errorf("missing wait:\n%s", script)
	}
	// The applied manifest embeds image, shared class ref, replicas, bucket.
	for _, want := range []string{
		"registry.example.com/engine:v1.2.3",
		"engineClassRef: my-engine-class",
		"replicas: 4",
		"managed_table_bucket_name: my-test-bucket",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

func TestCreateScriptOmitsStatusAndEmptyObjects(t *testing.T) {
	script := createScript(t)
	for _, unwanted := range []string{
		"status:",          // operator-owned subresource — not for apply
		"activeGeneration", // a non-omitempty status field
		"currentGeneration",
		"metadata: {}",  // empty pod-template metadata
		"resources: {}", // unset container resources
	} {
		if strings.Contains(script, unwanted) {
			t.Errorf("apply manifest should not contain %q:\n%s", unwanted, script)
		}
	}
}

func TestDeleteScriptRemovesOnlyTheEngine(t *testing.T) {
	c := NewClient("test-ns", "", "")
	script := c.DeleteEngineScript("my-engine")
	if !strings.Contains(script, "kubectl delete -n test-ns fireboltengine my-engine --ignore-not-found") {
		t.Errorf("delete engine missing:\n%s", script)
	}
	if strings.Contains(script, "fireboltengineclass") {
		t.Errorf("delete must not touch a per-engine class:\n%s", script)
	}
}

func TestCreateWithoutImageOrTypeUsesOperatorDefaults(t *testing.T) {
	spec := sampleSpec()
	spec.Image = ""
	spec.EngineType = ""
	e, err := buildFireboltEngine("test-ns", spec)
	if err != nil {
		t.Fatal(err)
	}
	if e.Spec.EngineClassRef != nil {
		t.Errorf("engineClassRef must be nil when --type omitted, got %q", *e.Spec.EngineClassRef)
	}
	// No --image -> no template override at all (operator default image applies).
	if e.Spec.Template != nil {
		t.Errorf("spec.template must be nil when --image omitted, got %+v", e.Spec.Template)
	}
	// The rendered manifest must not leak engineClassRef or a template block.
	c := NewClient("test-ns", "", "")
	plan, err := c.planCreateEngine(spec)
	if err != nil {
		t.Fatal(err)
	}
	script := renderScript(plan)
	for _, unwanted := range []string{"engineClassRef", "template:", "containers:"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("manifest should not contain %q when omitted:\n%s", unwanted, script)
		}
	}
}

func TestStorageHostPathIsOptIn(t *testing.T) {
	// Omitted: no storage override -> operator default (emptyDir).
	spec := sampleSpec()
	spec.HostPath = ""
	e, err := buildFireboltEngine("ns", spec)
	if err != nil {
		t.Fatal(err)
	}
	if e.Spec.Storage.HostPath != nil {
		t.Errorf("hostPath must be unset when --host-path omitted, got %+v", e.Spec.Storage.HostPath)
	}
	plan, err := NewClient("ns", "", "").planCreateEngine(spec)
	if err != nil {
		t.Fatal(err)
	}
	// (customEngineConfig has its own nested "storage:" key, so assert on the
	// hostPath backend specifically rather than the word "storage".)
	if script := renderScript(plan); strings.Contains(script, "hostPath") {
		t.Errorf("no hostPath storage expected when --host-path omitted:\n%s", script)
	}

	// Set: hostPath backend at the given path.
	spec.HostPath = "/mnt/nvme/eng"
	e, err = buildFireboltEngine("ns", spec)
	if err != nil {
		t.Fatal(err)
	}
	if e.Spec.Storage.HostPath == nil || e.Spec.Storage.HostPath.Path != "/mnt/nvme/eng" {
		t.Errorf("hostPath = %+v, want path /mnt/nvme/eng", e.Spec.Storage.HostPath)
	}
}

func TestCreateWithZeroReplicasSkipsReadyWait(t *testing.T) {
	spec := sampleSpec()
	spec.Replicas = 0 // scale-to-zero: operator parks it Stopped (Ready=False)
	plan, err := NewClient("test-ns", "", "").planCreateEngine(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 {
		t.Fatalf("replicas=0 plan should be apply-only, got %d commands", len(plan))
	}
	script := renderScript(plan)
	if strings.Contains(script, "kubectl wait") || strings.Contains(script, "condition=Ready") {
		t.Errorf("replicas=0 must not wait for Ready (engine is intentionally Stopped):\n%s", script)
	}
	if !strings.Contains(script, "kubectl apply") || !strings.Contains(script, "replicas: 0") {
		t.Errorf("expected an apply of a replicas: 0 engine:\n%s", script)
	}
}

func TestStorageConfigSetsBackendAndBucket(t *testing.T) {
	raw := string(buildEngine(t).Spec.CustomEngineConfig.Raw)
	for _, want := range []string{`"managed_table_storage":"s3"`, `"managed_table_bucket_name":"my-test-bucket"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("storage config missing %s, got %s", want, raw)
		}
	}
}

func TestBuildEngineOmitsStorageWhenBucketEmpty(t *testing.T) {
	// Builder behavior: no bucket -> no storage block. --bucket is optional
	// because object storage can instead come from the referenced
	// FireboltEngineClass (its customEngineConfig).
	spec := sampleSpec()
	spec.Bucket = ""
	e, err := buildFireboltEngine("test-ns", spec)
	if err != nil {
		t.Fatal(err)
	}
	if e.Spec.CustomEngineConfig != nil {
		t.Errorf("customEngineConfig must be omitted when no bucket, got %s", e.Spec.CustomEngineConfig.Raw)
	}
}

func TestStorageBackendIsConfigurableNotHardcodedToS3(t *testing.T) {
	spec := sampleSpec()
	spec.StorageType = "gcs" // GCS, not S3
	e, err := buildFireboltEngine("test-ns", spec)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(e.Spec.CustomEngineConfig.Raw)
	if !strings.Contains(raw, `"managed_table_storage":"gcs"`) {
		t.Errorf("backend must follow flags, got %s", raw)
	}
	if strings.Contains(raw, "s3") {
		t.Errorf("must not hardcode s3, got %s", raw)
	}
}

func TestReadyTimeoutDefaultsAndOverrides(t *testing.T) {
	c := NewClient("test-ns", "", "")
	// Empty ReadyTimeout falls back to DefaultReadyTimeout.
	plan, err := c.planCreateEngine(sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	if got := renderScript(plan); !strings.Contains(got, "--timeout="+DefaultReadyTimeout) {
		t.Errorf("default wait should use %s:\n%s", DefaultReadyTimeout, got)
	}
	// An explicit ReadyTimeout flows through to --timeout verbatim (not normalized).
	spec := sampleSpec()
	spec.ReadyTimeout = "3m"
	plan, err = c.planCreateEngine(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderScript(plan); !strings.Contains(got, "--timeout=3m") || strings.Contains(got, "3m0s") {
		t.Errorf("custom --timeout should render 3m verbatim (not normalized to 3m0s):\n%s", got)
	}
}

func TestWaitCommandCarriesDescribeHint(t *testing.T) {
	cmd := kubectl{}.wait("test-ns", resourceEngine, "my-engine", "10m")
	if !strings.Contains(cmd.hint, "kubectl describe fireboltengine/my-engine -n test-ns") {
		t.Errorf("wait hint should point to kubectl describe, got %q", cmd.hint)
	}
	// The hint is for failures only; it must not leak into --print-commands output.
	if strings.Contains(cmd.Render(), "describe") {
		t.Errorf("hint must not appear in rendered command:\n%s", cmd.Render())
	}
}

func TestWaitHintCarriesGlobalFlags(t *testing.T) {
	// The describe hint must target the same cluster the wait used, so it has to
	// carry the global --context/--kubeconfig flags, not just -n.
	k := kubectl{global: []string{"--context", "prod", "--kubeconfig", "/tmp/kc"}}
	cmd := k.wait("test-ns", resourceEngine, "my-engine", "5m")
	for _, want := range []string{"--context prod", "--kubeconfig /tmp/kc", "-n test-ns"} {
		if !strings.Contains(cmd.hint, want) {
			t.Errorf("wait hint missing %q, got %q", want, cmd.hint)
		}
	}
}

func TestCustomConfigHasBucket(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"with bucket", `{"storage":{"managed_table_storage":"s3","managed_table_bucket_name":"my-bucket"}}`, true},
		{"storage but empty bucket", `{"storage":{"managed_table_storage":"s3","managed_table_bucket_name":""}}`, false},
		{"storage without bucket", `{"storage":{"managed_table_storage":"s3"}}`, false},
		{"no storage section", `{"logging":{"level":"debug"}}`, false},
		{"empty object", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := customConfigHasBucket(&apiextensionsv1.JSON{Raw: []byte(tc.raw)}); got != tc.want {
				t.Errorf("customConfigHasBucket(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
	if customConfigHasBucket(nil) {
		t.Error("nil customEngineConfig must report no bucket")
	}
}

func TestEmptyNamespaceOmitsNamespaceFlagAndField(t *testing.T) {
	c := NewClient("", "", "")
	plan, err := c.planCreateEngine(sampleSpec())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	script := renderScript(plan)
	if strings.Contains(script, " -n ") {
		t.Errorf("empty namespace must not pass -n (kubectl uses the context default):\n%s", script)
	}
	if strings.Contains(script, "namespace:") {
		t.Errorf("empty namespace must not stamp metadata.namespace:\n%s", script)
	}
	if strings.Contains(c.DeleteEngineScript("demo"), " -n ") {
		t.Error("delete with empty namespace must not pass -n")
	}
}
