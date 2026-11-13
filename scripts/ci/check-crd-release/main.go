// Command check-crd-release rejects CRD chart version bumps without substantive content changes.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const chartPath = "helm/firebolt-operator-crds"
const tagPrefix = "firebolt-operator-crds-chart-"

func main() {
	base := flag.String("base", "", "PR base commit (required)")
	candidate := flag.String("candidate", "HEAD", "candidate commit to validate")
	flag.Parse()
	if err := check(*base, *candidate); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // Fixed git/helm call sites, no shell.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return out, nil
}

func revision(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("a base commit is required")
	}
	out, err := run("git", "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	return strings.TrimSpace(string(out)), err
}

func chartVersion(ref string) (string, error) {
	data, err := run("git", "show", ref+":"+chartPath+"/Chart.yaml")
	if err != nil {
		return "", err
	}
	var chart struct {
		Version string `json:"version"`
	}
	if err := yaml.Unmarshal(data, &chart); err != nil {
		return "", err
	}
	if chart.Version == "" {
		return "", errors.New("missing Chart.yaml version")
	}
	return chart.Version, nil
}

func check(base, candidate string) error {
	base, err := revision(base)
	if err != nil {
		return err
	}
	candidate, err = revision(candidate)
	if err != nil {
		return err
	}
	previousVersion, err := chartVersion(base)
	if err != nil {
		return err
	}
	version, err := chartVersion(candidate)
	if err != nil {
		return err
	}
	if version == previousVersion {
		fmt.Println("CRD chart version unchanged; no release validation needed")
		return nil
	}
	tag, err := run("git", "describe", "--tags", "--first-parent", "--abbrev=0", "--match", tagPrefix+"*", base)
	if err != nil {
		return err
	}
	baseline := strings.TrimSpace(string(tag))
	before, err := packagedChart(baseline)
	if err != nil {
		return err
	}
	after, err := packagedChart(candidate)
	if err != nil {
		return err
	}
	if maps.Equal(before, after) {
		return fmt.Errorf("CRD chart %s has no substantive change from %s; leave the release PR open for further changes", version, baseline)
	}
	fmt.Printf("CRD chart %s differs from %s\n", version, baseline)
	return nil
}

func packagedChart(ref string) (result map[string]string, err error) {
	dir, err := os.MkdirTemp("", "crd-release-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	archive, err := run("git", "archive", ref, chartPath)
	if err != nil {
		return nil, err
	}
	if err := extractChart(dir, archive); err != nil {
		return nil, err
	}
	if _, err := run("helm", "package", filepath.Join(dir, chartPath), "--destination", dir); err != nil {
		return nil, err
	}
	packages, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	if err != nil {
		return nil, err
	}
	if len(packages) != 1 {
		return nil, errors.New("expected one packaged CRD chart")
	}
	data, err := os.ReadFile(packages[0])
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	result = make(map[string]string)
	contents := tar.NewReader(reader)
	for {
		header, err := contents.Next()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("unsupported packaged file: %s", header.Name)
		}
		_, name, ok := strings.Cut(header.Name, "/")
		if !ok {
			return nil, fmt.Errorf("invalid chart package path: %s", header.Name)
		}
		if name == "README.md" || name == "CHANGELOG.md" {
			continue
		}
		data, err := io.ReadAll(contents)
		if err != nil {
			return nil, err
		}
		value, err := normalizeFile(name, data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		result[name] = string(value)
	}
}

func extractChart(dir string, archive []byte) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			// git archive records the commit ID in a global PAX header.
			continue
		case tar.TypeDir:
			if err := root.MkdirAll(header.Name, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			data, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			if err := root.WriteFile(header.Name, data, 0o600); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported chart entry: %s", header.Name)
		}
	}
}

func normalizeFile(name string, data []byte) ([]byte, error) {
	if (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) || bytes.Contains(data, []byte("{{")) {
		return data, nil
	}
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var documents []any
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		encoded, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		if object, ok := value.(map[string]any); ok {
			if name == "Chart.yaml" {
				delete(object, "version")
				delete(object, "description")
			} else if object["kind"] == "CustomResourceDefinition" {
				if err := normalizeCRD(object); err != nil {
					return nil, err
				}
			}
		}
		documents = append(documents, value)
	}
	return json.Marshal(documents)
}

func normalizeCRD(resource map[string]any) error {
	spec, ok := resource["spec"].(map[string]any)
	if !ok {
		return errors.New("CRD spec must be an object")
	}
	versions, ok := spec["versions"].([]any)
	if !ok {
		return errors.New("CRD spec.versions must be an array")
	}
	for _, value := range versions {
		version, ok := value.(map[string]any)
		if !ok {
			return errors.New("CRD version must be an object")
		}
		schema, ok := version["schema"].(map[string]any)
		if !ok {
			return errors.New("CRD version.schema must be an object")
		}
		root, ok := schema["openAPIV3Schema"].(map[string]any)
		if !ok {
			return errors.New("CRD version.schema.openAPIV3Schema must be an object")
		}
		stripDescriptions(root)
	}
	return nil
}

// Strip only schema annotations, preserving property names and literal defaults/examples.
func stripDescriptions(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	delete(schema, "description")
	for _, key := range []string{"properties", "patternProperties", "definitions", "dependencies"} {
		if children, ok := schema[key].(map[string]any); ok {
			for _, child := range children {
				stripDescriptions(child)
			}
		}
	}
	for _, key := range []string{"items", "additionalItems", "additionalProperties", "not", "allOf", "anyOf", "oneOf"} {
		if children, ok := schema[key].([]any); ok {
			for _, child := range children {
				stripDescriptions(child)
			}
		} else {
			stripDescriptions(schema[key])
		}
	}
}
