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

package controller

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// buildResourceValues converts Kubernetes ResourceRequirements into a map
// suitable for Helm chart values.
func buildResourceValues(req *corev1.ResourceRequirements) map[string]interface{} {
	result := map[string]interface{}{}

	if req.Requests != nil {
		requests := map[string]interface{}{}
		if cpu, ok := req.Requests[corev1.ResourceCPU]; ok {
			requests["cpu"] = cpu.String()
		}
		if mem, ok := req.Requests[corev1.ResourceMemory]; ok {
			requests["memory"] = mem.String()
		}
		result["requests"] = requests
	}

	if req.Limits != nil {
		limits := map[string]interface{}{}
		if cpu, ok := req.Limits[corev1.ResourceCPU]; ok {
			limits["cpu"] = cpu.String()
		}
		if mem, ok := req.Limits[corev1.ResourceMemory]; ok {
			limits["memory"] = mem.String()
		}
		result["limits"] = limits
	}

	return result
}

// mergeJSONOverride parses raw JSON and deep-merges it into the destination map.
// Keys in the override take precedence.
func mergeJSONOverride(dst map[string]interface{}, raw []byte) error {
	var override map[string]interface{}
	if err := json.Unmarshal(raw, &override); err != nil {
		return fmt.Errorf("invalid JSON override: %w", err)
	}
	deepMerge(dst, override)
	return nil
}

func deepMerge(dst, src map[string]interface{}) {
	for key, srcVal := range src {
		dstVal, exists := dst[key]
		if !exists {
			dst[key] = srcVal
			continue
		}

		srcMap, srcIsMap := srcVal.(map[string]interface{})
		dstMap, dstIsMap := dstVal.(map[string]interface{})
		if srcIsMap && dstIsMap {
			deepMerge(dstMap, srcMap)
		} else {
			dst[key] = srcVal
		}
	}
}
