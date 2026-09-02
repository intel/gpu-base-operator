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

const (
	// DRA's device attributes
	// deviceAttrDeviceID is the ResourceSlice device attribute name for the PCI device ID.
	deviceAttrDeviceID = "pciId"

	// deviceAttrBDF is the ResourceSlice device attribute name for the PCI Bus:Device.Function address.
	deviceAttrBDF = "pciAddress"

	// DRA's device taint keys
	// deviceTaintKeyReset is the device taint key applied by the DRA driver when a GPU requires a
	// hardware reset.
	deviceTaintKeyReset = "health-xpumd-gpu.wedged"

	// deviceTaintKeyReflash is the device taint key the DRA driver applies when a GPU was already
	// in survivability mode as the driver enumerated it.
	deviceTaintKeyReflash = "health-Survivability"

	// deviceTaintKeyXpumdReflash is the device taint key the DRA driver applies on xpumd's behalf
	// when a GPU falls into survivability mode at runtime.
	deviceTaintKeyXpumdReflash = "health-xpumd-gpu.survivability"

	// reasonWedged and reasonSurvivability are the human-readable causes recorded in
	// status.events[].reason, derived from the device taint key that triggered the event.
	reasonWedged        = "gpu-wedged"
	reasonSurvivability = "survivability-mode"

	// maxStatusMessages is the maximum number of entries kept in status.messages.
	maxStatusMessages = 50

	// maxStatusEvents is the maximum number of entries kept in status.events.
	maxStatusEvents = 1000

	// maxStateMessageLen caps status.events[].stateMessage.
	maxStateMessageLen = 200

	// maxRecoveryNameLen is the hard ceiling on a recovery Job name, and therefore on the
	// event ID it is built from.
	//
	// It is 63 rather than the 253 metadata.name would allow: the Job controller copies the Job
	// name into spec.template.labels as batch.kubernetes.io/job-name, and label values are capped
	// at 63 bytes. metadata.name is also the stricter character set (DNS-1123 subdomain), so that
	// is what generateEventID validates against.
	maxRecoveryNameLen = 63

	// nodeSegmentMax is the budget for the node-name segment inside an event ID, sized so the
	// longest Job name that can be built from it still fits maxRecoveryNameLen. Summing the
	// worst case of every part:
	//
	//	"recovery-"      9
	//	"evt-"           4
	//	<node>          26  <- nodeSegmentMax
	//	"-reflash"       8  longest recovery type
	//	"-0001-02-00-0" 13  BDF that kept a non-zero PCI domain
	//	"-99"            3  two-digit attempt index
	//	                --
	//	                61
	//
	nodeSegmentMax = 26

	// idHashLen is the number of hex characters of SHA-256 used when a segment has to be shortened
	// or an assembled ID has to be replaced.
	idHashLen = 6
)
