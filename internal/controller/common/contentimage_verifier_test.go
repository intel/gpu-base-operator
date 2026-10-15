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

package common

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("ContentImageVerifier", func() {
	Context("Docker auth config", func() {
		It("build key chain", func() {
			dverif := DefaultContentImageVerifier{
				k8sReader: k8sClient,
				namespace: "test-namespace",
			}

			ctx := context.Background()

			ns := &core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-namespace",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			secret := &core.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-pull-secret",
					Namespace: "test-namespace",
				},
				Data: map[string][]byte{
					".dockerconfigjson": []byte(`{
						"auths": {
							"my-registry.io": {
								"auth": "bXktdXNlcjpteS1wYXNz"
							}
						}
					}`),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			kc, err := dverif.buildKeychain(ctx, "my-pull-secret")
			Expect(err).To(Not(HaveOccurred()))
			Expect(kc).To(Not(BeNil()))
		})

		It("failure to build keychain without pullsecret", func() {
			dverif := DefaultContentImageVerifier{
				k8sReader: k8sClient,
				namespace: "test-namespace-not-existing",
			}

			ctx := context.Background()

			kc, err := dverif.buildKeychain(ctx, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(kc).To(Not(BeNil()))

			kc, err = dverif.buildKeychain(ctx, "my-pull-secret")
			Expect(err).To(HaveOccurred())
			Expect(kc).To(BeNil())
		})
	})

	Context("normalize image path", func() {
		It("should normalize image path", func() {
			Expect(normalizeImagePath("/fwupdate/file.bin")).To(Equal("fwupdate/file.bin"))
			Expect(normalizeImagePath("./fwupdate/file.bin")).To(Equal("fwupdate/file.bin"))
			Expect(normalizeImagePath("fwupdate/file.bin")).To(Equal("fwupdate/file.bin"))
		})
	})
})

var _ = Describe("verifyChecksumsFromExport", func() {
	// makeTar returns a tar stream containing the given files under "fwupdate/".
	makeTar := func(files map[string][]byte) *bytes.Buffer {
		buf := &bytes.Buffer{}
		tw := tar.NewWriter(buf)

		for name, content := range files {
			hdr := &tar.Header{
				Name: "fwupdate/" + name,
				Mode: 0o600,
				Size: int64(len(content)),
			}
			ExpectWithOffset(1, tw.WriteHeader(hdr)).To(Succeed())
			_, err := tw.Write(content)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		ExpectWithOffset(1, tw.Close()).To(Succeed())

		return buf
	}

	// sha256Of returns the "sha256:<hex>" checksum of the given data.
	sha256Of := func(data []byte) string {
		h := sha256.New()
		h.Write(data)
		return fmt.Sprintf("sha256:%x", h.Sum(nil))
	}

	It("passes when all declared checksums match", func() {
		content := []byte("firmware binary data")
		files := []ImageFile{
			{Name: "/fwupdate/gfx.bin", Checksum: sha256Of(content)},
		}

		err := verifyChecksumsFromExport(makeTar(map[string][]byte{"gfx.bin": content}), files)
		Expect(err).NotTo(HaveOccurred())
	})

	It("passes when some files have no checksum (those are skipped)", func() {
		content := []byte("firmware binary data")
		files := []ImageFile{
			{Name: "/fwupdate/gfx.bin", Checksum: sha256Of(content)},
			{Name: "/fwupdate/gfxdata.bin"}, // no checksum — skipped
		}
		tarFiles := map[string][]byte{
			"gfx.bin":     content,
			"gfxdata.bin": []byte("other data"),
		}

		err := verifyChecksumsFromExport(makeTar(tarFiles), files)
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns a mismatch error when checksum does not match", func() {
		files := []ImageFile{
			{Name: "/fwupdate/gfx.bin", Checksum: sha256Of([]byte("correct data"))},
		}

		err := verifyChecksumsFromExport(makeTar(map[string][]byte{"gfx.bin": []byte("wrong data")}), files)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("checksum mismatch"))
		Expect(err.Error()).To(ContainSubstring("gfx.bin"))
	})

	It("returns a distinct error when a declared file is absent from the image", func() {
		files := []ImageFile{
			{Name: "/fwupdate/missing.bin", Checksum: sha256Of([]byte("data"))},
		}

		err := verifyChecksumsFromExport(makeTar(map[string][]byte{"other.bin": []byte("data")}), files)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found"))
		Expect(err.Error()).To(ContainSubstring("missing.bin"))
	})

	It("ignores files outside the fwupdate/ directory", func() {
		content := []byte("firmware")
		files := []ImageFile{
			{Name: "/fwupdate/gfx.bin", Checksum: sha256Of(content)},
		}
		// Tar contains the file at the correct path AND a decoy at the wrong path.
		buf := &bytes.Buffer{}
		tw := tar.NewWriter(buf)
		for _, entry := range []struct {
			name string
			data []byte
		}{
			{"other/gfx.bin", []byte("wrong data at wrong path")},
			{"fwupdate/gfx.bin", content},
		} {
			hdr := &tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.data))}
			Expect(tw.WriteHeader(hdr)).To(Succeed())
			_, err := tw.Write(entry.data)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(tw.Close()).To(Succeed())

		err := verifyChecksumsFromExport(buf, files)
		Expect(err).NotTo(HaveOccurred())
	})
})
