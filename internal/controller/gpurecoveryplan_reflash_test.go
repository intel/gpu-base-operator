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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

// Reflash Jobs, which are built from the firmware-update template and need the
// firmware image staged before xpu-smi can flash from it.
var _ = Describe("GPURecoveryPlan Controller: reflash Jobs", func() {
	ctx := context.Background()

	// A card in survivability mode has firmware that no reset can fix, so the recovery is to write a
	// known-good image over it. That is a different Job from a reset — a firmware image, an
	// initContainer that stages it, and a shell command line rather than an argv — and every part the
	// operator fills in fails silently inside a pod if it is filled in wrongly.
	Context("Reflash Jobs", func() {
		const (
			reflashBDF  = "0000:4b:00.0"
			reflashFile = "gfx_fw.bin"
			fwImage     = "registry.example.com/intel/gpu-fw:2026.1"
		)

		// reflashPlan creates a plan (in the API server, so the Job's owner reference has a UID)
		// carrying one reflash event ready to be acted on, so createRecoveryJob is the whole of what
		// these specs drive.
		reflashPlan := func(planName, evtID string, fw *intelv1a1.FirmwareSpec) *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Firmware:         fw,
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{
						ID:           evtID,
						NodeName:     "node07",
						GPUBDF:       reflashBDF,
						RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
						Reason:       reasonSurvivability,
						State:        intelv1a1.RecoveryEventStateWaitingApproval,
						LastUpdated:  ptr.To(metav1.Now()),
					}},
				},
			}

			createPlanForOwnerRef(p)

			return p
		}

		containerFirmware := func() *intelv1a1.FirmwareSpec {
			return &intelv1a1.FirmwareSpec{
				Source: intelv1a1.FirmwareSource{
					ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: fwImage},
				},
				File: reflashFile,
			}
		}

		getJob := func(name string) *batch.Job {
			job := &batch.Job{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: name, Namespace: "default"}, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			return job
		}

		expectNoJob := func(name string) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &batch.Job{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "a parked reflash must not leave a Job behind")
		}

		It("should build the flash Job from the firmware-update template", func() {
			r := newTestReconciler()
			p := reflashPlan("plan-reflash-build", "evt-reflash-build", containerFirmware())
			p.Spec.XpuSmi = intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:v1", PullPolicy: "Always"}

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(evt.JobName).To(Equal("recovery-evt-reflash-build-0"))

			job := getJob(evt.JobName)
			Expect(job.Labels).To(HaveKeyWithValue(recoveryJobLabelPlan, "plan-reflash-build"))
			Expect(job.Labels).To(HaveKeyWithValue(recoveryJobLabelEvent, "evt-reflash-build"))
			Expect(job.Spec.Template.Spec.NodeName).To(Equal("node07"))

			// The firmware comes out of its own image, which is the initContainer's whole job.
			copyC := containerByName(job.Spec.Template.Spec.InitContainers, reflashCopyContainer)
			Expect(copyC).NotTo(BeNil())
			Expect(copyC.Image).To(Equal(fwImage))

			flashC := containerByName(job.Spec.Template.Spec.Containers, reflashJobContainer)
			Expect(flashC).NotTo(BeNil())
			Expect(flashC.Image).To(Equal("registry/xpu-smi:v1"))
			Expect(flashC.ImagePullPolicy).To(Equal(core.PullAlways))

			// The template runs /bin/sh -c, so the flash has to stay one command line: replacing the
			// command with an argv, as a reset does, would leave the shell nothing to run. The length
			// is the assertion that matters — sh -c takes its command from the first operand and turns
			// the rest into $0, $1, …, so a flash split across arguments runs a bare "xpu-smi" and
			// silently flashes nothing.
			Expect(flashC.Command).To(Equal([]string{"/bin/sh", "-c"}))
			Expect(flashC.Args).To(HaveLen(1))
			Expect(flashC.Args[0]).To(Equal(buildFDOFlashCommand(reflashBDF, reflashFile)))

			// An admin who has to check what was flashed onto which card reads this, not the pod log.
			Expect(p.Status.Messages).To(ContainElement(SatisfyAll(
				ContainSubstring(reflashBDF),
				ContainSubstring(reflashFile),
				ContainSubstring(evt.JobName),
			)))
		})

		// The two halves of the reflash are wired together by convention, not by anything either side
		// checks: the initContainer copies one directory into the staging volume, and xpu-smi is
		// handed a path under it. If the template's directories and the operator's constants drift
		// apart the Job still starts and fails minutes later, from inside a pod, on a missing file.
		It("should flash from the directory the initContainer stages into", func() {
			tmpl := deployments.XpuManagerFWUpdateJob()

			copyC := containerByName(tmpl.Spec.Template.Spec.InitContainers, reflashCopyContainer)
			Expect(copyC).NotTo(BeNil())
			Expect(copyC.Args).To(HaveLen(1))
			Expect(copyC.Args[0]).To(SatisfyAll(
				ContainSubstring(firmwareImageDir),
				ContainSubstring(reflashStagingDir),
			), "the copy must read where firmwareImagePath looks and write where the flash reads")

			// mountedAt returns the name of the volume the container sees at the given path, so the
			// two containers can be shown to be talking about the same emptyDir.
			mountedAt := func(c *core.Container, path string) string {
				for i := range c.VolumeMounts {
					if c.VolumeMounts[i].MountPath == path {
						return c.VolumeMounts[i].Name
					}
				}

				return ""
			}

			flashC := containerByName(tmpl.Spec.Template.Spec.Containers, reflashJobContainer)
			Expect(flashC).NotTo(BeNil())
			Expect(mountedAt(copyC, reflashStagingDir)).NotTo(BeEmpty())
			Expect(mountedAt(flashC, reflashStagingDir)).To(Equal(mountedAt(copyC, reflashStagingDir)))

			Expect(buildFDOFlashCommand(reflashBDF, reflashFile)).To(
				ContainSubstring(reflashStagingDir + "/" + reflashFile))
			Expect(firmwareImagePath(reflashFile)).To(Equal(firmwareImageDir + "/" + reflashFile))
		})

		// -y because there is no terminal to answer the prompt on, and --force because the card is in
		// survivability mode: xpu-smi otherwise declines to write an image it judges no newer than
		// what is on the device, and what is on the device is exactly what has to go.
		It("should force an unattended FDO flash", func() {
			Expect(buildFDOFlashCommand("0000:02:00.0", "fw.bin")).To(
				Equal("xpu-smi updatefw -d 0000:02:00.0 -t FDO -f /update/fw.bin -y --force"))
		})

		DescribeTable("should park in missing-firmware rather than build an unusable Job",
			func(planName, evtID string, fw *intelv1a1.FirmwareSpec, wantMsg string) {
				r := newTestReconciler()
				p := reflashPlan(planName, evtID, fw)

				// Not an error: nothing has gone wrong in the cluster, the plan is simply not
				// finished. Returning one would put the whole reconcile into backoff and pile the
				// same message into status.errors on every retry.
				Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

				evt := p.Status.Events[0]
				Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateMissingFirmware))
				Expect(evt.JobName).To(BeEmpty())
				Expect(evt.StateMessage).To(ContainSubstring(wantMsg))
				Expect(p.Status.Messages).To(ContainElement(ContainSubstring(wantMsg)))

				expectNoJob(recoveryJobName(evtID, 0))
			},
			Entry("no firmware at all", "plan-reflash-nofw", "evt-reflash-nofw",
				nil, "spec.firmware"),
			Entry("a source but no file", "plan-reflash-nofile", "evt-reflash-nofile",
				&intelv1a1.FirmwareSpec{
					Source: intelv1a1.FirmwareSource{
						ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: fwImage},
					},
				}, "spec.firmware"),
			// volumeSource is accepted by the CRD but not acted on: the reflash Job copies firmware
			// out of a container image. Parking says so rather than building a Job whose
			// initContainer would copy from an image that holds no firmware.
			Entry("a volume source, which is not implemented", "plan-reflash-vol", "evt-reflash-vol",
				&intelv1a1.FirmwareSpec{
					Source: intelv1a1.FirmwareSource{
						VolumeSource: &intelv1a1.VolumeFirmwareSource{Name: "fw-pvc"},
					},
					File: reflashFile,
				}, "containerSource"),
		)
	})
})
