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
