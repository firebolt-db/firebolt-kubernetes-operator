/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// Pins the operator chart's manager-rbac.yaml and apiserver-proxy-rbac.yaml
// templates to the four-cell matrix of (watchNamespaces, rbac.apiserverProxyGrant)
// combinations. The toggle is the load-bearing piece of the namespace-scoped
// install posture (FB-1494): if the chart silently drops the namespaced
// `Role`/`RoleBinding` shape, or accidentally renders both ClusterRole and
// per-NS Role at the same time, the operator's RBAC stops matching its cache
// scope and reconciles 403 in production. The cell-by-cell assertions below
// hold a line against that regression without spinning up a kind cluster.

type manifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

func renderChart(t *testing.T, extraArgs ...string) []manifest {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	chartDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "helm", "firebolt-operator")
	args := append([]string{"template", "firebolt-operator", chartDir}, extraArgs...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template %v: %v\nstderr: %s", extraArgs, err, stderr.String())
	}
	out := make([]manifest, 0, 16)
	for _, doc := range strings.Split(stdout.String(), "\n---\n") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var m manifest
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("parse rendered doc: %v\n%s", err, doc)
		}
		if m.Kind == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func count(ms []manifest, kind, namePrefix string, namespaces ...string) []manifest {
	wantNS := make(map[string]struct{}, len(namespaces))
	for _, ns := range namespaces {
		wantNS[ns] = struct{}{}
	}
	out := make([]manifest, 0)
	for _, m := range ms {
		if m.Kind != kind {
			continue
		}
		if !strings.HasPrefix(m.Metadata.Name, namePrefix) {
			continue
		}
		if len(wantNS) > 0 {
			if _, ok := wantNS[m.Metadata.Namespace]; !ok {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

func TestChartRBACToggle_ClusterWideDefault(t *testing.T) {
	helmAvailable(t)
	ms := renderChart(t)

	manager := count(ms, "ClusterRole", "firebolt-operator-manager")
	if len(manager) != 1 {
		t.Errorf("ClusterRole firebolt-operator-manager: want 1, got %d", len(manager))
	}
	managerBind := count(ms, "ClusterRoleBinding", "firebolt-operator-manager")
	if len(managerBind) != 1 {
		t.Errorf("ClusterRoleBinding firebolt-operator-manager: want 1, got %d", len(managerBind))
	}
	if got := count(ms, "Role", "firebolt-operator-manager"); len(got) != 0 {
		t.Errorf("cluster-wide mode must not render namespaced Role for manager; got %d", len(got))
	}
	if got := count(ms, "RoleBinding", "firebolt-operator-manager"); len(got) != 0 {
		t.Errorf("cluster-wide mode must not render namespaced RoleBinding for manager; got %d", len(got))
	}
	if got := count(ms, "ClusterRole", "firebolt-operator-apiserver-proxy"); len(got) != 0 {
		t.Errorf("rbac.apiserverProxyGrant off must not render apiserver-proxy ClusterRole; got %d", len(got))
	}
}

func TestChartRBACToggle_Namespaced(t *testing.T) {
	helmAvailable(t)
	ms := renderChart(t, "--set", "watchNamespaces={tenant-a,tenant-b}")

	if got := count(ms, "ClusterRole", "firebolt-operator-manager"); len(got) != 0 {
		t.Errorf("namespaced mode must not render manager ClusterRole; got %d", len(got))
	}
	if got := count(ms, "ClusterRoleBinding", "firebolt-operator-manager"); len(got) != 0 {
		t.Errorf("namespaced mode must not render manager ClusterRoleBinding; got %d", len(got))
	}
	roles := count(ms, "Role", "firebolt-operator-manager", "tenant-a", "tenant-b")
	if len(roles) != 2 {
		t.Errorf("Role firebolt-operator-manager across tenant-a,tenant-b: want 2, got %d", len(roles))
	}
	bindings := count(ms, "RoleBinding", "firebolt-operator-manager", "tenant-a", "tenant-b")
	if len(bindings) != 2 {
		t.Errorf("RoleBinding firebolt-operator-manager across tenant-a,tenant-b: want 2, got %d", len(bindings))
	}
}

func TestChartRBACToggle_ApiserverProxyClusterWide(t *testing.T) {
	helmAvailable(t)
	ms := renderChart(t, "--set", "rbac.apiserverProxyGrant=true")

	if got := count(ms, "ClusterRole", "firebolt-operator-apiserver-proxy"); len(got) != 1 {
		t.Errorf("apiserver-proxy ClusterRole: want 1, got %d", len(got))
	}
	if got := count(ms, "ClusterRoleBinding", "firebolt-operator-apiserver-proxy"); len(got) != 1 {
		t.Errorf("apiserver-proxy ClusterRoleBinding: want 1, got %d", len(got))
	}
	if got := count(ms, "Role", "firebolt-operator-apiserver-proxy"); len(got) != 0 {
		t.Errorf("apiserver-proxy in cluster-wide mode must not render namespaced Role; got %d", len(got))
	}
}

func TestChartRBACToggle_ApiserverProxyNamespaced(t *testing.T) {
	helmAvailable(t)
	ms := renderChart(t,
		"--set", "rbac.apiserverProxyGrant=true",
		"--set", "watchNamespaces={tenant-a,tenant-b}",
	)

	if got := count(ms, "ClusterRole", "firebolt-operator-apiserver-proxy"); len(got) != 0 {
		t.Errorf("apiserver-proxy in namespaced mode must not render ClusterRole; got %d", len(got))
	}
	roles := count(ms, "Role", "firebolt-operator-apiserver-proxy", "tenant-a", "tenant-b")
	if len(roles) != 2 {
		t.Errorf("apiserver-proxy Role across tenant-a,tenant-b: want 2, got %d", len(roles))
	}
	bindings := count(ms, "RoleBinding", "firebolt-operator-apiserver-proxy", "tenant-a", "tenant-b")
	if len(bindings) != 2 {
		t.Errorf("apiserver-proxy RoleBinding across tenant-a,tenant-b: want 2, got %d", len(bindings))
	}
}

func helmAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart-render assertion")
	}
}
