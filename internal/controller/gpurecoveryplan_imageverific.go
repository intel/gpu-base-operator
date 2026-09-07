/*
Copyright 2026 Intel Corporation. All Rights Reserved.

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

	core "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

type recoveryImage struct {
	image string
	field string

	// insecureSkipTLSVerify carries the per-image opt-out from registry certificate validation.
	insecureSkipTLSVerify bool

	// files, when non-empty, must exist in the image. Empty means the check is reachability only
	files []ImageFile
}

// recoveryImagesForEvent lists the images the Job for this event will pull, in check order.
func recoveryImagesForEvent(plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) []recoveryImage {
	imgs := make([]recoveryImage, 0, 2)

	// Ignore xpuSmi image verification if pull policy is Never.
	if plan.Spec.XpuSmi.PullPolicy != string(core.PullNever) {
		if img := plan.Spec.XpuSmi.Image; img != "" {
			imgs = append(imgs, recoveryImage{
				image:                 img,
				field:                 "spec.xpuSmi.image",
				insecureSkipTLSVerify: plan.Spec.XpuSmi.InsecureSkipTLSVerify,
			})
		}
	}

	// If the event is a reflash, check also the firmware image.
	if evt.RecoveryType.IsReflash() {
		fw := plan.Spec.Firmware
		if fw != nil && fw.File != "" && fw.Source.ContainerSource != nil && fw.Source.ContainerSource.Name != "" {
			imgs = append(imgs, recoveryImage{
				image:                 fw.Source.ContainerSource.Name,
				field:                 "spec.firmware.source.containerSource.name",
				insecureSkipTLSVerify: fw.Source.ContainerSource.InsecureSkipTLSVerify,
				files:                 []ImageFile{{Name: firmwareImagePath(fw.File)}},
			})
		}
	}

	return imgs
}

// holdForImageFix sends an event whose images did not verify back to waiting-approval, recording the
// spec version the check was made against so it is not repeated until the plan changes.
func holdForImageFix(plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent, img recoveryImage, verifyErr error) {
	evt.ImageVerifyGeneration = plan.Generation

	// The state this leaves the event in says "waiting for an approval", which is not what is
	// happening — the approval is already there — so the message is mandatory here. Without it an
	// admin reading status.events would see a recovery that had silently stopped.
	setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval,
		"%s (%s) cannot be pulled: %v; correct the plan to retry (approval retained)",
		img.field, img.image, verifyErr)

	klog.Warningf("GPURecoveryPlan %s: event %s held at waiting-approval: %s (%s) is not usable: %v",
		plan.Name, evt.ID, img.field, img.image, verifyErr)

	appendMessage(plan, fmt.Sprintf("Event %s: recovery held back — %s", evt.ID, evt.StateMessage))
}

// ensureImagesUsable reports whether the recovery for this event may go ahead, verifying against the
// registry that every image its Job needs can be pulled. On failure the event is sent back to
// waiting-approval and false is returned; the approval is left unconsumed by the caller, so the
// recovery resumes on its own once the plan is corrected — one admin action, not two.
func ensureImagesUsable(imgVerify ContentImageVerifier, secretName string, ctx context.Context, plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) bool {
	if plan.Spec.SkipImageVerification {
		return true
	}

	// Already asked, against this exact spec. The approval is still in place, so this is reached on
	// every reconcile until the plan changes; going quiet is the point.
	if evt.ImageVerifyGeneration != 0 && evt.ImageVerifyGeneration == plan.Generation {
		klog.V(2).Infof("GPURecoveryPlan %s: event %s failed image verification at generation %d; waiting for a spec change",
			plan.Name, evt.ID, plan.Generation)

		return false
	}

	// Form a list of images and then check them, if not yet checked previously.
	for _, img := range recoveryImagesForEvent(plan, evt) {
		req := ImageVerifyRequest{
			Image:                 img.image,
			PullSecret:            secretName,
			InsecureSkipTLSVerify: img.insecureSkipTLSVerify,
			Files:                 img.files,
		}

		if err := imgVerify.VerifyImage(ctx, req); err != nil {
			holdForImageFix(plan, evt, img, err)

			return false
		}
	}

	if evt.ImageVerifyGeneration != 0 {
		appendMessage(plan, fmt.Sprintf("Event %s: images verified after an earlier failure; resuming recovery", evt.ID))
		klog.Infof("GPURecoveryPlan %s: event %s images now verify; resuming", plan.Name, evt.ID)
	}

	clearImageVerifyHold(evt)

	return true
}

// clearImageVerifyHold drops what a failed image verification left on the event.
func clearImageVerifyHold(evt *intelv1a1.RecoveryEvent) {
	evt.ImageVerifyGeneration = 0
	evt.StateMessage = ""
}
