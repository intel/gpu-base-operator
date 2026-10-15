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

// GPU identifiers shared by every controller: the extended resources the device plugin
// registers, the DRA driver's device class, and the device attributes and taints its
// ResourceSlices carry.
const (
	XeResource   = "gpu.intel.com/xe"
	I915Resource = "gpu.intel.com/i915"

	// GPUDeviceClass is both the DRA driver name and the name of its DeviceClass.
	GPUDeviceClass = "gpu.intel.com"

	// DeviceAttrDeviceID is the ResourceSlice device attribute name for the PCI device ID.
	DeviceAttrDeviceID = "pciId"

	// DeviceAttrBDF is the ResourceSlice device attribute name for the PCI Bus:Device.Function address.
	DeviceAttrBDF = "pciAddress"

	// DeviceTaintKeyReset is the device taint key applied by the DRA driver when a GPU requires a
	// hardware reset.
	DeviceTaintKeyReset = "health-xpumd-gpu.wedged"

	// DeviceTaintKeyReflash is the device taint key the DRA driver applies when a GPU was already
	// in survivability mode as the driver enumerated it.
	DeviceTaintKeyReflash = "health-Survivability"

	// DeviceTaintKeyXpumdReflash is the device taint key the DRA driver applies on xpumd's behalf
	// when a GPU falls into survivability mode at runtime.
	DeviceTaintKeyXpumdReflash = "health-xpumd-gpu.survivability"
)

// DeviceRecovery is the kind of recovery a device taint calls for.
type DeviceRecovery int

const (
	DeviceRecoveryNone DeviceRecovery = iota
	DeviceRecoveryReset
	DeviceRecoveryReflash
)

// RecoveryForDeviceTaint maps a DRA device taint key to the recovery it calls for.
// DeviceRecoveryNone means the taint is not a recovery-related one.
func RecoveryForDeviceTaint(taintKey string) DeviceRecovery {
	switch taintKey {
	case DeviceTaintKeyReset:
		return DeviceRecoveryReset
	case DeviceTaintKeyXpumdReflash, DeviceTaintKeyReflash:
		return DeviceRecoveryReflash
	default:
		return DeviceRecoveryNone
	}
}
