package infra

// engine.go: FireboltEngine lifecycle — create, list, delete.
//
// create builds a FireboltEngine from explicit inputs only — it injects no
// opinionated defaults or scheduling. Optional inputs (image, hostPath, bucket,
// engineClassRef) are set only when given; anything omitted falls through to the
// operator's defaults or the referenced FireboltEngineClass. Placement and
// hardware (node/pod affinity, resources, service account, init containers) come
// from the class, not from here. No per-engine class is created.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	// kubectl resource names (CRD plurals/singulars the API server accepts).
	resourceEngine      = "fireboltengine"
	resourceInstance    = "fireboltinstance"
	resourceEngineClass = "fireboltengineclass"

	// engineContainerName is the operator-owned primary container whose image
	// the engine's spec.template overrides.
	engineContainerName = "engine"

	// DefaultReadyTimeout is the fallback for how long `engine create` waits for
	// an engine to report Ready when --timeout is not given. A Go duration
	// string, passed through to `kubectl wait --timeout` verbatim. Exported so
	// the CLI can use it as the --timeout flag default (single source of truth).
	DefaultReadyTimeout = "5m"
)

// EngineSpec is the user-supplied input for an engine. The namespace is carried
// by the Client, not duplicated here.
type EngineSpec struct {
	Name        string
	InstanceRef string
	Replicas    int32
	// Image is the container image in repository:tag form. Empty means the
	// operator uses its embedded default engine image.
	Image string
	// Bucket is the object-storage bucket for managed storage. Optional: it may
	// instead be supplied by the referenced FireboltEngineClass (via its
	// customEngineConfig), so the builder guards on empty and omits the storage
	// block entirely when unset rather than emitting an incomplete one.
	Bucket string
	// StorageType is the managed-table storage backend selector (e.g. "s3",
	// "gcs", "abs") — customEngineConfig.storage.managed_table_storage. Every
	// documented engine manifest sets it; without it the storage config is incomplete.
	StorageType string
	// HostPath, when set, backs the engine's data volume with a node hostPath
	// at this path. Empty means no storage override is emitted — the operator's
	// default (emptyDir) or the referenced class applies.
	HostPath string
	// EngineType is the name of a FireboltEngineClass on the cluster to
	// reference (the class encodes the engine's hardware and scheduling). It
	// becomes the engine's engineClassRef. Empty means no class reference — the
	// operator's defaults apply. Free-form: class names are cloud/deployment
	// specific, so the plugin does not constrain them.
	EngineType string
	// ReadyTimeout bounds how long create waits for the engine to report Ready,
	// as a Go duration string (e.g. "3m", "180s") passed through to
	// `kubectl wait`. Empty means use DefaultReadyTimeout.
	ReadyTimeout string
}

// EngineSummary is a one-line view of a FireboltEngine for `engine list`.
type EngineSummary struct {
	Name        string
	InstanceRef string
	ClassRef    string
	Replicas    int32
	Phase       string
	Ready       *bool
}

// CreateEngine applies the FireboltEngine and blocks until it reports Ready.
func (c *Client) CreateEngine(ctx context.Context, spec *EngineSpec) error {
	plan, err := c.planCreateEngine(spec)
	if err != nil {
		return err
	}
	for _, cmd := range plan {
		if err := cmd.Run(ctx); err != nil {
			return err
		}
	}
	return nil
}

// CreateEngineScript renders what CreateEngine would run as a copy-pasteable
// shell script instead of executing it (--print-commands). Pure: it touches the
// cluster for nothing, so no context is needed.
func (c *Client) CreateEngineScript(spec *EngineSpec) (string, error) {
	plan, err := c.planCreateEngine(spec)
	if err != nil {
		return "", err
	}
	return renderScript(plan), nil
}

// planCreateEngine is the ordered plan: apply the FireboltEngine, then (unless
// it's a scale-to-zero create) wait for Ready. Both CreateEngine and
// CreateEngineScript consume it, so executing and printing cannot drift.
//
// replicas=0 is scale-to-zero: the operator parks the engine in the terminal
// Stopped phase with Ready=False by design, so there is no Ready to wait for —
// waiting would just block until timeout on a create that already succeeded.
func (c *Client) planCreateEngine(spec *EngineSpec) ([]KubectlCmd, error) {
	engine, err := buildFireboltEngine(c.namespace, spec)
	if err != nil {
		return nil, err
	}
	engineYAML, err := toYAML(engine)
	if err != nil {
		return nil, err
	}
	plan := []KubectlCmd{
		c.kubectl.apply(c.namespace, engineYAML, fmt.Sprintf("apply FireboltEngine/%s", spec.Name)),
	}
	if spec.Replicas > 0 {
		timeout := spec.ReadyTimeout
		if timeout == "" {
			timeout = DefaultReadyTimeout
		}
		plan = append(plan, c.kubectl.wait(c.namespace, resourceEngine, spec.Name, timeout))
	}
	return plan, nil
}

// DeleteEngine removes the FireboltEngine (ignoring not-found). There is no
// per-engine class to clean up. It returns kubectl's confirmation line
// ("<resource>.<group>/<name> deleted") on success, or "" when
// --ignore-not-found matched nothing, so the caller can report either outcome.
func (c *Client) DeleteEngine(ctx context.Context, name string) (string, error) {
	out, err := c.kubectl.delete(c.namespace, resourceEngine, name).Capture(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// DeleteEngineScript renders the delete (--print-commands).
func (c *Client) DeleteEngineScript(name string) string {
	return c.kubectl.delete(c.namespace, resourceEngine, name).Render()
}

// ListEnginesScript renders the get that backs ListEngines (--print-commands).
func (c *Client) ListEnginesScript() string {
	return c.kubectl.get(c.namespace, resourceEngine).Render()
}

// ListEngineObjects lists the FireboltEngine objects in the namespace,
// optionally filtered by spec.instanceRef. It backs the summary view
// (ListEngines) from a single kubectl get.
func (c *Client) ListEngineObjects(ctx context.Context, filterInstance string) ([]v1alpha1.FireboltEngine, error) {
	out, err := c.kubectl.get(c.namespace, resourceEngine).Capture(ctx)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.FireboltEngineList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing FireboltEngine list: %w", err)
	}
	if filterInstance == "" {
		return list.Items, nil
	}
	filtered := make([]v1alpha1.FireboltEngine, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.InstanceRef == filterInstance {
			filtered = append(filtered, list.Items[i])
		}
	}
	return filtered, nil
}

// ListEngines lists FireboltEngines in the namespace, optionally filtered by
// spec.instanceRef, as one-line summaries.
func (c *Client) ListEngines(ctx context.Context, filterInstance string) ([]EngineSummary, error) {
	engines, err := c.ListEngineObjects(ctx, filterInstance)
	if err != nil {
		return nil, err
	}
	summaries := make([]EngineSummary, 0, len(engines))
	for i := range engines {
		e := &engines[i]
		summaries = append(summaries, EngineSummary{
			Name:        e.Name,
			InstanceRef: e.Spec.InstanceRef,
			ClassRef:    ptr.Deref(e.Spec.EngineClassRef, ""),
			Replicas:    e.Spec.Replicas,
			Phase:       string(e.Status.Phase),
			Ready:       readyFromConditions(e.Status.Conditions),
		})
	}
	return summaries, nil
}

// buildFireboltEngine builds the FireboltEngine from explicit inputs only — it
// injects no opinionated defaults. It carries instance ref and replicas, plus,
// when given: the engine image (spec.template), a hostPath storage backend, the
// managed-storage config (bucket), and engineClassRef. Anything not provided is
// omitted so the operator's defaults / the referenced class apply.
func buildFireboltEngine(namespace string, spec *EngineSpec) (*v1alpha1.FireboltEngine, error) {
	// Managed-storage config (object storage) is set per-engine here only when a
	// bucket is given; it may instead be inherited from the referenced
	// FireboltEngineClass (its customEngineConfig deep-merges beneath the
	// engine's), so --bucket is optional. The block needs a backend type plus the
	// bucket name; the backend is caller-controlled (StorageType) so the plugin
	// isn't tied to S3 — GCS/ABS work too. The builder guards on empty bucket so
	// an unset bucket omits the block rather than emitting an incomplete one.
	var customConfig *apiextensionsv1.JSON
	if spec.Bucket != "" {
		raw, err := json.Marshal(map[string]any{
			"storage": map[string]any{
				"managed_table_storage":     spec.StorageType,
				"managed_table_bucket_name": spec.Bucket,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("encoding customEngineConfig: %w", err)
		}
		customConfig = &apiextensionsv1.JSON{Raw: raw}
	}
	var classRef *string
	if spec.EngineType != "" {
		classRef = ptr.To(spec.EngineType)
	}
	// Storage backend is opt-in: only set hostPath when --host-path is given.
	// Otherwise leave Storage zero — it marshals to `storage: {}`, which the
	// manifest pruner drops, so the operator applies its emptyDir default.
	var storage v1alpha1.EngineStorageSpec
	if spec.HostPath != "" {
		storage.HostPath = &v1alpha1.EngineHostPathSpec{
			Path: spec.HostPath,
			Type: ptr.To(corev1.HostPathDirectoryOrCreate),
		}
	}
	return &v1alpha1.FireboltEngine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "FireboltEngine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
		},
		Spec: v1alpha1.FireboltEngineSpec{
			InstanceRef:        spec.InstanceRef,
			EngineClassRef:     classRef,
			Replicas:           spec.Replicas,
			Template:           engineTemplate(spec),
			Storage:            storage,
			CustomEngineConfig: customConfig,
		},
	}, nil
}

// engineTemplate builds the per-engine pod-template override: only the engine
// container image, and only when --image is given. Returns nil when no image is
// set, so spec.template is omitted entirely and the operator's defaults / the
// referenced FireboltEngineClass govern the pod.
//
// The plugin deliberately injects no scheduling here. Placement (node/pod
// affinity, topology spread, same-AZ co-location) belongs in the
// FireboltEngineClass: the operator replaces — does not merge — the class's
// Affinity with the engine template's (see effectiveAffinity), so any affinity
// set here would silently drop the class's instance-type and anti-affinity
// rules. It also avoids the self-referential required podAffinity that left the
// first pod unschedulable.
func engineTemplate(spec *EngineSpec) *corev1.PodTemplateSpec {
	if spec.Image == "" {
		return nil
	}
	return &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: engineContainerName, Image: spec.Image}},
		},
	}
}

// EngineClassProvidesStorage reports whether the named FireboltEngineClass
// carries object-storage config (customEngineConfig.storage.managed_table_bucket_name). A
// referencing engine that sets no --bucket inherits the class's config, so this
// lets `engine create` tell whether the effective config will actually have a
// bucket rather than only checking that some class was named.
func (c *Client) EngineClassProvidesStorage(ctx context.Context, name string) (bool, error) {
	out, err := c.kubectl.getNamed(c.namespace, resourceEngineClass, name).Capture(ctx)
	if err != nil {
		return false, err
	}
	var class v1alpha1.FireboltEngineClass
	if err := json.Unmarshal([]byte(out), &class); err != nil {
		return false, fmt.Errorf("parsing FireboltEngineClass %q: %w", name, err)
	}
	return customConfigHasBucket(class.Spec.CustomEngineConfig), nil
}

// customConfigHasBucket reports whether a customEngineConfig payload sets a
// non-empty storage.managed_table_bucket_name — the field the engine needs for
// managed object storage, written the same way by --bucket and by a class.
func customConfigHasBucket(raw *apiextensionsv1.JSON) bool {
	if raw == nil || len(raw.Raw) == 0 {
		return false
	}
	var cfg struct {
		Storage struct {
			ManagedTableBucketName string `json:"managed_table_bucket_name"`
		} `json:"storage"`
	}
	if err := json.Unmarshal(raw.Raw, &cfg); err != nil {
		return false
	}
	return cfg.Storage.ManagedTableBucketName != ""
}

// readyFromConditions reports the Ready condition's truth, or nil if absent.
func readyFromConditions(conds []metav1.Condition) *bool {
	cond := meta.FindStatusCondition(conds, "Ready")
	if cond == nil {
		return nil
	}
	v := cond.Status == metav1.ConditionTrue
	return &v
}
