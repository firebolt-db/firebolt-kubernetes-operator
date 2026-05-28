/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fireboltEngineWithRef returns a minimal valid FireboltEngine with the
// given engineClassRef (nil for no reference).
func fireboltEngineWithRef(ref *string) *FireboltEngine {
	return &FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: FireboltEngineSpec{
			InstanceRef:    "inst",
			Replicas:       1,
			EngineClassRef: ref,
		},
	}
}

// fakeReaderWithClasses builds a controller-runtime fake client preloaded
// with the named EngineClasses in the test engine's namespace ("default").
// EngineClass is namespaced, so the validator's Get includes the engine's
// namespace; the fixtures live in the same namespace to satisfy that
// lookup.
func fakeReaderWithClasses(t *testing.T, names ...string) client.Reader {
	t.Helper()
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatalf("scheme.AddToScheme: %v", err)
	}
	if err := AddToScheme(sch); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	objs := make([]client.Object, 0, len(names))
	for _, name := range names {
		objs = append(objs, &EngineClass{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func TestFireboltEngineValidator_NilRefIsAllowed(t *testing.T) {
	v := &FireboltEngineCustomValidator{Reader: fakeReaderWithClasses(t)}
	if _, err := v.ValidateCreate(context.Background(), fireboltEngineWithRef(nil)); err != nil {
		t.Fatalf("ValidateCreate: nil ref should pass, got %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), fireboltEngineWithRef(nil), fireboltEngineWithRef(nil)); err != nil {
		t.Fatalf("ValidateUpdate: nil ref should pass, got %v", err)
	}
}

func TestFireboltEngineValidator_ExistingClassIsAllowed(t *testing.T) {
	v := &FireboltEngineCustomValidator{Reader: fakeReaderWithClasses(t, "compute-optimized")}
	eng := fireboltEngineWithRef(ptr.To("compute-optimized"))
	if _, err := v.ValidateCreate(context.Background(), eng); err != nil {
		t.Fatalf("ValidateCreate: existing class should pass, got %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), eng, eng); err != nil {
		t.Fatalf("ValidateUpdate: existing class should pass, got %v", err)
	}
}

func TestFireboltEngineValidator_MissingClassIsRejected(t *testing.T) {
	v := &FireboltEngineCustomValidator{Reader: fakeReaderWithClasses(t)}
	eng := fireboltEngineWithRef(ptr.To("does-not-exist"))

	_, err := v.ValidateCreate(context.Background(), eng)
	if err == nil {
		t.Fatal("ValidateCreate: missing class should be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "engineClassRef") || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("ValidateCreate: error %q does not surface field path and missing name", err.Error())
	}

	_, err = v.ValidateUpdate(context.Background(), eng, eng)
	if err == nil {
		t.Fatal("ValidateUpdate: missing class should be rejected, got nil")
	}
}

// TestFireboltEngineValidator_ClassInDifferentNamespaceIsRejected pins
// down the namespace-coupled lookup: an EngineClass with the right name
// existing in a different namespace must NOT satisfy
// spec.engineClassRef. Kubernetes resolves the reference in the
// engine's own namespace at reconcile / pod-admission time; the webhook
// must agree.
func TestFireboltEngineValidator_ClassInDifferentNamespaceIsRejected(t *testing.T) {
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatalf("scheme.AddToScheme: %v", err)
	}
	if err := AddToScheme(sch); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	// Class lives in "other-ns"; the engine is in "default".
	reader := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		&EngineClass{ObjectMeta: metav1.ObjectMeta{Name: "compute-optimized", Namespace: "other-ns"}},
	).Build()
	v := &FireboltEngineCustomValidator{Reader: reader}
	eng := fireboltEngineWithRef(ptr.To("compute-optimized"))
	_, err := v.ValidateCreate(context.Background(), eng)
	if err == nil {
		t.Fatal("ValidateCreate: class in different namespace should be rejected (Kubernetes resolves engineClassRef in the engine's namespace)")
	}
	if !strings.Contains(err.Error(), "engineClassRef") || !strings.Contains(err.Error(), "compute-optimized") {
		t.Errorf("ValidateCreate: error %q does not surface field path and missing name", err.Error())
	}
}

func TestFireboltEngineValidator_DeleteIsNoOp(t *testing.T) {
	v := &FireboltEngineCustomValidator{Reader: fakeReaderWithClasses(t)}
	if _, err := v.ValidateDelete(context.Background(), fireboltEngineWithRef(ptr.To("any"))); err != nil {
		t.Fatalf("ValidateDelete: expected no-op, got %v", err)
	}
}

// engineWithResources returns a minimal valid FireboltEngine with the
// supplied resources block. nil ref keeps the EngineClass lookup out of
// the way so the resource-bound assertions are not entangled with
// reader fixtures.
func engineWithResources(req, lim corev1.ResourceList) *FireboltEngine {
	eng := fireboltEngineWithRef(nil)
	eng.Spec.Resources = corev1.ResourceRequirements{Requests: req, Limits: lim}
	return eng
}

func TestFireboltEngineValidator_ResourcesWithinBounds(t *testing.T) {
	v := &FireboltEngineCustomValidator{
		Reader: fakeReaderWithClasses(t),
		ResourceBounds: EngineResourceBounds{
			MaxCPU:    resource.MustParse("32"),
			MaxMemory: resource.MustParse("256Gi"),
		},
	}
	eng := engineWithResources(
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
		},
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("32"),
			corev1.ResourceMemory: resource.MustParse("256Gi"),
		},
	)
	if _, err := v.ValidateCreate(context.Background(), eng); err != nil {
		t.Fatalf("ValidateCreate: within bounds (including equality on max) should pass, got %v", err)
	}
}

func TestFireboltEngineValidator_ResourcesExceedCPULimit(t *testing.T) {
	v := &FireboltEngineCustomValidator{
		Reader: fakeReaderWithClasses(t),
		ResourceBounds: EngineResourceBounds{
			MaxCPU: resource.MustParse("32"),
		},
	}
	eng := engineWithResources(
		nil,
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64")},
	)
	_, err := v.ValidateCreate(context.Background(), eng)
	if err == nil {
		t.Fatal("ValidateCreate: cpu limit above bound should be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "spec.resources.limits") || !strings.Contains(err.Error(), "cpu") {
		t.Errorf("ValidateCreate: error %q does not surface limits.cpu path", err.Error())
	}
	if !strings.Contains(err.Error(), "32") {
		t.Errorf("ValidateCreate: error %q does not surface the configured maximum", err.Error())
	}
}

func TestFireboltEngineValidator_ResourcesExceedMemoryRequest(t *testing.T) {
	v := &FireboltEngineCustomValidator{
		Reader: fakeReaderWithClasses(t),
		ResourceBounds: EngineResourceBounds{
			MaxMemory: resource.MustParse("256Gi"),
		},
	}
	eng := engineWithResources(
		corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Gi")},
		nil,
	)
	_, err := v.ValidateCreate(context.Background(), eng)
	if err == nil {
		t.Fatal("ValidateCreate: memory request above bound should be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "spec.resources.requests") || !strings.Contains(err.Error(), "memory") {
		t.Errorf("ValidateCreate: error %q does not surface requests.memory path", err.Error())
	}
}

// TestFireboltEngineValidator_ResourcesUnboundedDimensionPasses keeps
// the operator from gatekeeping ResourceNames it has no bound for
// (extended resources, GPU vendors, etc.). The MaxMemory bound exists
// but the engine declares only cpu, so the request must be admitted.
func TestFireboltEngineValidator_ResourcesUnboundedDimensionPasses(t *testing.T) {
	v := &FireboltEngineCustomValidator{
		Reader: fakeReaderWithClasses(t),
		ResourceBounds: EngineResourceBounds{
			MaxMemory: resource.MustParse("256Gi"),
		},
	}
	eng := engineWithResources(
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("128")},
		nil,
	)
	if _, err := v.ValidateCreate(context.Background(), eng); err != nil {
		t.Fatalf("ValidateCreate: unbounded dimension should pass, got %v", err)
	}
}

// TestFireboltEngineValidator_ResourcesEmptyBoundsIsNoOp confirms that
// the default (empty) bound matrix admits any spec.resources value —
// platform teams must opt into bounds explicitly.
func TestFireboltEngineValidator_ResourcesEmptyBoundsIsNoOp(t *testing.T) {
	v := &FireboltEngineCustomValidator{Reader: fakeReaderWithClasses(t)}
	eng := engineWithResources(
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("999"),
			corev1.ResourceMemory: resource.MustParse("999Ti"),
		},
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("999"),
			corev1.ResourceMemory: resource.MustParse("999Ti"),
		},
	)
	if _, err := v.ValidateCreate(context.Background(), eng); err != nil {
		t.Fatalf("ValidateCreate: empty bounds should be a no-op, got %v", err)
	}
}

// TestFireboltEngineValidator_AggregatesClassAndResourceErrors makes
// sure a single admission round-trip reports both a missing class and a
// resources violation. Users with a fat-fingered apply should see every
// blocker at once instead of fixing them one-by-one.
func TestFireboltEngineValidator_AggregatesClassAndResourceErrors(t *testing.T) {
	v := &FireboltEngineCustomValidator{
		Reader: fakeReaderWithClasses(t),
		ResourceBounds: EngineResourceBounds{
			MaxCPU: resource.MustParse("8"),
		},
	}
	eng := engineWithResources(nil,
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("16")},
	)
	eng.Spec.EngineClassRef = ptr.To("missing")
	_, err := v.ValidateCreate(context.Background(), eng)
	if err == nil {
		t.Fatal("ValidateCreate: expected aggregated error, got nil")
	}
	if !strings.Contains(err.Error(), "engineClassRef") || !strings.Contains(err.Error(), "spec.resources.limits") {
		t.Errorf("ValidateCreate: error %q should report both engineClassRef and resources violations", err.Error())
	}
}

func TestFireboltEngineValidator_ResourceBoundsIsEmpty(t *testing.T) {
	empty := EngineResourceBounds{}
	if !empty.IsEmpty() {
		t.Fatal("zero-value EngineResourceBounds should report IsEmpty()=true")
	}
	withCPU := EngineResourceBounds{MaxCPU: resource.MustParse("1")}
	if withCPU.IsEmpty() {
		t.Fatal("any non-zero bound should make IsEmpty() false")
	}
}

// fakeFailingReader returns InternalError for every Get. Used to exercise
// the non-NotFound error path: when the API server is unreachable or RBAC
// hides the class, the webhook reports an internal-error field condition
// rather than masquerading as NotFound.
type fakeFailingReader struct{}

func (fakeFailingReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewServiceUnavailable("simulated apiserver outage")
}
func (fakeFailingReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return apierrors.NewServiceUnavailable("simulated apiserver outage")
}

// compile-time guard
var _ client.Reader = fakeFailingReader{}
var _ schema.ObjectKind = (*metav1.PartialObjectMetadata)(nil) // ensures import is wired even if other tests change

func TestFireboltEngineValidator_NonNotFoundSurfacesAsInternal(t *testing.T) {
	v := &FireboltEngineCustomValidator{Reader: fakeFailingReader{}}
	eng := fireboltEngineWithRef(ptr.To("any"))
	_, err := v.ValidateCreate(context.Background(), eng)
	if err == nil {
		t.Fatal("ValidateCreate: apiserver outage should bubble up as an admission error, got nil")
	}
	if !strings.Contains(err.Error(), "engineClassRef") {
		t.Errorf("ValidateCreate: error %q does not preserve field path", err.Error())
	}
}
