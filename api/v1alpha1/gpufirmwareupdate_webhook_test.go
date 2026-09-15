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

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("GPUFirmwareUpdate Webhook", func() {
	var (
		obj       *GPUFirmwareUpdate
		oldObj    *GPUFirmwareUpdate
		validator GPUFirmwareUpdateCustomValidator
	)

	const (
		updaterImageName = "registry/updaterimage:latest"
		oldImageName     = "someoldimage"
		newImageName     = "somenewimage"
	)

	BeforeEach(func() {
		obj = &GPUFirmwareUpdate{}
		oldObj = &GPUFirmwareUpdate{}
		validator = GPUFirmwareUpdateCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
	})

	Context("When creating or updating GPUFirmwareUpdate under Validating Webhook", func() {
		It("should accept create with ok inputs", func() {
			By("simulating a valid creation scenario")
			obj = &GPUFirmwareUpdate{
				Spec: GPUFirmwareUpdateSpec{
					UpdaterImage: oldImageName,
					UpdateMethod: "canary",
					Content: GPUFirmwareContent{
						ContainerImage: updaterImageName,
						Files: []GPUFirmwareFile{
							{
								Type:     "GFX",
								FileName: "someimage.bin",
							},
						},
					},
					PCIDeviceID: "0x1234",
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())

			obj.Spec.UpdaterImage = "foo/bar:latest@sha256:dc3ffc4fae44e7fa710ac0c9f6e1531ccd9dba3bc21551feaf23efee0b0bba2f"
			_, err = validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())

			obj.Spec.UpdaterImage = "foo/bar@sha256:dc3ffc4fae44e7fa710ac0c9f6e1531ccd9dba3bc21551feaf23efee0b0bba2f"
			_, err = validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should reject create with invalid sha checksum format", func() {
			By("simulating an invalid creation scenario with bad checksum format")
			obj = &GPUFirmwareUpdate{
				Spec: GPUFirmwareUpdateSpec{
					UpdaterImage: oldImageName,
					UpdateMethod: "canary",
					Content: GPUFirmwareContent{
						ContainerImage: updaterImageName,
						Files: []GPUFirmwareFile{
							{
								Type:     "GFX",
								FileName: "someimage.bin",
								Checksum: "md5:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
							},
						},
					},
					PCIDeviceID: "0x1234",
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported checksum format"))

			obj.Spec.Content.Files[0].Checksum = "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcde" // 63 chars instead of 64
			_, err = validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid sha256 checksum length"))
		})

		It("should reject create with duplicate firmware file entries", func() {
			By("simulating an invalid creation scenario with duplicate firmware file entries")
			obj = &GPUFirmwareUpdate{
				Spec: GPUFirmwareUpdateSpec{
					UpdaterImage: oldImageName,
					UpdateMethod: "canary",
					Content: GPUFirmwareContent{
						ContainerImage: updaterImageName,
						Files: []GPUFirmwareFile{
							{
								Type:     "GFX",
								FileName: "someimage.bin",
								Checksum: "md5:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
							},
							{
								Type:     "GFX",
								FileName: "someimage.bin",
								Checksum: "md5:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
							},
						},
					},
					PCIDeviceID: "0x1234",
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate firmware file entry"))
		})

		It("should accept checksum on a digest-pinned content image", func() {
			By("setting a checksum with a digest-pinned content image")
			obj = &GPUFirmwareUpdate{
				Spec: GPUFirmwareUpdateSpec{
					UpdaterImage: updaterImageName,
					UpdateMethod: "direct",
					Content: GPUFirmwareContent{
						ContainerImage: "registry/fwfiles@sha256:dc3ffc4fae44e7fa710ac0c9f6e1531ccd9dba3bc21551feaf23efee0b0bba2f",
						Files: []GPUFirmwareFile{
							{
								Type:     "GFX",
								FileName: "gfx.bin",
								Checksum: "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
							},
						},
					},
					PCIDeviceID: "0x1234",
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should reject checksum when content image is not digest-pinned", func() {
			By("setting a checksum with a tag-only content image")
			obj = &GPUFirmwareUpdate{
				Spec: GPUFirmwareUpdateSpec{
					UpdaterImage: updaterImageName,
					UpdateMethod: "direct",
					Content: GPUFirmwareContent{
						ContainerImage: "registry/fwfiles:v1.0",
						Files: []GPUFirmwareFile{
							{
								Type:     "GFX",
								FileName: "gfx.bin",
								Checksum: "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
							},
						},
					},
					PCIDeviceID: "0x1234",
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("digest-pinned"))
		})

		It("should accept files without checksum on a tag-only content image", func() {
			By("not setting checksums, so no digest requirement")
			obj = &GPUFirmwareUpdate{
				Spec: GPUFirmwareUpdateSpec{
					UpdaterImage: updaterImageName,
					UpdateMethod: "direct",
					Content: GPUFirmwareContent{
						ContainerImage: "registry/fwfiles:v1.0",
						Files: []GPUFirmwareFile{
							{Type: "GFX", FileName: "gfx.bin"},
							{Type: "GFX_DATA", FileName: "gfxdata.bin"},
						},
					},
					PCIDeviceID: "0x1234",
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
		It("should reject create with bad input", func() {
			By("simulating an invalid creation scenario")
			obj = &GPUFirmwareUpdate{
				Spec: GPUFirmwareUpdateSpec{
					UpdaterImage: oldImageName,
					UpdateMethod: "canary",
					Content: GPUFirmwareContent{
						ContainerImage: updaterImageName,
						Files: []GPUFirmwareFile{
							{
								Type:     "GFX",
								FileName: "someimage.bin",
							},
						},
					},
					PCIDeviceID: "0x1234",
				},
			}

			filenames := []string{
				"../someimage.bin",
				"some/dir/someimage.bin",
				"someimage.bin;rm -rf /",
				"someimage.bin|rm -rf /",
				"someimage.bin&&rm -rf /",
				"someimage.bin$(echo foo)",
				"someimage.bin`echo foo`",
				// filepath.Base leaves "." and ".." unchanged, so the path-component
				// check above does not reject them - the character allow-list must.
				".",
				"..",
				".hidden.bin",
				"-rf",
			}

			for _, fname := range filenames {
				obj.Spec.Content.Files[0].FileName = fname

				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
			}

			obj.Spec.Content.Files[0].FileName = "someimage.bin"

			pciIds := []string{
				"1234",
				"0x12G4",
				"0x123",
				"0x12345",
				// Lower-case only, matching every other PCI ID field in the API.
				"0x12AB",
			}

			for _, pciId := range pciIds {
				obj.Spec.PCIDeviceID = pciId

				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
			}

			obj.Spec.PCIDeviceID = "0x1234"

			updaterImages := []string{
				"foo/bar:latest@sha256:abcdef",
				"foo/bar@sha256:abcdef",
				"foo/bar:latest:extra",
				"foo/bar:latest@sha256:abcdef:extra",
				"",
			}

			for _, updaterImage := range updaterImages {
				obj.Spec.UpdaterImage = updaterImage

				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
			}

			obj.Spec.UpdaterImage = updaterImageName

			for _, contentImage := range updaterImages {
				obj.Spec.Content.ContainerImage = contentImage

				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
			}
		})

		It("should always accept delete", func() {
			By("simulating a valid deletion scenario")
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should validate updates correctly", func() {
			By("simulating a valid update scenario")
			oldObj.Status.State = "" // Not started state
			oldObj.Spec = GPUFirmwareUpdateSpec{
				UpdaterImage: oldImageName,
				UpdateMethod: "canary",
				Content: GPUFirmwareContent{
					ContainerImage: updaterImageName,
					Files:          []GPUFirmwareFile{{Type: "GFX", FileName: "gfx.bin"}},
				},
				PCIDeviceID: "0x1234",
			}
			obj.Spec = oldObj.Spec
			obj.Spec.UpdaterImage = newImageName
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should validate inputs on updates outside in-progress states", func() {
			By("rejecting shell metacharacters in a firmware filename")
			obj.Spec = GPUFirmwareUpdateSpec{
				UpdaterImage: updaterImageName,
				UpdateMethod: "direct",
				Content: GPUFirmwareContent{
					ContainerImage: updaterImageName,
					Files:          []GPUFirmwareFile{{Type: "GFX", FileName: `gfx.bin";echo pwned;"`}},
				},
				PCIDeviceID: "0x1234",
			}

			for _, state := range []string{"", "completed", "error"} {
				oldObj.Status.State = state
				_, err := validator.ValidateUpdate(ctx, oldObj, obj)
				Expect(err).To(HaveOccurred(), "state %q", state)
				Expect(err.Error()).To(ContainSubstring("invalid firmware filename"), "state %q", state)
			}
		})

		// The controller removes its finalizer with a plain Update, which this
		// webhook sees. If input validation ran then, a CR whose spec no longer
		// passes current validation could never be deleted.
		It("Should not block a deletion in progress on an invalid spec", func() {
			now := metav1.Now()
			obj.DeletionTimestamp = &now
			obj.Finalizers = []string{"gpufirmwareupdate.intel.com/finalizer"}
			obj.Spec = GPUFirmwareUpdateSpec{
				UpdaterImage: updaterImageName,
				UpdateMethod: "direct",
				Content: GPUFirmwareContent{
					ContainerImage: updaterImageName,
					Files:          []GPUFirmwareFile{{Type: "GFX", FileName: `gfx.bin";echo pwned;"`}},
				},
				PCIDeviceID: "0x1234",
			}
			oldObj.Status.State = ""
			oldObj.Spec = *obj.Spec.DeepCopy()

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should validate ok if no changes to critical fields", func() {
			By("simulating a valid update scenario")
			oldObj.Status.State = "updating"
			oldObj.Spec.PCIDeviceID = "0x1234"
			oldObj.Spec.UpdaterImage = oldImageName
			oldObj.Spec.Content.ContainerImage = "someimage:latest"
			oldObj.Spec.Content.Files = []GPUFirmwareFile{
				{Type: "GFX", FileName: "gfx.bin"},
			}

			obj = oldObj.DeepCopy()

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should prevent update on in-progress states", func() {
			By("simulating an update scenario where the update is in progress")
			oldObj.Status.State = "updating"
			oldObj.Spec.UpdaterImage = oldImageName
			obj.Spec.UpdaterImage = newImageName
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
		})
	})
})
