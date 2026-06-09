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

// Package images embeds one of the variant-specific defaults files
// (defaults.latest.env by default, defaults.dev.env when built with
// `-tags=dev`) and exposes the image references used by both the operator
// controllers and the E2E tests. The two variants must be consistent across
// the operator binary, the gateway pod template it builds, and the E2E
// suite — selecting them at compile time via a build tag is what enforces
// that consistency.
package images

import (
	"strings"
)

var defaults = parse(raw)

func parse(data string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// Get returns the value for a key from the embedded defaults file, or empty
// string if absent.
func Get(key string) string { return defaults[key] }

// All returns a copy of the full defaults map.
func All() map[string]string {
	cp := make(map[string]string, len(defaults))
	for k, v := range defaults {
		cp[k] = v
	}
	return cp
}

// Operator default images, sourced from the embedded defaults file.
var (
	PostgresImage = defaults["POSTGRES_IMAGE"]
	MetadataImage = defaults["METADATA_IMAGE"]
	MetadataTag   = defaults["METADATA_TAG"]
	EnvoyImage    = defaults["ENVOY_IMAGE"]
	EnvoyTag      = defaults["ENVOY_TAG"]
	EngineImage   = defaults["ENGINE_IMAGE"]
	EngineTag     = defaults["ENGINE_TAG"]
	CoreUIImage   = defaults["CORE_UI_IMAGE"]
	CoreUITag     = defaults["CORE_UI_TAG"]
)

// DefaultMetadata returns "repo:tag" for the metadata service image.
func DefaultMetadata() string { return MetadataImage + ":" + MetadataTag }

// DefaultEnvoy returns "repo:tag" for the Envoy proxy image.
func DefaultEnvoy() string { return EnvoyImage + ":" + EnvoyTag }

// DefaultEngine returns "repo:tag" for the engine (firebolt-db) image.
func DefaultEngine() string { return EngineImage + ":" + EngineTag }

// DefaultCoreUI returns "repo:tag" for the optional Core UI sidecar image.
func DefaultCoreUI() string { return CoreUIImage + ":" + CoreUITag }

// Variant reports which defaults file is embedded in this build, "latest"
// or "dev". The two variants must travel together across the operator
// binary and the E2E suite, so this is exposed for diagnostics and for
// the load-images script's pre-flight checks.
func Variant() string { return variant }
