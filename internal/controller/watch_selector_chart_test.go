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
)

// Pins the operator chart's watchLabelSelector wiring in deployment.yaml:
// a non-empty value must surface as a --watch-label-selector argument on
// the manager container and the default must render no such argument. A
// cluster-wide install relies on this argument to ignore CRs
// owned by namespace-scoped installs; if the chart silently drops it, both
// installs reconcile the same FireboltEngines again and the StatefulSet
// flips between their two renders.

func renderChartRaw(t *testing.T, extraArgs ...string) string {
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
	return stdout.String()
}

func TestChartWatchLabelSelector_DefaultRendersNoFlag(t *testing.T) {
	helmAvailable(t)
	out := renderChartRaw(t)
	if strings.Contains(out, "--watch-label-selector") {
		t.Error("default render must not pass --watch-label-selector")
	}
}

func TestChartWatchLabelSelector_RendersFlag(t *testing.T) {
	helmAvailable(t)
	out := renderChartRaw(t, "--set-string", "watchLabelSelector=!example.com/managed")
	if !strings.Contains(out, "- --watch-label-selector=!example.com/managed") {
		t.Error("watchLabelSelector value must surface as a --watch-label-selector manager argument")
	}
}

func TestChartWatchLabelSelector_ComposesWithWatchNamespaces(t *testing.T) {
	helmAvailable(t)
	out := renderChartRaw(t,
		"--set-string", "watchLabelSelector=!example.com/managed",
		"--set", "watchNamespaces={tenant-a,tenant-b}",
	)
	if !strings.Contains(out, "- --namespaces=tenant-a,tenant-b") {
		t.Error("watchNamespaces must still surface as a --namespaces manager argument")
	}
	if !strings.Contains(out, "- --watch-label-selector=!example.com/managed") {
		t.Error("watchLabelSelector must surface alongside --namespaces")
	}
}
