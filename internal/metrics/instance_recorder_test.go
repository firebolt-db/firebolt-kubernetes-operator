package metrics

import (
	"testing"

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
				{Type: computev1alpha1.InstanceConditionPostgresReady, Status: metav1.ConditionTrue},
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
