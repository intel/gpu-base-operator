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
	"bufio"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	intelcomv1alpha1 "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

const (
	xeResource   = "gpu.intel.com/xe"
	i915Resource = "gpu.intel.com/i915"

	gpuDraDeviceClass = "gpu.intel.com"

	canaryUpdate = "canary"
	directUpdate = "direct"

	stateNotStarted   = ""
	stateDraining     = "draining"
	stateUpdating     = "updating"
	stateError        = "error"
	stateCleanup      = "cleanup"
	stateComplete     = "completed"
	stateErrorCleanup = "error_cleanup"
	stateCanaryDone   = "canary_done"

	FirmwareTypeGFX         = "GFX"
	FirmwareTypeGFXDATA     = "GFX_DATA"
	FirmwareTypeGFXCODEDATA = "GFX_CODE_DATA"
	FirmwareTypeGFXPSCBIN   = "GFX_PSCBIN"
	FirmwareTypeAMC         = "AMC"
	FirmwareTypeFANTABLE    = "FAN_TABLE"
	FirmwareTypeVRCONFIG    = "VR_CONFIG"
	FirmwareTypeOPROMCODE   = "OPROM_CODE"
	FirmwareTypeOPROMDATA   = "OPROM_DATA"

	completedTag = "##COMPLETED##"

	gpuFwUpdateFinalizer = "gpufirmwareupdate.intel.com/finalizer"

	withError   = true
	withSuccess = false

	gpuFwUpdateResourcePart = "gpu-fwupdate"
)

var (
	supportedFwTypes = []string{
		FirmwareTypeGFX,
		FirmwareTypeGFXDATA,
		FirmwareTypeGFXCODEDATA,
		FirmwareTypeGFXPSCBIN,
		FirmwareTypeAMC,
		FirmwareTypeFANTABLE,
		FirmwareTypeVRCONFIG,
		FirmwareTypeOPROMCODE,
		FirmwareTypeOPROMDATA,
	}

	taintTemplate = core.Taint{
		Key:    "",
		Effect: core.TaintEffectNoSchedule,
	}
)

// GPUFirmwareUpdateReconciler reconciles a GPUFirmwareUpdate object
type GPUFirmwareUpdateReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	logRet    LogsRetriever
	imgVerify ContentImageVerifier
	Opts      ControllerOpts
}

type LogsRetriever interface {
	RetrieveLogsForPodContainer(ctx context.Context, pod *core.Pod, containerName string) ([]string, error)
}

func newLogsRetriever(cs *kubernetes.Clientset) LogsRetriever {
	return &DefaultLogsRetriever{
		Clientset: cs,
	}
}

type DefaultLogsRetriever struct {
	Clientset *kubernetes.Clientset
}

func (d *DefaultLogsRetriever) RetrieveLogsForPodContainer(ctx context.Context, pod *core.Pod, containerName string) ([]string, error) {
	var logs []string

	req := d.Clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &core.PodLogOptions{
		Container: containerName,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := stream.Close(); err != nil {
			klog.Errorf("failed to close log stream: %v", err)
		}
	}()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		logs = append(logs, scanner.Text())
	}

	return logs, nil
}

// +kubebuilder:rbac:groups=intel.com,resources=gpufirmwareupdates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=intel.com,resources=gpufirmwareupdates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=intel.com,resources=gpufirmwareupdates/finalizers,verbs=update

// +kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get;list;watch;update;patch

// Namespace-scoped resources (batch/jobs, core/pods, core/pods/log, core/secrets) are intentionally
// omitted here; they are granted via the namespaced Role in config/rbac/namespaced_role.yaml.

// checkJobCompletionFromLogs checks if a job has completed successfully by looking for "##COMPLETED##" in the logs
func (r *GPUFirmwareUpdateReconciler) checkJobCompletionFromLogs(ctx context.Context, job *batch.Job) (bool, error) {
	podList := &core.PodList{}

	matcher := client.MatchingLabels{"job-name": job.Name}

	if err := r.List(ctx, podList, client.InNamespace(job.Namespace), matcher); err != nil {
		return false, fmt.Errorf("failed to list pods for job %s: %v", job.Name, err)
	}

	if len(podList.Items) == 0 {
		// No pods found, job might not have started yet
		return false, nil
	}

	// Take the first Pod, there should always be only one Pod per Job.
	targetPod := podList.Items[0]

	if logs, err := r.logRet.RetrieveLogsForPodContainer(ctx, &targetPod, targetPod.Spec.Containers[0].Name); err != nil {
		return false, fmt.Errorf("failed to retrieve logs %+v", err)
	} else {
		for _, logLine := range logs {
			if strings.Contains(logLine, completedTag) {
				return true, nil
			}
		}
	}

	return false, nil
}

func (r *GPUFirmwareUpdateReconciler) verifyGivenParameters(fu *intelcomv1alpha1.GPUFirmwareUpdate) error {
	if fu.Spec.UpdaterImage == "" {
		return fmt.Errorf("updater image must be specified")
	}

	if fu.Spec.Content.ContainerImage == "" {
		return fmt.Errorf("content container image must be specified")
	}

	for _, fw := range fu.Spec.Content.Files {
		if !slices.Contains(supportedFwTypes, fw.Type) {
			return fmt.Errorf("unsupported firmware type %s", fw.Type)
		}

		if fw.Type == FirmwareTypeAMC && fu.Spec.AMCCredentialsSecret == "" {
			return fmt.Errorf("amcCredentialsSecret must be specified when firmware type AMC is requested")
		}
	}

	return nil
}

func (r *GPUFirmwareUpdateReconciler) taintAndDrainNodes(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate, nodesToProcess []string) ([]string, error) {
	taintToBeAdded := taintTemplate
	taintToBeAdded.Key = fu.Spec.UpdateTaint

	var lastErr error

	draining := []string{}

	for _, nodeName := range nodesToProcess {
		klog.Infof("Selected node \"%s\" for firmware update", nodeName)

		node := &core.Node{}

		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			lastErr = fmt.Errorf("failed to get node %s: %v", nodeName, err)

			break
		}

		if slices.Contains(node.Spec.Taints, taintToBeAdded) {
			klog.Infof("Node \"%s\" already has taint %s, skipping\n", nodeName, fu.Spec.UpdateTaint)
		} else {
			node.Spec.Taints = append(node.Spec.Taints, taintToBeAdded)

			if err := r.Update(ctx, node); err != nil {
				lastErr = fmt.Errorf("failed to update node %s with taint: %v", nodeName, err)

				break
			}
		}

		if toEvict, err := r.getGPUPodsForNode(ctx, nodeName); err != nil {
			lastErr = fmt.Errorf("failed to get GPU pods for node %s: %v", nodeName, err)
		} else {
			klog.Infof("Evicting %d pods from node %s\n", len(toEvict), nodeName)

			if len(toEvict) > 0 {
				draining = append(draining, nodeName)
			}

			for _, pod := range toEvict {
				if pod.DeletionTimestamp != nil {
					continue
				}

				if err := r.Delete(ctx, pod); err != nil {
					klog.Errorf("Failed to delete pod %s from node %s: %v", pod.Name, nodeName, err)
				} else {
					klog.Infof("Evicted pod %s from node %s", pod.Name, nodeName)
				}
			}
		}
	}

	return draining, lastErr
}

func (r *GPUFirmwareUpdateReconciler) selectNodesTaintAndDrain(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) ([]string, error) {
	klog.Infof("Select nodes, taint and drain them %s\n", fu.Name)

	// Always select all candidate nodes so we know the full scope up front.
	allCandidates := r.selectNodesToUpdate(ctx, fu, directUpdate)

	var firstBatch []string

	if fu.Spec.UpdateMethod == canaryUpdate && len(allCandidates) > 0 {
		firstBatch = allCandidates[:1]
		fu.Status.NodeInfos.Pending = allCandidates[1:]
	} else {
		firstBatch = allCandidates
	}

	fu.Status.NodeInfos.All = firstBatch

	draining, lastErr := r.taintAndDrainNodes(ctx, fu, firstBatch)
	fu.Status.NodeInfos.Draining = draining

	return firstBatch, lastErr
}

func (r *GPUFirmwareUpdateReconciler) selectNodesToUpdate(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate, updateMethod string) []string {
	selected := []string{}

	nodeList := &core.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels(fu.Spec.NodeSelector)); err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return selected
	}

	for _, node := range nodeList.Items {
		// Ignore nodes marked for deletion.
		if node.DeletionTimestamp != nil {
			continue
		}
		// Ignore NotReady nodes.
		nodeNotReady := false
		for _, cond := range node.Status.Conditions {
			if cond.Type == core.NodeReady && cond.Status != core.ConditionTrue {
				nodeNotReady = true
				break
			}
		}

		if nodeNotReady {
			continue
		}

		selected = append(selected, node.Name)
		if updateMethod == canaryUpdate {
			break
		}
	}

	return selected
}

func (r *GPUFirmwareUpdateReconciler) checkClaimForIntelGPURequests(ctx context.Context, claims []core.PodResourceClaim, namespace string) (bool, error) {
	for _, rc := range claims {
		if rc.ResourceClaimName != nil {
			resClaim := resv1.ResourceClaim{}

			if err := r.Get(ctx, client.ObjectKey{Name: *rc.ResourceClaimName, Namespace: namespace}, &resClaim); err != nil {
				return false, fmt.Errorf("failed to get ResourceClaim %s: %v", *rc.ResourceClaimName, err)
			}

			for _, req := range resClaim.Spec.Devices.Requests {
				if req.Exactly != nil && req.Exactly.DeviceClassName == gpuDraDeviceClass {
					return true, nil
				}
				for _, fa := range req.FirstAvailable {
					if fa.DeviceClassName == gpuDraDeviceClass {
						return true, nil
					}
				}
			}
		}
		if rc.ResourceClaimTemplateName != nil {
			resClaimTmpl := resv1.ResourceClaimTemplate{}

			if err := r.Get(ctx, client.ObjectKey{Name: *rc.ResourceClaimTemplateName, Namespace: namespace}, &resClaimTmpl); err != nil {
				return false, fmt.Errorf("failed to get ResourceClaimTemplate %s: %v", *rc.ResourceClaimTemplateName, err)
			}

			for _, req := range resClaimTmpl.Spec.Spec.Devices.Requests {
				if req.Exactly != nil && req.Exactly.DeviceClassName == gpuDraDeviceClass {
					return true, nil
				}
				for _, fa := range req.FirstAvailable {
					if fa.DeviceClassName == gpuDraDeviceClass {
						return true, nil
					}
				}
			}
		}
	}

	return false, nil
}

func (r *GPUFirmwareUpdateReconciler) getGPUPodsForNode(ctx context.Context, nodeName string) ([]*core.Pod, error) {
	var pods core.PodList

	listOpts := []client.ListOption{
		client.InNamespace(core.NamespaceAll), // Search across all namespaces
		client.MatchingFields{"spec.nodeName": nodeName},
	}

	if err := r.List(ctx, &pods, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %v", nodeName, err)
	}

	gpuPods := []*core.Pod{}
	for index := range pods.Items {
		pod := pods.Items[index]

		include := false

		// Check for extended GPU resources.
		for _, c := range pod.Spec.Containers {
			if _, found := c.Resources.Limits[xeResource]; found {
				include = true
			} else if _, found := c.Resources.Limits[i915Resource]; found {
				include = true
			}
		}

		// Check for DRA resource claims.
		if !include {
			if len(pod.Spec.ResourceClaims) > 0 {
				if isGpu, err := r.checkClaimForIntelGPURequests(ctx, pod.Spec.ResourceClaims, pod.Namespace); err != nil {
					return nil, fmt.Errorf("failed to check ResourceClaims for pod %s: %v", pod.Name, err)
				} else if isGpu {
					include = true
				}
			}
		}

		if include {
			gpuPods = append(gpuPods, &pods.Items[index])
		}
	}

	return gpuPods, nil
}

func (r *GPUFirmwareUpdateReconciler) verifyContentImage(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) error {
	return r.imgVerify.Verify(ctx, &fu.Spec)
}

func (r *GPUFirmwareUpdateReconciler) beginUpdate(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) (ctrl.Result, error) {
	if err := r.verifyGivenParameters(fu); err != nil {
		klog.Errorf("Invalid parameters for firmware update: %v", err)

		fu.Status.State = stateError
		fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("invalid parameters: %v", err))
		if err := r.Status().Update(ctx, fu); err != nil {
			klog.Error(err, "unable to update GPUFirmwareUpdate status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}

	if r.Opts.OpenShift {
		if err := r.ensureOpenShiftResources(ctx, fu.Name); err != nil {
			klog.Errorf("Failed to ensure OpenShift SCC resources for firmware update %s: %v", fu.Name, err)

			fu.Status.State = stateError
			fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("failed to ensure OpenShift SCC resources: %v", err))
			if statusErr := r.Status().Update(ctx, fu); statusErr != nil {
				klog.Error(statusErr, "unable to update GPUFirmwareUpdate status")
				return ctrl.Result{}, statusErr
			}

			return ctrl.Result{}, err
		}
	}

	if err := r.verifyContentImage(ctx, fu); err != nil {
		klog.Errorf("Content image verification failed for firmware update %s: %v", fu.Name, err)

		fu.Status.State = stateError
		fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("content image verification failed: %v", err))
		if err := r.Status().Update(ctx, fu); err != nil {
			klog.Error(err, "unable to update GPUFirmwareUpdate status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	selectedNodes, err := r.selectNodesTaintAndDrain(ctx, fu)
	if err != nil {
		klog.Errorf("Failed to start firmware update process: %v", err)

		fu.Status.State = stateError
		fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("unable to select nodes, taint or drain: %v", err))
		if err := r.Status().Update(ctx, fu); err != nil {
			klog.Error(err, "unable to update GPUFirmwareUpdate status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	if len(selectedNodes) == 0 {
		klog.Warningf("No nodes selected for firmware update %s, marking as error\n", fu.Name)
		fu.Status.State = stateError
		fu.Status.Messages = append(fu.Status.Messages, "No suitable nodes found for update")
		if err := r.Status().Update(ctx, fu); err != nil {
			klog.Error(err, "unable to update GPUFirmwareUpdate status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, fmt.Errorf("no suitable nodes found for update")
	}

	fu.Status.State = stateDraining
	fu.Status.NodeInfos.All = selectedNodes
	fu.Status.Messages = append(fu.Status.Messages, "Nodes selected, tainted and draining initiated")

	if err := r.Status().Update(ctx, fu); err != nil {
		klog.Error(err, "unable to update GPUFirmwareUpdate status")
		return ctrl.Result{}, err
	}

	controllerutil.AddFinalizer(fu, gpuFwUpdateFinalizer)
	if err := r.Update(ctx, fu); err != nil {
		klog.Error(err, "unable to update GPUFirmwareUpdate with finalizer")
		return ctrl.Result{}, err
	}

	// Requeue to check the draining status after some time.
	return ctrl.Result{RequeueAfter: time.Second * 5}, nil
}

func (r *GPUFirmwareUpdateReconciler) checkForNodeDrainStatus(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) (ctrl.Result, error) {
	remaining := []string{}

	var lastErr error

	for _, nodeName := range fu.Status.NodeInfos.Draining {
		pods, err := r.getGPUPodsForNode(ctx, nodeName)
		if err != nil {
			lastErr = err

			break
		}

		if len(pods) != 0 {
			remaining = append(remaining, nodeName)
		}
	}

	if lastErr != nil {
		klog.Errorf("Failed to check drain status: %v", lastErr)

		fu.Status.State = stateError
		fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("unable to check drain status: %v", lastErr))

		if err := r.Status().Update(ctx, fu); err != nil {
			klog.Error(err, "unable to update GPUFirmwareUpdate status")
		}

		return ctrl.Result{}, lastErr
	}

	retryDuration := time.Second * 5

	fu.Status.NodeInfos.Draining = remaining

	if len(remaining) == 0 {
		klog.Infof("All nodes drained for GPU Firmware Update %s, proceeding to update\n", fu.Name)

		fu.Status.State = stateUpdating
		fu.Status.Messages = append(fu.Status.Messages, "All nodes drained, proceeding to update")

		// Faster requeue to start the update.
		retryDuration = time.Second * 1
	}

	if err := r.Status().Update(ctx, fu); err != nil {
		klog.Error(err, "unable to update GPUFirmwareUpdate status")
	}

	return ctrl.Result{RequeueAfter: retryDuration}, nil
}

func (r *GPUFirmwareUpdateReconciler) ensureOpenShiftResources(ctx context.Context, fuName string) error {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(fuName, gpuFwUpdateResourcePart)

	if err := createServiceAccount(ctx, r.Client, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure FW update ServiceAccount: %w", err)
	}

	if err := ensureSCC(ctx, r.Client, buildFWUpdateSCC(sccName)); err != nil {
		return fmt.Errorf("failed to ensure FW update SCC: %w", err)
	}

	if err := createSCCRole(ctx, r.Client, roleName, sccName); err != nil {
		return fmt.Errorf("failed to ensure FW update SCC ClusterRole: %w", err)
	}

	if err := createSCCRoleBinding(ctx, r.Client, bindingName, roleName, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure FW update SCC ClusterRoleBinding: %w", err)
	}

	return nil
}

func (r *GPUFirmwareUpdateReconciler) cleanupOpenShiftResources(ctx context.Context, fuName string) {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(fuName, gpuFwUpdateResourcePart)
	deleteOpenShiftSCCResources(ctx, r.Client, sccName, roleName, bindingName, saName, r.Opts.Namespace)
}

func (r *GPUFirmwareUpdateReconciler) createUpdateJobObjForNode(nodeName string, fu *intelcomv1alpha1.GPUFirmwareUpdate) *batch.Job {
	job := deployments.XpuManagerFWUpdateJob()
	job.Namespace = r.Opts.Namespace
	job.Labels = map[string]string{
		"gpufirmwareupdate": fu.Name,
		"node":              nodeName,
	}

	jobName := fmt.Sprintf("%s-fwupdate-%s", fu.Name, nodeName)

	job.Name = jobName

	job.Spec.Template.Spec.Containers[0].Image = fu.Spec.UpdaterImage

	if fu.Spec.ImagePullSecret != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []core.LocalObjectReference{
			{Name: fu.Spec.ImagePullSecret},
		}
	}

	job.Spec.Template.Spec.Tolerations = []core.Toleration{
		{
			Key:    fu.Spec.UpdateTaint,
			Effect: core.TaintEffectNoSchedule,
		},
	}
	job.Spec.Template.Spec.Tolerations = append(job.Spec.Template.Spec.Tolerations, fu.Spec.Tolerations...)

	job.Spec.Template.Spec.NodeSelector = map[string]string{
		"kubernetes.io/hostname": nodeName,
	}
	job.Spec.BackoffLimit = ptr.To(int32(0))
	job.Spec.Completions = ptr.To(int32(1))

	if r.Opts.OpenShift {
		_, _, _, saName := buildOpenShiftNames(fu.Name, gpuFwUpdateResourcePart)
		job.Spec.Template.Spec.ServiceAccountName = saName
	}

	job.Spec.Template.Spec.InitContainers[0].Image = fu.Spec.Content.ContainerImage

	if fu.Spec.AMCCredentialsSecret != "" {
		job.Spec.Template.Spec.Containers[0].Env = append(
			job.Spec.Template.Spec.Containers[0].Env,
			core.EnvVar{
				Name: "AMC_USERNAME",
				ValueFrom: &core.EnvVarSource{
					SecretKeyRef: &core.SecretKeySelector{
						LocalObjectReference: core.LocalObjectReference{Name: fu.Spec.AMCCredentialsSecret},
						Key:                  "username",
					},
				},
			},
			core.EnvVar{
				Name: "AMC_PASSWORD",
				ValueFrom: &core.EnvVarSource{
					SecretKeyRef: &core.SecretKeySelector{
						LocalObjectReference: core.LocalObjectReference{Name: fu.Spec.AMCCredentialsSecret},
						Key:                  "password",
					},
				},
			},
		)
	}

	updateCommands := []string{fmt.Sprintf("echo \"Starting firmware update process on node %s\"", nodeName)}

	for _, fw := range fu.Spec.Content.Files {
		fwfile := fmt.Sprintf("/update/%s", fw.FileName)

		infoCmd := fmt.Sprintf("echo \"Updating %s firmware with file %s\"", fw.Type, fwfile)
		updateCmd := fmt.Sprintf("sh /update/update.sh \"%s\" \"%s\" \"%s\"", fu.Spec.PCIDeviceID, fw.Type, fwfile)

		updateCommands = append(updateCommands, infoCmd, updateCmd)
	}

	updateCommands = append(updateCommands, fmt.Sprintf("echo \"Firmware update process %s\"", completedTag))

	// Combine all commands into a single command string.
	job.Spec.Template.Spec.Containers[0].Args = []string{
		strings.Join(updateCommands, " && "),
	}

	return job
}

func (r *GPUFirmwareUpdateReconciler) startUpdateJobs(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) error {
	// Create Jobs for nodes that have not yet been updated.
	jobs := []string{}
	for _, nodeName := range fu.Status.NodeInfos.All {
		if slices.Contains(fu.Status.NodeInfos.Completed, nodeName) {
			continue
		}

		klog.Infof("Starting firmware update on node %s for GPU Firmware Update %s\n", nodeName, fu.Name)

		job := r.createUpdateJobObjForNode(nodeName, fu)

		if err := ctrl.SetControllerReference(fu, job, r.Scheme); err != nil {
			warning := fmt.Sprintf("Failed to set controller reference for job %s: %v", job.Name, err)
			fu.Status.Messages = append(fu.Status.Messages, warning)

			// Don't fail, just log.
			klog.Warning(warning)
		}

		err := r.Create(ctx, job)
		if err != nil && client.IgnoreAlreadyExists(err) == nil {
			klog.Warningf("Update Job %s for node %s already exists, skipping creation\n", job.Name, nodeName)
		} else if err != nil {
			klog.Errorf("Failed to create update Job for node %s: %v", nodeName, err)

			fu.Status.State = stateErrorCleanup
			fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("unable to create update Job for node %s: %v", nodeName, err))

			if err := r.Status().Update(ctx, fu); err != nil {
				klog.Error(err, "unable to update GPUFirmwareUpdate status")
				return err
			}

			return err
		}

		jobs = append(jobs, job.Name)
	}

	// Only set Updating for nodes that don't have a completed job yet.
	toUpdate := []string{}
	for _, nodeName := range fu.Status.NodeInfos.All {
		if !slices.Contains(fu.Status.NodeInfos.Completed, nodeName) {
			toUpdate = append(toUpdate, nodeName)
		}
	}
	fu.Status.NodeInfos.Updating = toUpdate
	fu.Status.NodeInfos.Jobs = append(fu.Status.NodeInfos.Jobs, jobs...)

	return nil
}

func (r *GPUFirmwareUpdateReconciler) continueUpdateJobs(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) (bool, error) {
	klog.V(3).Infof("Checking firmware update statuses for GPU Firmware Update (%s)\n", fu.Name)

	// Check the status of the update Jobs.
	jobList := &batch.JobList{}
	matcher := client.MatchingLabels{"gpufirmwareupdate": fu.Name}

	if err := r.List(ctx, jobList, client.InNamespace(r.Opts.Namespace), matcher); err != nil {
		klog.Errorf("Failed to list update Jobs: %v", err)

		fu.Status.State = stateErrorCleanup
		fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("unable to list update Jobs: %v", err))

		return false, err
	}

	remaining := []string{}
	updated := []string{}
	failed := []string{}

	for _, job := range jobList.Items {
		nodeName, found := job.Labels["node"]
		if !found {
			fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("Job with no node label found: %s", job.Name))

			klog.Errorf("Job with no node label found: %s", job.Name)
			continue
		}

		// Skip jobs for nodes that completed in a previous phase.
		if slices.Contains(fu.Status.NodeInfos.Completed, nodeName) {
			continue
		}

		if job.Status.Succeeded > 0 {
			// Check job completion using log analysis
			completed, err := r.checkJobCompletionFromLogs(ctx, &job)
			if err != nil {
				klog.Errorf("Error checking job completion for %s: %v", job.Name, err)
				fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("Error checking job %s: %v", job.Name, err))

				completed = false
			}

			if completed {
				updated = append(updated, nodeName)
				klog.Infof("Firmware update job %s completed successfully on node %s", job.Name, nodeName)
			} else {
				klog.Warningf("Firmware update job %s failed on node %s", job.Name, nodeName)
				failed = append(failed, nodeName)
			}
		} else if job.Status.Failed > 0 {
			failed = append(failed, nodeName)
			klog.Warningf("Firmware update job %s failed on node %s", job.Name, nodeName)
		} else {
			remaining = append(remaining, nodeName)
		}
	}

	fu.Status.NodeInfos.Error = failed
	fu.Status.NodeInfos.Updating = remaining
	fu.Status.NodeInfos.Completed = append(fu.Status.NodeInfos.Completed, updated...)

	if len(remaining) > 0 {
		// Nodes are still updating, requeue to check again after some time.
		return false, nil
	}

	// Set initial status here and change it in the ifs below
	fu.Status.State = stateCleanup

	msg := "Firmware update completed on all nodes"

	if len(fu.Status.NodeInfos.Error) > 0 {
		fu.Status.State = stateErrorCleanup

		msg = fmt.Sprintf("Firmware update completed with errors on %d nodes", len(fu.Status.NodeInfos.Error))
	} else if fu.Spec.UpdateMethod == canaryUpdate && len(fu.Status.NodeInfos.Pending) > 0 {
		fu.Status.State = stateCanaryDone

		msg = "Canary update succeeded, proceeding to full rollout"
	}

	fu.Status.Messages = append(fu.Status.Messages, msg)

	return true, nil
}

func (r *GPUFirmwareUpdateReconciler) startOrContinueUpdate(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) (ctrl.Result, error) {
	requeueDuration := time.Second * 5

	// If no nodes are currently updating, start the update process.
	if len(fu.Status.NodeInfos.Updating) == 0 {
		if err := r.startUpdateJobs(ctx, fu); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if okToContinue, err := r.continueUpdateJobs(ctx, fu); err != nil {
			return ctrl.Result{}, err
		} else if !okToContinue {
			// If not all updates are completed, requeue to check again after some time.
			return ctrl.Result{RequeueAfter: requeueDuration}, nil
		} else {
			// If all updates are completed, proceed to the next phase ~immediately.
			requeueDuration = time.Second * 1
		}
	}

	if err := r.Status().Update(ctx, fu); err != nil {
		klog.Error(err, "unable to update GPUFirmwareUpdate status")
	}

	return ctrl.Result{RequeueAfter: requeueDuration}, nil
}

func (r *GPUFirmwareUpdateReconciler) handleCanaryDone(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) (ctrl.Result, error) {
	if fu.Spec.HoldAfterCanary {
		klog.Infof("Canary update for %s complete, waiting for approval to proceed with full rollout", fu.Name)

		return ctrl.Result{RequeueAfter: time.Second * 10}, nil
	}

	remaining := fu.Status.NodeInfos.Pending
	fu.Status.NodeInfos.Pending = nil

	if len(remaining) == 0 {
		// Canary was the only matching node — go straight to cleanup.
		fu.Status.State = stateCleanup
		fu.Status.Messages = append(fu.Status.Messages, "Canary was the only node, proceeding to cleanup")

		if err := r.Status().Update(ctx, fu); err != nil {
			klog.Error(err, "unable to update GPUFirmwareUpdate status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: time.Second * 1}, nil
	}

	klog.Infof("Canary update complete for %s, starting full rollout on %d remaining nodes", fu.Name, len(remaining))
	fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("Canary succeeded, starting full rollout on %d remaining nodes", len(remaining)))

	// Expand All to include all tainted nodes (canary + remaining) so that
	// untaintNodesAndFinalize removes taints from every node at the end.
	fu.Status.NodeInfos.All = append(fu.Status.NodeInfos.All, remaining...)

	draining, err := r.taintAndDrainNodes(ctx, fu, remaining)
	if err != nil {
		klog.Errorf("Failed to taint/drain nodes for full rollout: %v", err)

		fu.Status.State = stateError
		fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("failed to taint/drain nodes for full rollout: %v", err))

		if updateErr := r.Status().Update(ctx, fu); updateErr != nil {
			klog.Error(updateErr, "unable to update GPUFirmwareUpdate status")
		}

		return ctrl.Result{}, err
	}

	fu.Status.NodeInfos.Draining = draining
	fu.Status.State = stateDraining

	if err := r.Status().Update(ctx, fu); err != nil {
		klog.Error(err, "unable to update GPUFirmwareUpdate status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Second * 5}, nil
}

func (r *GPUFirmwareUpdateReconciler) untaintNodesAndFinalize(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate, withError bool) (ctrl.Result, error) {
	taintToBeRemoved := taintTemplate
	taintToBeRemoved.Key = fu.Spec.UpdateTaint

	var lastErr error

	for _, nodeName := range fu.Status.NodeInfos.All {
		node := &core.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			klog.Errorf("Failed to get node %s: %v", nodeName, err)
			lastErr = err
			continue
		}

		tindex := slices.Index(node.Spec.Taints, taintToBeRemoved)

		if tindex < 0 {
			klog.Warningf("Node %s does not have taint %s, skipping\n", nodeName, fu.Spec.UpdateTaint)
			fu.Status.Messages = append(fu.Status.Messages, fmt.Sprintf("Node %s lost taint %s, skipping", nodeName, fu.Spec.UpdateTaint))

			continue
		}

		node.Spec.Taints = slices.Delete(node.Spec.Taints, tindex, tindex+1)

		if err := r.Update(ctx, node); err != nil {
			klog.Errorf("Failed to update node %s: %v", nodeName, err)
			lastErr = err
		}
	}

	endResult := "completed successfully"
	if lastErr != nil || withError {
		fu.Status.State = stateError
		endResult = "completed with errors"
	} else {
		fu.Status.State = stateComplete
	}

	klog.Infof("All nodes untainted, firmware update process %s", endResult)

	if err := r.Status().Update(ctx, fu); err != nil {
		klog.Error(err, "unable to update GPUFirmwareUpdate status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GPUFirmwareUpdateReconciler) finalCleanup(ctx context.Context, fu *intelcomv1alpha1.GPUFirmwareUpdate) (ctrl.Result, error) {
	// CR is being deleted, clean up any remaining Jobs and remove finalizer so it can be fully deleted.
	if fu.DeletionTimestamp != nil {
		for _, jobName := range fu.Status.NodeInfos.Jobs {
			job := &batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      jobName,
					Namespace: r.Opts.Namespace,
				},
			}

			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && client.IgnoreNotFound(err) != nil {
				klog.Errorf("Failed to delete job %s: %v", jobName, err)

				return ctrl.Result{}, err
			}
		}

		if r.Opts.OpenShift {
			r.cleanupOpenShiftResources(ctx, fu.Name)
		}

		if controllerutil.ContainsFinalizer(fu, gpuFwUpdateFinalizer) {
			controllerutil.RemoveFinalizer(fu, gpuFwUpdateFinalizer)

			if err := r.Update(context.Background(), fu); err != nil {
				klog.Error(err, "unable to remove finalizer from GPUFirmwareUpdate")

				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GPUFirmwareUpdateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	fu := &intelcomv1alpha1.GPUFirmwareUpdate{}

	if err := r.Get(ctx, req.NamespacedName, fu); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle early deletion where we have not yet started the update process
	// If update is ongoing, we have to wait it through before we can cleanup etc.
	if fu.DeletionTimestamp != nil {
		klog.Infof("GPUFirmwareUpdate %s is being deleted, performing cleanup if necessary", fu.Name)

		switch fu.Status.State {
		case stateCanaryDone:
			fallthrough
		case stateNotStarted:
			fallthrough
		case stateDraining:
			return r.untaintNodesAndFinalize(ctx, fu, withError)
		}
	}

	klog.V(3).Infof("GPUFirmwareUpdate (%s): update specs: method: %s, taint: %s, nodeSelector: %+v",
		fu.Name, fu.Spec.UpdateMethod, fu.Spec.UpdateTaint, fu.Spec.NodeSelector)
	klog.V(3).Infof("Update state: %s", fu.Status.State)

	switch fu.Status.State {
	case stateNotStarted:
		return r.beginUpdate(ctx, fu)
	case stateDraining:
		return r.checkForNodeDrainStatus(ctx, fu)
	case stateUpdating:
		return r.startOrContinueUpdate(ctx, fu)
	case stateCanaryDone:
		return r.handleCanaryDone(ctx, fu)
	case stateCleanup:
		return r.untaintNodesAndFinalize(ctx, fu, withSuccess)
	case stateErrorCleanup:
		return r.untaintNodesAndFinalize(ctx, fu, withError)
	case stateComplete:
		fallthrough
	case stateError:
		return r.finalCleanup(ctx, fu)
	default:
		klog.Fatalf("GPUFirmwareUpdate %s in unknown state %s", fu.Name, fu.Status.State)
	}

	return ctrl.Result{}, nil
}

func podIndexerFunc(rawObj client.Object) []string {
	pod := rawObj.(*core.Pod)
	return []string{pod.Spec.NodeName}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GPUFirmwareUpdateReconciler) SetupWithManager(mgr ctrl.Manager, copts ControllerOpts) error {
	r.Opts = copts

	// Create Kubernetes clientset for log access
	config := mgr.GetConfig()
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes clientset: %v", err)
	}
	r.logRet = newLogsRetriever(clientset)
	r.imgVerify = newContentImageVerifier(mgr.GetAPIReader(), copts.Namespace)

	pod := &core.Pod{}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), pod, "spec.nodeName", podIndexerFunc); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&intelcomv1alpha1.GPUFirmwareUpdate{}).
		Named("gpufirmwareupdate").
		Complete(r)
}
