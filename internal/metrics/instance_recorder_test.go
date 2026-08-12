package metrics

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func TestInstanceRecorderRecord(t *testing.T) {
	rec := NewInstanceRecorder()
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-1", Namespace: "ns"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID:       "01JTEST",
			Metadata: computev1alpha1.MetadataSpec{},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			Phase: computev1alpha1.InstancePhaseReady,
			Conditions: []metav1.Condition{
				{Type: computev1alpha1.InstanceConditionReady, Status: metav1.ConditionTrue},
				{Type: computev1alpha1.InstanceConditionMetadataReady, Status: metav1.ConditionTrue},
				{Type: computev1alpha1.InstanceConditionGatewayReady, Status: metav1.ConditionFalse},
			},
		},
	}

	rec.Record(instance)

	// Phase StateSet
	if v := gaugeValue(InstancePhase.WithLabelValues("ns", "inst-1", "Ready")); v != 1 {
		t.Errorf("Ready phase = %v, want 1", v)
	}
	if v := gaugeValue(InstancePhase.WithLabelValues("ns", "inst-1", "Provisioning")); v != 0 {
		t.Errorf("Provisioning phase = %v, want 0", v)
	}

	// Conditions
	if v := gaugeValue(InstanceCondition.WithLabelValues("ns", "inst-1", "Ready")); v != 1 {
		t.Errorf("Ready condition = %v, want 1", v)
	}
	if v := gaugeValue(InstanceCondition.WithLabelValues("ns", "inst-1", "GatewayReady")); v != 0 {
		t.Errorf("GatewayReady condition = %v, want 0", v)
	}

	// Info gauge
	if v := gaugeValue(InstanceInfo.WithLabelValues("ns", "inst-1", "01JTEST", "internal")); v != 1 {
		t.Errorf("info = %v, want 1", v)
	}
}

func TestInstanceRecorderExternalPostgres(t *testing.T) {
	rec := NewInstanceRecorder()
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-ext", Namespace: "ns"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID: "01JEXT",
			Metadata: computev1alpha1.MetadataSpec{
				Postgres: &computev1alpha1.PostgresSpec{
					Host:     "pg.example.com",
					Database: "meta",
				},
			},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			Phase: computev1alpha1.InstancePhaseProvisioning,
		},
	}

	rec.Record(instance)

	if v := gaugeValue(InstanceInfo.WithLabelValues("ns", "inst-ext", "01JEXT", "external")); v != 1 {
		t.Errorf("info postgres_mode = external, got gauge value %v", v)
	}
}

func TestInstanceRecorderDelete(t *testing.T) {
	rec := NewInstanceRecorder()
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-del", Namespace: "ns"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID:       "01JDEL",
			Metadata: computev1alpha1.MetadataSpec{},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			Phase: computev1alpha1.InstancePhaseReady,
		},
	}

	rec.Record(instance)
	rec.Delete("ns", "inst-del")

	for _, phase := range InstancePhases {
		v := gaugeValue(InstancePhase.WithLabelValues("ns", "inst-del", phase))
		if v != 0 {
			t.Errorf("post-delete: phase %s = %v, want 0", phase, v)
		}
	}
}

// TestInstanceRecorderSigningKeyRotation covers the rotation-stall series. The
// pending-seconds gauge is the one an alert fires on, since rotation gates park
// indefinitely by design and only duration separates a normal engine roll from a
// wedged fleet.
func TestInstanceRecorderSigningKeyRotation(t *testing.T) {
	rec := NewInstanceRecorder()
	stuckSince := metav1.NewTime(time.Now().Add(-90 * time.Minute))
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-rot", Namespace: "ns"},
		Status: computev1alpha1.FireboltInstanceStatus{
			Auth: &computev1alpha1.AuthStatus{
				SigningKeyGeneration: 4,
				SigningKeys: []computev1alpha1.SigningKeyStatus{
					{ID: "signing-4", Phase: computev1alpha1.SigningKeyActive},
					{ID: "signing-3", Phase: computev1alpha1.SigningKeyValidationOnly},
				},
				PendingRotationStep: computev1alpha1.RotationStepAwaitingRetireAnchor,
				PendingSince:        &stuckSince,
				LaggingEngines:      []string{"e1", "e2"},
				LaggingEngineCount:  7,
			},
		},
	}

	rec.Record(instance)

	if v := gaugeValue(InstanceSigningKeyGeneration.WithLabelValues("ns", "inst-rot")); v != 4 {
		t.Errorf("generation = %v, want 4", v)
	}
	if v := gaugeValue(InstanceSigningKeys.WithLabelValues("ns", "inst-rot", "Active")); v != 1 {
		t.Errorf("Active keys = %v, want 1", v)
	}
	if v := gaugeValue(InstanceSigningKeys.WithLabelValues("ns", "inst-rot", "ValidationOnly")); v != 1 {
		t.Errorf("ValidationOnly keys = %v, want 1", v)
	}
	// Written as an explicit zero, so a phase emptying out reads as 0 rather than
	// leaving its last non-zero value to look current.
	if v := gaugeValue(InstanceSigningKeys.WithLabelValues("ns", "inst-rot", "Removing")); v != 0 {
		t.Errorf("Removing keys = %v, want 0", v)
	}
	if v := gaugeValue(InstanceRotationPendingStep.WithLabelValues("ns", "inst-rot", "AwaitingRetireAnchor")); v != 1 {
		t.Errorf("AwaitingRetireAnchor = %v, want 1", v)
	}
	if v := gaugeValue(InstanceRotationPendingStep.WithLabelValues("ns", "inst-rot", "AwaitingPromotion")); v != 0 {
		t.Errorf("AwaitingPromotion = %v, want 0", v)
	}
	if v := gaugeValue(InstanceRotationPendingSeconds.WithLabelValues("ns", "inst-rot")); v < 5000 {
		t.Errorf("pending seconds = %v, want roughly 5400 (90 minutes)", v)
	}
	// The true total, not the truncated name list in status.
	if v := gaugeValue(InstanceRotationLaggingEngines.WithLabelValues("ns", "inst-rot")); v != 7 {
		t.Errorf("lagging engines = %v, want 7", v)
	}

	t.Run("no rotation pending reports zero, not stale", func(t *testing.T) {
		idle := instance.DeepCopy()
		idle.Status.Auth.PendingRotationStep = ""
		idle.Status.Auth.PendingSince = nil
		idle.Status.Auth.LaggingEngineCount = 0
		rec.Record(idle)

		if v := gaugeValue(InstanceRotationPendingSeconds.WithLabelValues("ns", "inst-rot")); v != 0 {
			t.Errorf("pending seconds = %v, want 0 once nothing is pending", v)
		}
		if v := gaugeValue(InstanceRotationPendingStep.WithLabelValues("ns", "inst-rot", "AwaitingRetireAnchor")); v != 0 {
			t.Errorf("AwaitingRetireAnchor = %v, want 0 once the step advanced", v)
		}
		if v := gaugeValue(InstanceRotationLaggingEngines.WithLabelValues("ns", "inst-rot")); v != 0 {
			t.Errorf("lagging engines = %v, want 0", v)
		}
	})

	t.Run("instance without auth reports zeros", func(t *testing.T) {
		rec.Record(&computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "inst-noauth", Namespace: "ns"},
		})
		if v := gaugeValue(InstanceSigningKeyGeneration.WithLabelValues("ns", "inst-noauth")); v != 0 {
			t.Errorf("generation = %v, want 0", v)
		}
	})

	t.Run("delete clears the rotation series", func(t *testing.T) {
		rec.Delete("ns", "inst-rot")
		if v := gaugeValue(InstanceSigningKeyGeneration.WithLabelValues("ns", "inst-rot")); v != 0 {
			t.Errorf("generation after Delete = %v, want a fresh 0", v)
		}
	})
}

// TestInstanceConditionTypesCoversAuthAndTLS pins that every condition the
// instance controller writes is exported as a metric label. A condition missing
// here is invisible on the /metrics endpoint.
func TestInstanceConditionTypesCoversAuthAndTLS(t *testing.T) {
	for _, want := range []string{
		computev1alpha1.InstanceConditionAuthReady,
		computev1alpha1.InstanceConditionEngineTLSReady,
		computev1alpha1.InstanceConditionGatewayTLSReady,
	} {
		if !slices.Contains(InstanceConditionTypes, want) {
			t.Errorf("condition %q is not exported as a firebolt_instance_status_condition label", want)
		}
	}
}
