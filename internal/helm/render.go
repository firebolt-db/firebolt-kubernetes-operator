package helm

import (
	"fmt"
	"io/fs"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

// RenderChart renders a Helm chart from an embedded filesystem and returns a map
// of template name to rendered YAML content.
func RenderChart(chartFS fs.FS, chartPath string, releaseName, namespace string, values map[string]interface{}) (map[string]string, error) {
	ch, err := loadChartFromFS(chartFS, chartPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	releaseOpts := chartutil.ReleaseOptions{
		Name:      releaseName,
		Namespace: namespace,
		IsInstall: true,
	}

	valuesToRender, err := chartutil.ToRenderValues(ch, values, releaseOpts, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to compose values: %w", err)
	}

	rendered, err := engine.Render(ch, valuesToRender)
	if err != nil {
		return nil, fmt.Errorf("failed to render chart: %w", err)
	}

	result := make(map[string]string)
	for name, content := range rendered {
		trimmed := strings.TrimSpace(content)
		if trimmed == "" {
			continue
		}
		result[name] = trimmed
	}

	return result, nil
}

func loadChartFromFS(chartFS fs.FS, chartPath string) (*chart.Chart, error) {
	var files []*loader.BufferedFile

	err := fs.WalkDir(chartFS, chartPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		data, err := fs.ReadFile(chartFS, path)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", path, err)
		}

		relPath := strings.TrimPrefix(path, chartPath+"/")
		files = append(files, &loader.BufferedFile{
			Name: relPath,
			Data: data,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk chart directory: %w", err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no files found in chart path %q", chartPath)
	}

	return loader.LoadFiles(files)
}
