/*
Copyright 2025 Intel Corporation. All Rights Reserved.

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

// Package testenv boots the envtest API server the controller test suites run against. It is
// imported only from tests, so it never ends up in the manager binary.
package testenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
	prometheusv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	nfdv1 "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	intelcomv1alpha1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// Start registers the API types the controllers use into s and starts an envtest API server
// with the operator's CRDs plus the third-party CRDs (NFD, KMM, OpenShift SCC) they create
// objects of.
func Start(s *k8sruntime.Scheme) (*envtest.Environment, *rest.Config, error) {
	for _, add := range []func(*k8sruntime.Scheme) error{
		intelcomv1alpha1.AddToScheme,
		apiextensions.AddToScheme,
		nfdv1.AddToScheme,
		prometheusv1.AddToScheme,
		resv1.AddToScheme,
		corev1.AddToScheme,
		kmmv1beta1.AddToScheme,
	} {
		if err := add(s); err != nil {
			return nil, nil, err
		}
	}

	root := repoRoot()

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join(root, "config", "crd", "bases"),
			filepath.Join(root, "config", "deployments", "nfd", "crds"),
			filepath.Join(goModDir("github.com/kubernetes-sigs/kernel-module-management"), "config", "crd", "bases"),
			filepath.Join(root, "internal", "controller", "common", "testenv", "testdata"),
		},
		ErrorIfCRDPathMissing: true,
	}

	testEnv.ControlPlane.APIServer = &envtest.APIServer{}
	// DRADeviceTaints is what makes the API server keep ResourceSlice device taints instead of
	// pruning them, and those taints are the recovery trigger the GPURecoveryPlan tests drive.
	testEnv.ControlPlane.APIServer.Configure().Append("feature-gates", "DynamicResourceAllocation=true,DRADeviceTaints=true")
	testEnv.ControlPlane.APIServer.Configure().Append("runtime-config", "resource.k8s.io/v1beta1=true,resource.k8s.io/v1alpha3=true")

	// Retrieve the first found binary directory to allow running tests from IDEs
	if dir := getFirstFoundEnvTestBinaryDir(root); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		return nil, nil, err
	}

	return testEnv, cfg, nil
}

// repoRoot returns the repository root, located relative to this source file so that it does
// not depend on which package's tests are running.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("unable to resolve the testenv source location")
	}

	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
}

// goModDir returns the module cache directory of the given module. '-mod=mod' is
// required because 'go list -m' reports an empty directory in vendor mode, and the
// module's non-Go files (e.g. CRD manifests) are not copied into 'vendor/' anyway.
func goModDir(module string) string {
	out, err := exec.Command("go", "list", "-mod=mod", "-m", "-f", "{{.Dir}}", module).Output()
	if err != nil {
		panic(fmt.Sprintf("failed to resolve module directory for %s: %v", module, err))
	}

	dir := strings.TrimSpace(string(out))
	if dir == "" {
		panic(fmt.Sprintf("empty module directory for %s, run 'go mod download %s'", module, module))
	}

	return dir
}

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir(root string) string {
	basePath := filepath.Join(root, "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
