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

package controller

import (
	"context"
	"fmt"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	core "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
	prometheusv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	nfdcrd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"
)

type MiscReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

const (
	nfdRuleName = "intel-gpu-devices"

	nfdRuleCrd        = "nodefeaturerules.nfd.k8s-sigs.io"
	serviceMonitorCrd = "servicemonitors.monitoring.coreos.com"

	gpuResourceName         = "gpu.intel.com"
	gpui915ResourceName     = gpuResourceName + "/i915"
	gpuXeResourceName       = gpuResourceName + "/xe"
	draResourceName         = "dra." + gpuResourceName
	kueueAPIVersion         = "kueue.x-k8s.io/v1beta2"
	kueueResourceFlavorCrd  = "resourceflavors.kueue.x-k8s.io"
	kueueResourceFlavorKind = "ResourceFlavor"
	kueueClusterQueueCrd    = "clusterqueues.kueue.x-k8s.io"
	kueueClusterQueueKind   = "ClusterQueue"
	kueueLocalQueueCrd      = "localqueues.kueue.x-k8s.io"
	kueueLocalQueueKind     = "LocalQueue"
	kueueFlavorName         = gpuResourceName + "-kueue-flavor"
	kueueAppLabel           = gpuResourceName + "-kueue-queue"
)

// +kubebuilder:rbac:groups="kueue.x-k8s.io",resources=clusterqueues,verbs=watch;list;create;get;delete;update;patch;deletecollection
// +kubebuilder:rbac:groups="kueue.x-k8s.io",resources=resourceflavors,verbs=watch;list;create;get;delete;update;patch;deletecollection
// +kubebuilder:rbac:groups="kueue.x-k8s.io",resources=localqueues,verbs=watch;list;create;get;delete;update;patch;deletecollection

func createNfdRule(cp *v1alpha.ClusterPolicy, namespace string) *nfdcrd.NodeFeatureRule {
	nfr := deployments.NFDNodeFeatureRulesGpu()

	nfr.Labels = map[string]string{
		"app":   "nfd-gpu",
		"owner": cp.Name,
	}

	if len(namespace) > 0 {
		nfr.Namespace = namespace
	}

	return nfr
}

func (r *MiscReconciler) reconcilePrometheusComponents(ctx context.Context, cp *v1alpha.ClusterPolicy) error {
	_ = logf.FromContext(ctx)

	if cp == nil {
		return nil
	}

	if found, err := r.checkIfCRDsExists(ctx, serviceMonitorCrd); err != nil {
		klog.Error(err, "unable to check if CRDs exist")

		return err
	} else if !found {
		return nil
	}

	klog.Info("Reconciling Prometheus integration")

	matchLabels := map[string]string{
		"app":   "intel-xpumanager",
		"owner": cp.Name,
	}

	monitors := &prometheusv1.ServiceMonitorList{}

	err := r.List(ctx, monitors, client.InNamespace(r.Opts.Namespace), client.MatchingLabels(matchLabels))
	if err != nil {
		klog.Error(err, "Failed to list ServiceMonitors")
		return err
	}

	services := &core.ServiceList{}

	err = r.List(ctx, services, client.InNamespace(r.Opts.Namespace), client.MatchingLabels(matchLabels))
	if err != nil {
		klog.Error(err, "Failed to list Services")
		return err
	}

	monitorAndProm := cp.Spec.ResourceMonitoring && cp.Spec.PrometheusIntegration

	if (len(monitors.Items) == 0 || len(services.Items) == 0) && monitorAndProm {
		objs := []client.Object{}

		sm := deployments.PrometheusServiceMonitor()
		sm.Namespace = r.Opts.Namespace

		sm.Spec.NamespaceSelector.MatchNames = []string{r.Opts.Namespace}
		sm.Labels = matchLabels

		objs = append(objs, sm)

		serv := deployments.XpuManagerService()
		serv.Namespace = r.Opts.Namespace
		serv.Labels = matchLabels

		objs = append(objs, serv)

		for _, obj := range objs {
			if err := r.Create(ctx, obj); err != nil {
				if client.IgnoreAlreadyExists(err) == nil {
					continue
				}

				klog.Error(err, "Failed to create object", obj)
				return err
			}
		}
	} else if (len(monitors.Items) > 0 || len(services.Items) > 0) && !monitorAndProm {
		for _, sm := range monitors.Items {
			klog.Info("Deleting ServiceMonitor for Prometheus integration")

			if err := r.Delete(ctx, &sm); err != nil {
				klog.Error(err, "Failed to delete ServiceMonitor")
				return err
			}
		}
		for _, s := range services.Items {
			klog.Info("Deleting Service for Prometheus integration")

			if err := r.Delete(ctx, &s); err != nil {
				klog.Error(err, "Failed to delete Service")
				return err
			}
		}
	}

	return nil
}

func (r *MiscReconciler) removePrometheusComponents(ctx context.Context, cRName string) error {
	if found, err := r.checkIfCRDsExists(ctx, serviceMonitorCrd); err != nil {
		klog.Error(err, "unable to check if CRDs exist")

		return err
	} else if !found {
		return nil
	}

	matchLabels := map[string]string{
		"app":   "intel-xpumanager",
		"owner": cRName,
	}

	sm := deployments.PrometheusServiceMonitor()
	sm.Namespace = r.Opts.Namespace
	sm.Spec.NamespaceSelector.MatchNames = []string{r.Opts.Namespace}

	if err := r.Delete(ctx, sm); err != nil {
		if client.IgnoreNotFound(err) != nil {
			klog.Error(err, "unable to delete ServiceMonitor")

			return err
		}

		klog.Info("Removed Prometheus ServiceMonitor", "name", cRName)
	}

	serv := deployments.XpuManagerService()
	serv.Namespace = r.Opts.Namespace
	serv.Labels = matchLabels

	if err := r.Delete(ctx, serv); err != nil {
		if client.IgnoreNotFound(err) != nil {
			klog.Error(err, "unable to delete Service")

			return err
		}

		klog.Info("Removed XPUM Service", "name", cRName)
	}

	return nil
}

func (r *MiscReconciler) checkIfCRDsExists(ctx context.Context, crdName string) (bool, error) {
	// Check if CRDs already exist using unstructured.UnstructuredList
	crdList := &unstructured.UnstructuredList{}
	crdList.SetKind("CustomResourceDefinition")
	crdList.SetAPIVersion("apiextensions.k8s.io/v1")

	if err := r.List(ctx, crdList); err != nil {
		klog.Error(err, "unable to list CRDs")

		return false, err
	}

	for _, crd := range crdList.Items {
		if crd.GetName() == crdName {
			return true, nil
		}
	}

	return false, nil
}

// getNFDCRDScope returns the scope of the NFD NodeFeatureRule CRD.
// Returns apiextensionsv1.ClusterScoped or apiextensionsv1.NamespaceScoped.
func (r *MiscReconciler) getNFDCRDScope(ctx context.Context) (apiextensionsv1.ResourceScope, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.Get(ctx, types.NamespacedName{Name: nfdRuleCrd}, crd); err != nil {
		return "", fmt.Errorf("unable to get NFD CRD %s: %w", nfdRuleCrd, err)
	}

	return crd.Spec.Scope, nil
}

func (r *MiscReconciler) reconcileNfdRules(ctx context.Context, cp *v1alpha.ClusterPolicy) error {
	_ = logf.FromContext(ctx)

	// Get NFD CRD scope to create the rules correctly.
	// Also use the error to determine if the CRD is installed or not.
	scope, err := r.getNFDCRDScope(ctx)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}

		klog.Error(err, "unable to determine NFD CRD scope")

		return err
	}

	klog.V(4).Info("Reconciling NFD NodeFeatureRules for GPU detection")

	namedObject := types.NamespacedName{Name: nfdRuleName}

	nfrNamespace := ""
	if scope == apiextensionsv1.NamespaceScoped {
		nfrNamespace = r.Opts.Namespace
		namedObject = types.NamespacedName{Name: nfdRuleName, Namespace: nfrNamespace}
	}

	if cp.Spec.UseNFDLabeling {
		// Directly trying to use the NFD object in the Get call results in:
		//   no kind "NodeFeatureRuleList" is registered for version "nfd.k8s-sigs.io/v1alpha1" in
		//   scheme "pkg/runtime/scheme.go:110" unable to get NodeFeatureRule

		// Use unstructured object to work with dynamically installed CRDs
		currentNfr := &unstructured.Unstructured{}
		currentNfr.SetKind("NodeFeatureRule")
		currentNfr.SetAPIVersion("nfd.k8s-sigs.io/v1alpha1")
		currentNfr.SetName(nfdRuleName)

		found := true
		if err := r.Get(ctx, namedObject, currentNfr); err != nil {
			if client.IgnoreNotFound(err) != nil {
				klog.Error(err, " unable to get NodeFeatureRule")

				return err
			}

			found = false
		}

		if found {
			nfr := &nfdcrd.NodeFeatureRule{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(currentNfr.UnstructuredContent(), nfr)
			if err != nil {
				klog.Error(err, "unable to convert unstructured to NodeFeatureRule")

				return err
			}

			fromPolicy := createNfdRule(cp, "")

			specDiff := cmp.Diff(nfr.Spec, fromPolicy.Spec, cmpopts.EquateEmpty())
			if len(specDiff) > 0 {
				klog.Info("Updating NFD NodeFeatureRule for GPU detection", "diff", specDiff)

				nfr.Spec = fromPolicy.Spec
				if err := r.Update(ctx, nfr); err != nil {
					klog.Error(err, "unable to update NodeFeatureRule")

					return err
				}
			}
		} else {
			newNfr := createNfdRule(cp, nfrNamespace)
			if err := r.Create(ctx, newNfr); err != nil {
				klog.Error(err, "unable to create NodeFeatureRule")

				return err
			}
		}
	} else {
		newNfr := createNfdRule(cp, nfrNamespace)

		if err := r.Delete(ctx, newNfr); err != nil {
			if client.IgnoreNotFound(err) != nil {
				klog.Error(err, "Failed to delete NodeFeatureRule")

				return err
			}
		}
	}

	return nil
}

func (r *MiscReconciler) removeNfdRules(ctx context.Context, crName string) error {
	_ = logf.FromContext(ctx)

	// Get NFD CRD scope to delete the rules correctly.
	// Also use the error to determine if the CRD is installed or not.
	scope, err := r.getNFDCRDScope(ctx)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}

		klog.Error(err, "unable to determine NFD CRD scope")

		return err
	}

	matchLabels := map[string]string{
		"app":   "nfd-gpu",
		"owner": crName,
	}

	nfr := deployments.NFDNodeFeatureRulesGpu()
	nfr.Labels = matchLabels

	if scope == apiextensionsv1.NamespaceScoped {
		nfr.Namespace = r.Opts.Namespace
	}

	if err := r.Delete(ctx, nfr); err != nil {
		if client.IgnoreNotFound(err) != nil {
			klog.Error(err, "Failed to delete NodeFeatureRule")

			return err
		}
	}

	return nil
}

type clusterNodeMap map[string]*core.Node

type clusterResourceMap map[string]*resource.Quantity

func (r *MiscReconciler) getClusterNodes(ctx context.Context) (clusterNodeMap, error) {
	var nodes core.NodeList
	if err := r.List(ctx, &nodes, &client.ListOptions{}); err != nil {
		return nil, err
	}

	clusterNodes := make(clusterNodeMap)
	for _, node := range nodes.Items {
		clusterNodes[node.Name] = &node

		klog.V(3).Infof("Found node '%s'", node.Name)
	}

	return clusterNodes, nil
}

func addToResource(resources clusterResourceMap, kind string, quantity resource.Quantity) {
	if resources[kind] == nil {
		resources[kind] = &quantity
	} else {
		resources[kind].Add(quantity)
	}

	klog.V(3).Infof("Added %s '%s' resource, total %v", quantity.String(), kind, resources[kind])
}

func (r *MiscReconciler) divideResources(clusterResources clusterResourceMap, numQueues int64) ([]clusterResourceMap, error) {
	queueResources := []clusterResourceMap{}

	for i := int64(0); i < numQueues; i++ {
		klog.V(3).Infof("Dividing resources (%d/%d)", i+1, numQueues)

		localResources := make(clusterResourceMap)
		for kind, quantity := range clusterResources {
			if quantity.IsZero() {
				continue
			}
			value := quantity.Value() / numQueues
			mod := quantity.Value() % numQueues
			if i < mod {
				value += 1
			}

			if value < 1 {
				return nil, fmt.Errorf("unable to create %d queues using %d '%s' reources", numQueues, quantity.Value(), kind)
			}

			localResources[kind] = resource.NewQuantity(value, "")

			klog.V(3).Infof("queue %d resource '%s' value %v (of %v)", i+1, kind, localResources[kind], quantity)
		}
		queueResources = append(queueResources, localResources)
	}
	return queueResources, nil
}

func (r *MiscReconciler) createResourceFlavor() *kueuev1beta2.ResourceFlavor {
	labels := map[string]string{
		"app":   kueueAppLabel,
		"owner": r.Opts.ReqName,
	}

	return &kueuev1beta2.ResourceFlavor{
		ObjectMeta: metav1.ObjectMeta{
			Name:   kueueFlavorName,
			Labels: labels,
		},
	}
}

func (r *MiscReconciler) modifyResourceFlavor(ctx context.Context) error {
	currentUnstructured := &unstructured.Unstructured{}
	currentUnstructured.SetKind(kueueResourceFlavorKind)
	currentUnstructured.SetAPIVersion(kueueAPIVersion)
	currentUnstructured.SetName(kueueFlavorName)

	found := true
	if err := r.Get(ctx, types.NamespacedName{Name: kueueFlavorName}, currentUnstructured); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot fetch Kueue '%s': %v", kueueFlavorName, err)
		}
		found = false
	}

	if !found {
		err := r.Create(ctx, r.createResourceFlavor())
		if client.IgnoreAlreadyExists(err) != nil {
			klog.Error(err, "unable to create Kueue ResourceFlavor '%s'", kueueFlavorName)
			return err
		}

		klog.V(3).Infof("Created ResourceFlavor '%s'", kueueFlavorName)
	}

	return nil
}

func (r *MiscReconciler) getDevicePluginResources(clusterNodes clusterNodeMap) clusterResourceMap {
	resources := make(clusterResourceMap)
	for _, node := range clusterNodes {
		resourceAdded := false

		klog.V(3).Infof("Calculating resources for node '%s", node.Name)

		for _, resourceName := range []core.ResourceName{core.ResourceName(gpui915ResourceName), core.ResourceName(gpuXeResourceName)} {
			quantity, exists := node.Status.Allocatable[resourceName]
			if exists {
				addToResource(resources, string(resourceName), quantity)
				resourceAdded = true
			}
		}

		if resourceAdded {
			addToResource(resources, string(core.ResourceCPU), node.Status.Allocatable[core.ResourceCPU])
			addToResource(resources, string(core.ResourceMemory), node.Status.Allocatable[core.ResourceMemory])
		}
	}

	return resources
}

func (r *MiscReconciler) addResourceFromResourceSlice(rs *resourcev1.ResourceSlice, resources clusterResourceMap, clusterNodes clusterNodeMap) *string {
	nodeName := rs.Spec.NodeName

	_, cnExists := clusterNodes[*nodeName]
	if !cnExists {
		klog.Errorf("ResourceSlice references unknown node '%s'", *nodeName)
		return nil
	}

	if rs.Spec.Driver != gpuResourceName {
		return nil
	}

	hasDevices := false
	for _, dev := range rs.Spec.Devices {
		if dev.Attributes == nil && dev.Capacity == nil {
			continue
		}

		addToResource(resources, draResourceName, *resource.NewQuantity(1, ""))
		hasDevices = true
	}

	if hasDevices {
		return nodeName
	}

	return nil
}

func (r *MiscReconciler) getDraResources(ctx context.Context, clusterNodes clusterNodeMap) clusterResourceMap {
	var resourceSliceList resourcev1.ResourceSliceList
	if err := r.List(ctx, &resourceSliceList, &client.ListOptions{}); err != nil {
		klog.Infof("Could not list resource slices: %v", err)
		return nil
	}

	addNodes := []string{}
	resources := make(clusterResourceMap)

	for _, rs := range resourceSliceList.Items {
		klog.V(3).Infof("ResourceSlice: %s/%s\n", rs.Namespace, rs.Name)
		nodeName := r.addResourceFromResourceSlice(&rs, resources, clusterNodes)
		if nodeName != nil {
			addNodes = append(addNodes, *nodeName)
		}
	}

	for _, nodeName := range addNodes {
		if node, cnExists := clusterNodes[nodeName]; cnExists {
			addToResource(resources, string(core.ResourceCPU), node.Status.Allocatable[core.ResourceCPU])
			addToResource(resources, string(core.ResourceMemory), node.Status.Allocatable[core.ResourceMemory])
		}
	}

	return resources
}

func (r *MiscReconciler) createLocalQueue(clusterQueueName string, localQueueSpec *v1alpha.LocalQueueSpec) *kueuev1beta2.LocalQueue {
	labels := map[string]string{
		"app":   kueueAppLabel,
		"owner": r.Opts.ReqName,
	}

	localQueue := kueuev1beta2.LocalQueue{
		ObjectMeta: metav1.ObjectMeta{
			Name:      localQueueSpec.Name,
			Namespace: localQueueSpec.Namespace,
			Labels:    labels,
		},
		Spec: kueuev1beta2.LocalQueueSpec{
			ClusterQueue: kueuev1beta2.ClusterQueueReference(clusterQueueName),
		},
	}

	return &localQueue
}

func (r *MiscReconciler) modifyLocalQueues(ctx context.Context, clusterQueue *v1alpha.ClusterQueueSpec) error {
	if len(clusterQueue.LocalQueues) == 0 {
		klog.V(3).Infof("No LocalQueues defined for ClusterQueue '%s'", clusterQueue.Name)
		return nil
	}

	for _, localQueue := range clusterQueue.LocalQueues {
		currentUnstructured := &unstructured.Unstructured{}
		currentUnstructured.SetKind(kueueLocalQueueKind)
		currentUnstructured.SetAPIVersion(kueueAPIVersion)
		currentUnstructured.SetName(localQueue.Name)

		found := true
		if err := r.Get(ctx, types.NamespacedName{Name: localQueue.Name, Namespace: localQueue.Namespace}, currentUnstructured); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("cannot fetch Kueue LocalQueue '%s': %v", localQueue.Name, err)
			}
			found = false
		}

		newLocalQueue := r.createLocalQueue(clusterQueue.Name, &localQueue)

		if !found {
			err := r.Create(ctx, newLocalQueue)
			if client.IgnoreAlreadyExists(err) != nil {
				klog.Error(err, "unable to create Kueue LocalQueue '%s/%s'", localQueue.Namespace, localQueue.Name)
				return err
			}

			klog.V(3).Infof("Created LocalQueue '%s/%s'", localQueue.Namespace, localQueue.Name)

			continue
		}

		currentLocalQueue := &kueuev1beta2.LocalQueue{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(currentUnstructured.UnstructuredContent(), currentLocalQueue); err != nil {
			klog.Error(err, "unable to convert unstructured to LocalQueue '%s'", localQueue.Name)
			return err
		}

		specDiff := cmp.Diff(currentLocalQueue.Spec, newLocalQueue.Spec, cmpopts.EquateEmpty())
		if len(specDiff) > 0 {
			klog.V(3).Infof("Updating LocalQueue '%s/%s'", localQueue.Namespace, localQueue.Name)

			currentLocalQueue.Spec = newLocalQueue.Spec
			err := r.Update(ctx, currentLocalQueue)
			if client.IgnoreAlreadyExists(err) != nil {
				klog.Error(err, "unable to update Kueue LocalQueue '%s/%s'", localQueue.Namespace, localQueue.Name)
				return err
			}

			klog.V(3).Infof("Updated LocalQueue '%s/%s'", localQueue.Namespace, localQueue.Name)
		} else {
			klog.V(3).Infof("No changes to LocalQueue '%s/%s'", localQueue.Namespace, localQueue.Name)
		}
	}

	return nil
}

func (r *MiscReconciler) createClusterQueue(resources clusterResourceMap, clusterQueueSpec *v1alpha.ClusterQueueSpec) *kueuev1beta2.ClusterQueue {
	coveredResources := []core.ResourceName{}
	resourceQuotas := []kueuev1beta2.ResourceQuota{}

	labels := map[string]string{
		"app":   kueueAppLabel,
		"owner": r.Opts.ReqName,
	}

	for name, res := range resources {
		coveredResources = append(coveredResources, core.ResourceName(name))
		resourceQuotas = append(resourceQuotas,
			kueuev1beta2.ResourceQuota{
				Name:         core.ResourceName(name),
				NominalQuota: *res,
			})
	}

	clusterQueue := &kueuev1beta2.ClusterQueue{
		ObjectMeta: metav1.ObjectMeta{
			Name:   clusterQueueSpec.Name,
			Labels: labels,
		},
		Spec: kueuev1beta2.ClusterQueueSpec{
			NamespaceSelector: &metav1.LabelSelector{},
			ResourceGroups: []kueuev1beta2.ResourceGroup{
				{
					CoveredResources: coveredResources,
					Flavors: []kueuev1beta2.FlavorQuotas{
						{
							Name:      kueueFlavorName,
							Resources: resourceQuotas,
						},
					},
				},
			},
		},
	}

	return clusterQueue
}

func (r *MiscReconciler) modifyClusterQueue(ctx context.Context, resources clusterResourceMap, kueueSpec *v1alpha.KueueQueueSpec) error {
	if len(kueueSpec.EqualResources) == 0 {
		klog.V(3).Infof("At least one EqualResources queue should be configured")
		return nil
	}

	clusterResources, err := r.divideResources(resources, int64(len(kueueSpec.EqualResources)))
	if err != nil {
		return err
	}

	for n, clusterQueue := range kueueSpec.EqualResources {
		currentUnstructured := &unstructured.Unstructured{}
		currentUnstructured.SetKind(kueueClusterQueueKind)
		currentUnstructured.SetAPIVersion(kueueAPIVersion)
		currentUnstructured.SetName(clusterQueue.Name)

		found := true
		if err := r.Get(ctx, types.NamespacedName{Name: clusterQueue.Name}, currentUnstructured); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("cannot fetch Kueue '%s': %v", clusterQueue.Name, err)
			}
			found = false
		}

		newClusterQueue := r.createClusterQueue(clusterResources[n], &clusterQueue)

		if !found {
			err = r.Create(ctx, newClusterQueue)
			if client.IgnoreAlreadyExists(err) != nil {
				klog.Error(err, "unable to create Kueue ClusterQueue '%s'", clusterQueue.Name)
				return err
			}

			klog.V(3).Infof("Created ClusterQueue '%s'", clusterQueue.Name)
		} else {

			currentClusterQueue := &kueuev1beta2.ClusterQueue{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(currentUnstructured.UnstructuredContent(), currentClusterQueue); err != nil {
				klog.Error(err, "unable to convert unstructured to ClusterQueue")
				return err
			}

			specDiff := cmp.Diff(currentClusterQueue.Spec, newClusterQueue.Spec, cmpopts.EquateEmpty())
			if len(specDiff) > 0 {
				klog.V(3).Infof("Updating ClusterQueue '%s'", clusterQueue.Name)

				currentClusterQueue.Spec = newClusterQueue.Spec
				err = r.Update(ctx, currentClusterQueue)
				if client.IgnoreAlreadyExists(err) != nil {
					klog.Error(err, "unable to update Kueue ClusterQueue '%s'", clusterQueue.Name)
					return err
				}

				klog.V(3).Infof("Updated ClusterQueue '%s'", clusterQueue.Name)
			} else {
				klog.V(3).Infof("No changes to ClusterQueue '%s'", clusterQueue.Name)
			}
		}

		if err := r.modifyLocalQueues(ctx, &clusterQueue); err != nil {
			return err
		}
	}

	return nil
}

func (r *MiscReconciler) createKueueQueues(ctx context.Context, resources clusterResourceMap, kueueSpec *v1alpha.KueueQueueSpec) error {
	if resources == nil {
		klog.V(3).Infof("no resources defined when attempting to create Kueue queues")
		return nil
	}

	if err := r.modifyResourceFlavor(ctx); err != nil {
		return err
	}

	if err := r.modifyClusterQueue(ctx, resources, kueueSpec); err != nil {
		return err
	}

	return nil
}

func (r *MiscReconciler) removeKueueObjects(ctx context.Context, crName string) error {
	_ = logf.FromContext(ctx)

	if found, err := r.checkIfCRDsExists(ctx, kueueClusterQueueCrd); err != nil {
		klog.Error(err, "unable to check if Kueue is installed")
		return err
	} else if !found {
		return nil
	}

	matchLabels := map[string]string{
		"app":   kueueAppLabel,
		"owner": crName,
	}

	resourceFlavorCrd := &kueuev1beta2.ResourceFlavor{}

	if err := r.DeleteAllOf(ctx, resourceFlavorCrd, client.MatchingLabels(matchLabels)); err != nil {
		if client.IgnoreNotFound(err) == nil {
			klog.V(3).Infof("No Kueue ResourceFlavors to delete")
		} else {
			klog.Warningf("Error when attempting to delete Kueue Resourceflavors: %v", err)
		}
	} else {
		klog.V(3).Infof("Deleted Kueue ResourceFlavor")
	}

	clusterQueueCrd := &kueuev1beta2.ClusterQueue{}

	if err := r.DeleteAllOf(ctx, clusterQueueCrd, client.MatchingLabels(matchLabels)); err != nil {
		if client.IgnoreNotFound(err) == nil {
			klog.V(3).Infof("No Kueue ClusterQueues to delete")
		} else {
			klog.Warningf("Error when attempting to delete Kueue ClusterQueues: %v", err)
		}
	} else {
		klog.V(3).Infof("Deleted Kueue ClusterQueues")
	}

	localQueues := &kueuev1beta2.LocalQueueList{}
	if err := r.List(ctx, localQueues, client.InNamespace(metav1.NamespaceAll), client.MatchingLabels(matchLabels)); err != nil {
		klog.Warningf("Error when deleting Kueue LocalQueues: %v", err)
	} else {
		for i := range localQueues.Items {

			localQueueCrd := &localQueues.Items[i]

			if err := r.Delete(ctx, localQueueCrd); err != nil && client.IgnoreNotFound(err) != nil {
				klog.Warningf("Error when attempting to delete Kueue LocalQueue %s/%s: %v", localQueueCrd.Namespace, localQueueCrd.Name, err)
			} else {
				klog.V(3).Infof("Deleted LocalQueue '%s/%s'", localQueueCrd.Namespace, localQueueCrd.Name)
			}
		}
	}

	return nil
}

func (r *MiscReconciler) reconcileKueueObjects(ctx context.Context, cp *v1alpha.ClusterPolicy) error {
	var resources clusterResourceMap

	_ = logf.FromContext(ctx)

	for _, crd := range []string{kueueResourceFlavorCrd, kueueClusterQueueCrd, kueueLocalQueueCrd} {
		if found, err := r.checkIfCRDsExists(ctx, crd); err != nil {
			return fmt.Errorf("unable to check if Kueue is installed: %v", err)
		} else if !found {
			return nil
		}
	}

	if !cp.Spec.EnableKueue {
		klog.V(3).Info("Kueue disabled")
		return r.removeKueueObjects(ctx, cp.Name)
	}

	klog.V(3).Info("Kueue enabled and available")

	if cp.Spec.Kueue == nil || cp.Spec.Kueue.EqualResources == nil || len(cp.Spec.Kueue.EqualResources) == 0 {
		klog.V(3).Infof("Kueue enabled, but no EqualResources queues defined")
		return nil
	}

	clusternodes, err := r.getClusterNodes(ctx)

	if err != nil {
		return err
	}

	switch cp.Spec.ResourceRegistration {
	case "dp":
		resources = r.getDevicePluginResources(clusternodes)
	case "dra":
		resources = r.getDraResources(ctx, clusternodes)
	default:
		klog.Warningf("spec.resourceRegistration '%s' not supported", cp.Spec.ResourceRegistration)
		return nil
	}

	if len(resources) == 0 {
		klog.V(3).Infof("Kueue enabled, but cluster has no resources")
		return nil
	}

	return r.createKueueQueues(ctx, resources, cp.Spec.Kueue)
}

func (r *MiscReconciler) Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	if cp == nil || !cp.DeletionTimestamp.IsZero() {
		klog.Info("ClusterPolicy deleted, delete any existing components")

		var lastError error

		if err := r.removeNfdRules(ctx, r.Opts.ReqName); err != nil {
			klog.Error(err, "unable to remove NFD rules")
			lastError = err
		}

		if err := r.removePrometheusComponents(ctx, r.Opts.ReqName); err != nil {
			klog.Error(err, "unable to remove Prometheus components")
			lastError = err
		}

		if err := r.removeKueueObjects(ctx, r.Opts.ReqName); err != nil {
			klog.Error(err, "unable to remove Kueue rules")
			lastError = err
		}

		return ctrl.Result{}, lastError
	}

	if err := r.reconcilePrometheusComponents(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileNfdRules(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileKueueObjects(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
