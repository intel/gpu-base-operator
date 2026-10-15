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

package common

import (
	stderrors "errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RequeueReconcileErr", func() {
	cause := stderrors.New("waiting for resource claims to drain")

	Context("sentinel matching", func() {
		It("matches a bare requeue request", func() {
			Expect(stderrors.Is(error(RequeueReconcileErr{}), RequeueReconcileErr{})).To(BeTrue())
		})

		It("matches a requeue request carrying a cause", func() {
			// Without the Is method errors.Is falls back to == against the zero value,
			// so this would be misreported as a reconcile failure.
			Expect(stderrors.Is(error(RequeueReconcileErr{Err: cause}), RequeueReconcileErr{})).To(BeTrue())
		})

		It("matches through an additional wrap", func() {
			err := fmt.Errorf("dra sub-controller: %w", RequeueReconcileErr{Err: cause})
			Expect(stderrors.Is(err, RequeueReconcileErr{})).To(BeTrue())
		})

		It("does not match an unrelated error", func() {
			Expect(stderrors.Is(cause, RequeueReconcileErr{})).To(BeFalse())
		})
	})

	Context("Error", func() {
		It("describes a bare requeue request without dereferencing the nil cause", func() {
			err := error(RequeueReconcileErr{})

			Expect(err.Error()).NotTo(BeEmpty())
			// fmt recovers a panicking Error method and renders a PANIC marker instead
			// of crashing, so assert on the formatted output rather than on a panic.
			Expect(fmt.Sprintf("%v", err)).NotTo(ContainSubstring("PANIC"))
		})

		It("reports the cause when one was wrapped", func() {
			Expect(error(RequeueReconcileErr{Err: cause}).Error()).To(Equal(cause.Error()))
		})
	})

	Context("Unwrap", func() {
		It("returns nil for a bare requeue request", func() {
			Expect(stderrors.Unwrap(error(RequeueReconcileErr{}))).To(Succeed())
		})

		It("exposes the cause to errors.As", func() {
			var requeueErr RequeueReconcileErr

			err := fmt.Errorf("dra sub-controller: %w", RequeueReconcileErr{Err: cause})

			Expect(stderrors.As(err, &requeueErr)).To(BeTrue())
			Expect(requeueErr.Unwrap()).To(Equal(cause))
		})
	})
})
