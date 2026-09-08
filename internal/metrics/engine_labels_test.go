package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Every engine family names the parent instance firebolt_io_instance. The
// recorders pass label values positionally, so a rename of the label itself
// is invisible to them and to their tests; this test reads what a scraper
// would read. `instance` is Prometheus's target label, and a scraper that
// does not honor exposed labels overwrites it with the target address, which
// is how the parent instance went missing from every engine series once.
func TestEngineFamiliesExposeParentInstanceAsFireboltIoInstance(t *testing.T) {
	engines := map[string]prometheus.Collector{
		"firebolt_engine_status_phase":              EnginePhase,
		"firebolt_engine_status_condition":          EngineCondition,
		"firebolt_engine_spec_replicas":             EngineSpecReplicas,
		"firebolt_engine_active_generation":         EngineActiveGeneration,
		"firebolt_engine_pods_ready":                EnginePodsReady,
		"firebolt_engine_pods_total":                EnginePodsTotal,
		"firebolt_engine_draining_generation":       EngineDrainingGeneration,
		"firebolt_engine_last_reconciled_timestamp": EngineLastReconciled,
		"firebolt_engine_drain_check_errors_total":  EngineDrainCheckErrors,
	}

	// One series per family, so Gather has something to describe.
	EnginePhase.WithLabelValues("ns", "eng", "inst", "stable").Set(1)
	EngineCondition.WithLabelValues("ns", "eng", "inst", "Ready").Set(1)
	EngineSpecReplicas.WithLabelValues("ns", "eng", "inst").Set(1)
	EngineActiveGeneration.WithLabelValues("ns", "eng", "inst").Set(1)
	EnginePodsReady.WithLabelValues("ns", "eng", "inst").Set(1)
	EnginePodsTotal.WithLabelValues("ns", "eng", "inst").Set(1)
	EngineDrainingGeneration.WithLabelValues("ns", "eng", "inst").Set(-1)
	EngineLastReconciled.WithLabelValues("ns", "eng", "inst").SetToCurrentTime()
	EngineDrainCheckErrors.WithLabelValues("ns", "eng", "inst").Inc()

	reg := prometheus.NewPedanticRegistry()
	for _, c := range engines {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	seen := map[string]bool{}
	for _, fam := range families {
		if _, ours := engines[fam.GetName()]; !ours {
			continue
		}
		seen[fam.GetName()] = true
		for _, m := range fam.GetMetric() {
			names := labelNames(m)
			if !names["firebolt_io_instance"] {
				t.Errorf("%s: exported labels %v lack firebolt_io_instance", fam.GetName(), keys(names))
			}
			if names["instance"] {
				t.Errorf("%s: exports a label named instance, which a scraper overwrites with the target address", fam.GetName())
			}
			if !names["namespace"] || !names["name"] {
				t.Errorf("%s: exported labels %v lack namespace or name", fam.GetName(), keys(names))
			}
		}
	}
	for name := range engines {
		if !seen[name] {
			t.Errorf("%s: not gathered", name)
		}
	}
}

func labelNames(m *dto.Metric) map[string]bool {
	out := map[string]bool{}
	for _, lp := range m.GetLabel() {
		out[lp.GetName()] = true
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
