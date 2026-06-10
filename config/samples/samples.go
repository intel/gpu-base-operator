// Copyright 2025 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package samples

import (
	_ "embed"

	api "github.com/intel/gpu-base-operator/api/v1alpha1"

	"sigs.k8s.io/yaml"
)

//go:embed deviceplugin/clusterpolicy.yaml
var devicepluginPolicy []byte

//go:embed dra/clusterpolicy.yaml
var draPolicy []byte

//go:embed base/clusterpolicy.yaml
var basePolicy []byte

func ListCollateralImages() map[string]struct{} {
	dpcp := getClusterPolicy(devicepluginPolicy)
	dracp := getClusterPolicy(draPolicy)
	basecp := getClusterPolicy(basePolicy)

	images := make(map[string]struct{})

	images[dpcp.Spec.DevicePluginSpec.PluginImage] = struct{}{}
	images[dracp.Spec.DynamicResourceAllocationSpec.Image] = struct{}{}
	images[basecp.Spec.XpuManagerSpec.Image] = struct{}{}

	return images
}

// generic cp getter
func getClusterPolicy(content []byte) *api.ClusterPolicy {
	var result api.ClusterPolicy

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}
