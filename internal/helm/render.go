/*
Copyright 2025.

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

package helm

import (
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
)

// RenderChart templates a Helm chart with the given values, returning the
// rendered multi-document YAML string. No cluster connection is required;
// rendering is performed entirely client-side.
func RenderChart(ch *chart.Chart, releaseName, namespace string, values map[string]interface{}) (string, error) {
	cfg := &action.Configuration{}
	install := action.NewInstall(cfg)
	install.ReleaseName = releaseName
	install.Namespace = namespace
	install.DryRun = true
	install.ClientOnly = true
	install.Replace = true

	if err := chartutil.ProcessDependencies(ch, values); err != nil {
		return "", fmt.Errorf("processing chart dependencies: %w", err)
	}

	rel, err := install.Run(ch, values)
	if err != nil {
		return "", fmt.Errorf("rendering chart %s: %w", ch.Name(), err)
	}

	return rel.Manifest, nil
}
