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

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// validPlan returns a minimal valid GPURecoveryPlan for use in tests. Minimal includes
// defaultResetType: the platform's reset mechanism cannot be inferred, so the field is mandatory.
func validPlan() *GPURecoveryPlan {
	return &GPURecoveryPlan{
		Spec: GPURecoveryPlanSpec{
			DeviceID:         "0x1234",
			DefaultResetType: RecoveryTypeSlot,
		},
	}
}

var _ = Describe("GPURecoveryPlan Webhook", func() {
	var (
		obj       *GPURecoveryPlan
		oldObj    *GPURecoveryPlan
		validator GPURecoveryPlanCustomValidator
		defaulter GPURecoveryPlanCustomDefaulter
	)

	BeforeEach(func() {
		obj = validPlan()
		oldObj = validPlan()
		validator = GPURecoveryPlanCustomValidator{}
		defaulter = GPURecoveryPlanCustomDefaulter{}
		Expect(validator).NotTo(BeNil())
		Expect(defaulter).NotTo(BeNil())
	})

	// ── Defaulter ────────────────────────────────────────────────────────────────

	Context("Defaulting Webhook", func() {
		It("should generate IDs for approvals that have none", func() {
			obj.Spec.Approvals = []RecoveryApproval{
				{EventID: "evt-aabb"},
				{EventID: "evt-ccdd"},
			}

			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			Expect(obj.Spec.Approvals[0].ID).To(MatchRegexp(`^app-[0-9a-f]{8}$`))
			Expect(obj.Spec.Approvals[1].ID).To(MatchRegexp(`^app-[0-9a-f]{8}$`))
		})

		It("should not overwrite an existing approval ID", func() {
			obj.Spec.Approvals = []RecoveryApproval{
				{ID: "app-12345678", EventID: "evt-aabb"},
			}

			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Approvals[0].ID).To(Equal("app-12345678"))
		})

		It("should leave an empty approvals list unchanged", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Approvals).To(BeEmpty())
		})

		// defaultResetType is deliberately not defaulted: neither accepted value is safe to
		// assume, and a wrong guess is silent — the Job runs a reset the platform cannot perform
		// and exits 0. The validator rejects the omission instead.
		It("should not invent a defaultResetType", func() {
			obj.Spec.DefaultResetType = ""

			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.DefaultResetType).To(BeEmpty())
		})

		It("should not overwrite an explicit defaultResetType", func() {
			obj.Spec.DefaultResetType = RecoveryTypeAMC

			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.DefaultResetType).To(Equal(RecoveryTypeAMC))
		})

		It("should generate distinct IDs across multiple approvals", func() {
			obj.Spec.Approvals = make([]RecoveryApproval, 10)
			for i := range obj.Spec.Approvals {
				obj.Spec.Approvals[i] = RecoveryApproval{
					Selector: &ApprovalSelector{RecoveryType: RecoveryTypeSBR},
				}
			}

			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			seen := make(map[string]bool)
			for _, a := range obj.Spec.Approvals {
				Expect(a.ID).To(MatchRegexp(`^app-[0-9a-f]{8}$`))
				Expect(seen[a.ID]).To(BeFalse(), "duplicate ID generated: %s", a.ID)
				seen[a.ID] = true
			}
		})

		// The CRD defaults spec.xpuSmi to {pullPolicy: IfNotPresent}, which covers the field being
		// absent; this covers spec.xpuSmi being written with only an image in it, and the case
		// where webhooks are the only defaulting in play.
		It("should default xpuSmi.pullPolicy", func() {
			obj.Spec.XpuSmi = XpuSmiSpec{Image: "registry/xpu-smi:latest"}

			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.XpuSmi.PullPolicy).To(Equal("IfNotPresent"))
		})

		It("should not overwrite an explicit xpuSmi.pullPolicy", func() {
			obj.Spec.XpuSmi.PullPolicy = "Always"

			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.XpuSmi.PullPolicy).To(Equal("Always"))
		})
	})

	// ── Validator – ValidateCreate ────────────────────────────────────────────────

	Context("ValidateCreate", func() {
		It("should accept a minimal valid plan", func() {
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should reject a missing deviceId", func() {
			obj.Spec.DeviceID = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("deviceId"))
		})

		It("should reject an invalid deviceId format", func() {
			obj.Spec.DeviceID = "1234"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("deviceId"))
		})

		// The error has to name the choice, not just the schema: nothing on a node reports whether
		// its PCIe slots do hot-plug, so an admin hitting this needs to be told what to look at.
		It("should reject a missing defaultResetType and say how to pick one", func() {
			obj.Spec.DefaultResetType = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("defaultResetType"))
			Expect(err.Error()).To(ContainSubstring("hot-plug"))
		})

		// sbr and reflash are valid RecoveryTypes but not platform defaults: sbr is the per-card
		// backup and reflash is not a reset. As a cluster-wide default either would apply to every
		// wedged GPU the DRA driver reports.
		DescribeTable("should reject a defaultResetType that is not a platform reset",
			func(rt RecoveryType) {
				obj.Spec.DefaultResetType = rt
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("defaultResetType"))
			},
			Entry("sbr, the per-card backup", RecoveryTypeSBR),
			Entry("reflash, not a reset at all", RecoveryTypeReflash),
			Entry("a value outside the enum", RecoveryType("flr")),
		)

		It("should accept amc as a defaultResetType", func() {
			obj.Spec.DefaultResetType = RecoveryTypeAMC
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should reject an invalid subDeviceId format", func() {
			obj.Spec.SubDeviceID = "0xGGGG"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("subDeviceId"))
		})

		It("should reject an invalid subVendorId format", func() {
			obj.Spec.SubVendorID = "0xZZZZ"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("subVendorId"))
		})

		It("should accept valid optional PCI IDs", func() {
			obj.Spec.SubDeviceID = "0xabcd"
			obj.Spec.SubVendorID = "0xABCD"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("approvals validation", func() {
			It("should reject an approval with both eventId and selector", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{
						EventID:  "evt-aabb",
						Selector: &ApprovalSelector{RecoveryType: RecoveryTypeSBR},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
			})

			It("should reject an approval with neither eventId nor selector", func() {
				obj.Spec.Approvals = []RecoveryApproval{{Comment: "no target"}}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("one of eventId or selector must be set"))
			})

			It("should reject duplicate approval IDs", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{ID: "app-1234", EventID: "evt-aabb"},
					{ID: "app-1234", EventID: "evt-ccdd"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("duplicate approval ID"))
			})

			It("should reject persistent=true without a selector", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{EventID: "evt-aabb", Persistent: true},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("persistent"))
			})

			It("should accept a valid eventId approval", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{ID: "app-1234", EventID: "evt-aabb"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should accept a consumed eventId approval (audit trail entry)", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{ID: "app-1234", EventID: "evt-aabb", Consumed: true},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should accept a valid selector approval", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{
						Selector: &ApprovalSelector{RecoveryType: RecoveryTypeReflash},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should accept a persistent selector approval", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{
						Selector:   &ApprovalSelector{RecoveryType: RecoveryTypeSBR},
						Persistent: true,
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should accept a valid nodeSelector", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{
						Selector: &ApprovalSelector{
							NodeSelector: map[string]string{"rack": "rack-04-32", "gpu.intel.com/family": "bmg"},
						},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should reject a nodeSelector with an invalid label key", func() {
				// labels.SelectorFromSet does not validate, so an invalid key would
				// silently match no nodes and the approval would appear to be ignored.
				obj.Spec.Approvals = []RecoveryApproval{
					{
						Selector: &ApprovalSelector{
							NodeSelector: map[string]string{"not a valid key": "x"},
						},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nodeSelector"))
			})

			It("should reject a nodeSelector with an invalid label value", func() {
				obj.Spec.Approvals = []RecoveryApproval{
					{
						Selector: &ApprovalSelector{
							NodeSelector: map[string]string{"rack": "not a valid value"},
						},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nodeSelector"))
			})
		})

		// A namespace name that cannot exist makes the entry a silent no-op: the drain evicts the
		// pods the admin meant to protect and nothing in the CR says why. The CRD's item pattern
		// catches most of it, but the webhook is what produces a message naming the field and the
		// value, and it still runs where the CRD is applied by an older chart.
		Context("drain validation", func() {
			It("should accept a valid namespacesToSkip list", func() {
				obj.Spec.Drain.NamespacesToSkip = []string{"kube-system", "cert-manager"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should accept an empty namespacesToSkip list", func() {
				obj.Spec.Drain.NamespacesToSkip = nil
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should reject a namespace name that is not a DNS label", func() {
				obj.Spec.Drain.NamespacesToSkip = []string{"Kube System"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("namespacesToSkip[0]"))
			})

			It("should reject an empty namespace name", func() {
				obj.Spec.Drain.NamespacesToSkip = []string{"kube-system", ""}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("namespacesToSkip[1]"))
			})

			// A duplicate is harmless to the drain itself, but it is a sign the admin edited the
			// list by hand and meant to write two different namespaces.
			It("should reject a duplicate namespace", func() {
				obj.Spec.Drain.NamespacesToSkip = []string{"kube-system", "kube-system"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("duplicate namespace"))
			})
		})

		Context("firmware validation", func() {
			validFW := func() *FirmwareSpec {
				return &FirmwareSpec{
					Source: FirmwareSource{
						ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:latest"},
					},
					File: "gfx.bin",
				}
			}

			It("should accept a valid firmware spec", func() {
				obj.Spec.Firmware = validFW()
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should reject an invalid xpuSmi.image reference", func() {
				obj.Spec.XpuSmi.Image = "INVALID IMAGE::"
				obj.Spec.Firmware = validFW()
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("xpuSmi.image"))
			})

			It("should accept a valid xpuSmi.image", func() {
				obj.Spec.XpuSmi.Image = "registry/xpu-smi:latest"
				obj.Spec.Firmware = validFW()
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should reject missing source (no container and no volume)", func() {
				fw := validFW()
				fw.Source = FirmwareSource{}
				obj.Spec.Firmware = fw
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("source"))
			})

			It("should accept a volumeSource instead of containerSource", func() {
				fw := validFW()
				fw.Source = FirmwareSource{
					VolumeSource: &VolumeFirmwareSource{Name: "my-pvc"},
				}
				obj.Spec.Firmware = fw
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should reject an empty volumeSource name", func() {
				fw := validFW()
				fw.Source = FirmwareSource{
					VolumeSource: &VolumeFirmwareSource{Name: ""},
				}
				obj.Spec.Firmware = fw
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("volumeSource"))
			})

			It("should reject an empty file", func() {
				fw := validFW()
				fw.File = ""
				obj.Spec.Firmware = fw
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("file must not be empty"))
			})

			// The name is interpolated into the reflash Job's shell command unquoted, so the
			// next three entries are security checks, not tidiness: a path component escapes the
			// firmware mount and a shell metacharacter runs as root in a privileged container.
			DescribeTable("should reject an unsafe file name",
				func(name, wantMsg string) {
					fw := validFW()
					fw.File = name
					obj.Spec.Firmware = fw
					_, err := validator.ValidateCreate(ctx, obj)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring(wantMsg))
				},
				Entry("relative path escape", "../etc/passwd", "path components"),
				Entry("absolute path", "/etc/passwd", "path components"),
				Entry("subdirectory", "fw/gfx.bin", "path components"),
				Entry("space", "fw file.bin", "invalid characters"),
				Entry("command substitution", "gfx.bin$(id)", "invalid characters"),
				Entry("shell separator", "gfx.bin;rm", "invalid characters"),
			)
		})
	})

	// ── Validator – ValidateUpdate ────────────────────────────────────────────────

	Context("ValidateUpdate", func() {
		It("should accept a valid update with no active reflash events", func() {
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should reject an invalid spec on update", func() {
			obj.Spec.DeviceID = "bad"
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
		})

		It("should reject firmware changes while a reflash event is in-progress", func() {
			oldObj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v1"}},
				File:   "gfx.bin",
			}
			oldObj.Status.Events = []RecoveryEvent{
				{
					ID:           "evt-aabb",
					NodeName:     "node01",
					GPUBDF:       "0000:02:00.0",
					RecoveryType: RecoveryTypeSpec{Type: RecoveryTypeReflash},
					State:        RecoveryEventStateInProgress,
				},
			}

			obj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v2"}},
				File:   "gfx.bin",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("firmware"))
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})

		It("should allow firmware changes when reflash event is succeeded", func() {
			oldObj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v1"}},
				File:   "gfx.bin",
			}
			oldObj.Status.Events = []RecoveryEvent{
				{
					ID:           "evt-aabb",
					NodeName:     "node01",
					GPUBDF:       "0000:02:00.0",
					RecoveryType: RecoveryTypeSpec{Type: RecoveryTypeReflash},
					State:        RecoveryEventStateSucceeded,
				},
			}

			obj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v2"}},
				File:   "gfx.bin",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		// missing-firmware, and waiting-approval after a failed image check, are the two situations
		// in which the operator is asking the admin to change this very field. Rejecting the change
		// there is a deadlock, not protection: the state exists to request an edit, and the guard
		// forbade the edit.
		//
		// The nil -> set case below is the worst of them. It is the ordinary way a reflash gets
		// configured — the plan is written, a card drops into FDO mode, the event parks in
		// missing-firmware — and there was no way out of it short of deleting the event or the
		// whole plan, with the card unrecoverable in the meantime.
		It("should allow firmware to be set for the first time while an event is missing-firmware", func() {
			oldObj.Spec.Firmware = nil
			oldObj.Status.Events = []RecoveryEvent{
				{
					ID:           "evt-ccdd",
					NodeName:     "node02",
					GPUBDF:       "0000:03:00.0",
					RecoveryType: RecoveryTypeSpec{Type: RecoveryTypeReflash},
					State:        RecoveryEventStateMissingFirmware,
				},
			}

			obj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v1"}},
				File:   "gfx.bin",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"missing-firmware means the operator is waiting for this field; refusing to accept "+
					"it leaves the card unrecoverable with no diagnostic pointing at the webhook")
		})

		It("should allow firmware to be corrected while an event is missing-firmware", func() {
			oldObj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{VolumeSource: &VolumeFirmwareSource{Name: "fw-pvc"}},
				File:   "gfx.bin",
			}
			oldObj.Status.Events = []RecoveryEvent{
				{
					ID:           "evt-ccdd",
					NodeName:     "node02",
					GPUBDF:       "0000:03:00.0",
					RecoveryType: RecoveryTypeSpec{Type: RecoveryTypeReflash},
					State:        RecoveryEventStateMissingFirmware,
				},
			}

			obj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v1"}},
				File:   "gfx.bin",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"a volume-only source is one of the three things that park an event in "+
					"missing-firmware, so switching it to a container source has to be permitted")
		})

		// The state the user hit in a real cluster: the fwfiles image reference was wrong. The event
		// is sent back to waiting-approval with its approval retained, and the correction is the
		// only thing that makes the operator check the registry again — so accepting the edit is
		// the whole fix, and blocking it would make the admin delete and re-approve.
		It("should allow firmware to be corrected after an event failed image verification", func() {
			oldObj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/typo:v1"}},
				File:   "gfx.bin",
			}
			oldObj.Status.Events = []RecoveryEvent{
				{
					ID:                    "evt-ccdd",
					NodeName:              "node02",
					GPUBDF:                "0000:03:00.0",
					RecoveryType:          RecoveryTypeSpec{Type: RecoveryTypeReflash},
					State:                 RecoveryEventStateWaitingApproval,
					ApprovalID:            "apr-1",
					ImageVerifyGeneration: 4,
				},
			}

			obj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v1"}},
				File:   "gfx.bin",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"the image check reports an unpullable reference; the correction must be accepted")
		})

		// A blocked reflash is already approved and starts by itself once its node frees up, with
		// no further admin action. Editing the spec in that window would flash firmware nobody
		// approved — the same hazard as in-progress, just before the Job exists.
		It("should reject firmware changes while a reflash event is blocked", func() {
			oldObj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v1"}},
				File:   "gfx.bin",
			}
			oldObj.Status.Events = []RecoveryEvent{
				{
					ID:           "evt-eeff",
					NodeName:     "node03",
					GPUBDF:       "0000:04:00.0",
					RecoveryType: RecoveryTypeSpec{Type: RecoveryTypeReflash},
					State:        RecoveryEventStateBlocked,
				},
			}

			obj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v2"}},
				File:   "gfx.bin",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})

		// A blocked *reset* says nothing about firmware. Blocking edits on it would freeze the
		// field for the duration of an unrelated queue.
		It("should allow firmware changes while a blocked event is a reset", func() {
			oldObj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v1"}},
				File:   "gfx.bin",
			}
			oldObj.Status.Events = []RecoveryEvent{
				{
					ID:           "evt-1122",
					NodeName:     "node04",
					GPUBDF:       "0000:05:00.0",
					RecoveryType: RecoveryTypeSpec{Type: RecoveryTypeSBR},
					State:        RecoveryEventStateBlocked,
				},
			}

			obj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v2"}},
				File:   "gfx.bin",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should allow a firmware change when no reflash events exist", func() {
			oldObj.Status.Events = []RecoveryEvent{
				{
					ID:           "evt-aabb",
					NodeName:     "node01",
					GPUBDF:       "0000:02:00.0",
					RecoveryType: RecoveryTypeSpec{Type: RecoveryTypeSBR},
					State:        RecoveryEventStateInProgress,
				},
			}

			obj.Spec.Firmware = &FirmwareSpec{
				Source: FirmwareSource{ContainerSource: &ContainerFirmwareSource{Name: "registry/fw:v2"}},
				File:   "gfx.bin",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ── Validator – ValidateDelete ────────────────────────────────────────────────

	Context("ValidateDelete", func() {
		It("should always allow deletion", func() {
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
