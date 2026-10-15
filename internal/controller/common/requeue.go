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

// RequeueReconcileErr asks the caller for another reconcile pass without treating it as a
// failure. Err is the optional cause, reported in the logs.
type RequeueReconcileErr struct {
	Err error
}

// Is makes errors.Is(err, RequeueReconcileErr{}) match any RequeueReconcileErr, not just
// the zero value. Without it errors.Is falls back to == comparison, so a
// RequeueReconcileErr wrapping a real error would never be recognised as a requeue
// request and would be reported as a reconcile failure instead.
func (RequeueReconcileErr) Is(target error) bool {
	_, ok := target.(RequeueReconcileErr)

	return ok
}

// Error tolerates a nil wrapped error. Sub-controllers return a bare RequeueReconcileErr{}
// when they only need another pass and have no failure to report.
func (e RequeueReconcileErr) Error() string {
	if e.Err == nil {
		return "sub-controller requested a requeue"
	}

	return e.Err.Error()
}

// Unwrap exposes the wrapped cause to errors.As and errors.Unwrap. A bare requeue request
// returns nil, which simply terminates the chain.
func (e RequeueReconcileErr) Unwrap() error {
	return e.Err
}
