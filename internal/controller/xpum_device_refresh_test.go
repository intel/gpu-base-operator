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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
)

var _ = Describe("XPU Manager device refresh", func() {
	ctx := context.Background()

	const (
		refreshNode = "xpum-refresh-node"

		// A namespace of its own, labelled for admin access. The label is not test scaffolding:
		// the API server refuses adminAccess requests and allocations in a namespace without it,
		// so the operator's own namespace needs it for the monitoring claim to be allocated at all.
		refreshNS = "xpum-refresh"
		devA      = "0000-04-00-0-0xe20b"
		devB      = "0000-05-00-0-0xe20b"
		devC      = "0000-06-00-0-0xe20b"
		claimName = "xpum-refresh-claim"
	)

	// publishedDevice describes one device to publish: its name, the taint key it carries if any,
	// and the kernel driver the DRA driver reports it bound to (defaulting to xe).
	//
	// unbound publishes the driver attribute as the empty string, which is how a device with no KMD
	// bound appears — the state a reflash, a driver reload and a passthrough switch all pass through.
	type publishedDevice struct {
		name    string
		taint   string
		driver  string
		unbound bool
	}

	BeforeEach(func() {
		ns := &core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   refreshNS,
				Labels: map[string]string{"resource.kubernetes.io/admin-access": "true"},
			},
		}

		// Namespaces are never torn down: envtest has no namespace controller, so a deleted one
		// stays Terminating and refuses new objects for the rest of the suite.
		if err := k8sClient.Create(ctx, ns); err != nil {
			Expect(errors.IsAlreadyExists(err)).To(BeTrue(), "unexpected error creating namespace: %v", err)
		}

		// Sweep leftover xpum pods before every spec. Without a kubelet nothing finishes a
		// graceful deletion, so a pod this suite deleted — or one the reconciler restarted —
		// stays Terminating forever, and restartingXpumPods counts it against the
		// concurrency cap. Three leaks and no spec could ever observe a restart again.
		Expect(k8sClient.DeleteAllOf(ctx, &core.Pod{},
			client.InNamespace(refreshNS),
			client.MatchingLabels{xpuLabel: xpuValue},
			client.GracePeriodSeconds(0))).To(Succeed())
	})

	newRefreshReconciler := func() *XpumDeviceRefreshReconciler {
		return &XpumDeviceRefreshReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			lastRestart:     map[string]time.Time{},
			restartAttempts: map[string]int{},
			lostDevices:     map[string]map[string]bool{},
			Opts: ControllerOpts{
				Namespace:    refreshNS,
				RequeueDelay: 2 * time.Second,
				DRAEnable:    true,
			},
		}
	}

	// makePolicy creates the single ClusterPolicy the reconciler reads its mode from. Any policy
	// left over from another spec is removed first: restartMode takes the first item of the list,
	// so a leak would silently decide these specs' behaviour.
	makePolicy := func(mode v1alpha.XpumRestartMode, registration string) {
		existing := &v1alpha.ClusterPolicyList{}
		Expect(k8sClient.List(ctx, existing)).To(Succeed())

		for i := range existing.Items {
			Expect(k8sClient.Delete(ctx, &existing.Items[i])).To(Succeed())
		}

		cp := &v1alpha.ClusterPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "xpum-refresh-policy"},
			Spec: v1alpha.ClusterPolicySpec{
				ResourceRegistration: registration,
				ResourceMonitoring:   true,
				XpuManagerSpec: v1alpha.XpuManagerSpec{
					Image:                   "xpumd:test",
					RestartOnDeviceRecovery: mode,
				},
			},
		}
		Expect(k8sClient.Create(ctx, cp)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, cp)
		})
	}

	// publishSlice publishes the node's GPUs, replacing whatever was there before, so a spec can
	// move a device in and out of a recovery taint the way the driver does.
	publishSlice := func(devices ...publishedDevice) {
		name := "slice-" + refreshNode
		key := types.NamespacedName{Name: name}

		slice := &resv1.ResourceSlice{}
		if err := k8sClient.Get(ctx, key, slice); err == nil {
			Expect(k8sClient.Delete(ctx, slice)).To(Succeed())
		}

		devs := make([]resv1.Device, 0, len(devices))

		for _, d := range devices {
			driver := d.driver
			if driver == "" && !d.unbound {
				driver = "xe"
			}

			dev := resv1.Device{
				Name: d.name,
				Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
					deviceAttrDeviceID: {StringValue: ptr.To("0x1234")},
					deviceAttrBDF:      {StringValue: ptr.To("0000:04:00.0")},
					deviceAttrDriver:   {StringValue: ptr.To(driver)},
				},
			}

			if d.taint != "" {
				dev.Taints = []resv1.DeviceTaint{
					{Key: d.taint, Effect: resv1.DeviceTaintEffectNoExecute},
				}
			}

			devs = append(devs, dev)
		}

		fresh := &resv1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: resv1.ResourceSliceSpec{
				Driver:   gpuDeviceClass,
				NodeName: ptr.To(refreshNode),
				Pool:     resv1.ResourcePool{Name: "pool-" + refreshNode, ResourceSliceCount: 1},
				Devices:  devs,
			},
		}
		Expect(k8sClient.Create(ctx, fresh)).To(Succeed())
		DeferCleanup(func() {
			stale := &resv1.ResourceSlice{}
			if err := k8sClient.Get(ctx, key, stale); err == nil {
				_ = k8sClient.Delete(ctx, stale)
			}
		})
	}

	// makeClaim creates the pod's monitoring claim, allocated to the named devices.
	//
	// Both the request and the allocation results carry adminAccess, as the real monitoring claim's
	// do: counting admin-access results is the one thing this controller has to do differently from
	// claimHoldsDevice, which skips them.
	makeClaim := func(name string, allocated ...string) {
		claim := &resv1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: refreshNS},
			Spec: resv1.ResourceClaimSpec{
				Devices: resv1.DeviceClaim{
					Requests: []resv1.DeviceRequest{{
						Name: "gpu",
						Exactly: &resv1.ExactDeviceRequest{
							DeviceClassName: gpuDeviceClass,
							AdminAccess:     ptr.To(true),
							AllocationMode:  resv1.DeviceAllocationModeAll,
						},
					}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
		DeferCleanup(func() {
			fresh := &resv1.ResourceClaim{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: refreshNS}, fresh); err == nil {
				fresh.Status = resv1.ResourceClaimStatus{}
				_ = k8sClient.Status().Update(ctx, fresh)
				_ = k8sClient.Delete(ctx, fresh)
			}
		})

		if len(allocated) == 0 {
			return
		}

		results := make([]resv1.DeviceRequestAllocationResult, 0, len(allocated))

		for _, dev := range allocated {
			results = append(results, resv1.DeviceRequestAllocationResult{
				Request:     "gpu",
				Driver:      gpuDeviceClass,
				Pool:        "pool-" + refreshNode,
				Device:      dev,
				AdminAccess: ptr.To(true),
			})
		}

		claim.Status = resv1.ResourceClaimStatus{
			Allocation: &resv1.AllocationResult{
				Devices: resv1.DeviceAllocationResult{Results: results},
			},
		}
		Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())
	}

	// makeXpumPod creates an xpum DaemonSet-style pod on the node. A nil record leaves the pod
	// unadopted, which is what the adoption specs need; a pointer to the empty string is a pod that
	// was adopted having been given no usable GPUs at all, which is a different thing entirely.
	makeXpumPod := func(name string, phase core.PodPhase, record *string, claim string) *core.Pod {
		pod := &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: refreshNS,
				Labels:    map[string]string{xpuLabel: xpuValue},
			},
			Spec: core.PodSpec{
				NodeName: refreshNode,
				Containers: []core.Container{{
					Name:  xpumdContainerName,
					Image: "xpumd:test",
					Resources: core.ResourceRequirements{
						Claims: []core.ResourceClaim{{Name: monClaim}},
					},
				}},
				ResourceClaims: []core.PodResourceClaim{{
					Name:              monClaim,
					ResourceClaimName: ptr.To(claim),
				}},
			},
		}

		if record != nil {
			pod.Annotations = map[string]string{xpumDevicesAnnotation: *record}
		}

		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			fresh := &core.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: refreshNS}, fresh); err == nil {
				_ = k8sClient.Delete(ctx, fresh, client.GracePeriodSeconds(0))
			}
		})

		pod.Status.Phase = phase
		pod.Status.ResourceClaimStatuses = []core.PodResourceClaimStatus{{
			Name:              monClaim,
			ResourceClaimName: ptr.To(claim),
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		return pod
	}

	reconcileNode := func(r *XpumDeviceRefreshReconciler) reconcile.Result {
		res, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: refreshNode},
		})
		Expect(err).NotTo(HaveOccurred())

		return res
	}

	deviceRecord := func(name string) (string, bool) {
		pod := &core.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: refreshNS}, pod); err != nil {
			Expect(errors.IsNotFound(err)).To(BeTrue(), "unexpected error reading pod: %v", err)

			return "", false
		}

		value, ok := pod.Annotations[xpumDevicesAnnotation]

		return value, ok
	}

	podExists := func(name string) bool {
		pod := &core.Pod{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: refreshNS}, pod)

		return err == nil && pod.DeletionTimestamp == nil
	}

	Context("adoption", func() {
		It("records what the container was given and does not restart it", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devB})
			makeClaim(claimName, devA, devB)
			makeXpumPod("xpum-adopt", core.PodRunning, nil, claimName)

			reconcileNode(newRefreshReconciler())

			record, ok := deviceRecord("xpum-adopt")
			Expect(ok).To(BeTrue())
			Expect(record).To(Equal(devA + "," + devB))
			Expect(podExists("xpum-adopt")).To(BeTrue(), "adoption must never restart a pod")
		})

		It("leaves out an allocated device that is tainted for recovery", func() {
			// The monitoring claim has no selectors and tolerates every device taint, so a card in
			// survivability mode is allocated to the pod. Allocation is not usability.
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			publishSlice(
				publishedDevice{name: devA},
				publishedDevice{name: devB, taint: "health-Survivability"},
			)
			makeClaim(claimName, devA, devB)
			makeXpumPod("xpum-adopt-tainted", core.PodRunning, nil, claimName)

			reconcileNode(newRefreshReconciler())

			record, _ := deviceRecord("xpum-adopt-tainted")
			Expect(record).To(Equal(devA))
			Expect(podExists("xpum-adopt-tainted")).To(BeTrue())
		})

		It("leaves out a device bound away from the graphics drivers", func() {
			// A card handed to vfio-pci has no DRM node for anyone to monitor: it is not a device
			// the container is missing, and no restart could deliver it.
			makePolicy(v1alpha.XpumRestartAlways, resourceModeDRA)
			publishSlice(
				publishedDevice{name: devA},
				publishedDevice{name: devB, driver: "vfio-pci"},
			)
			makeClaim(claimName, devA, devB)
			makeXpumPod("xpum-adopt-vfio", core.PodRunning, nil, claimName)

			r := newRefreshReconciler()
			reconcileNode(r)

			record, _ := deviceRecord("xpum-adopt-vfio")
			Expect(record).To(Equal(devA))

			By("reconciling again now that the record is in place")
			reconcileNode(r)

			Expect(podExists("xpum-adopt-vfio")).To(BeTrue(),
				"a passthrough device must not restart the pod once per pass forever")
		})

		It("restarts on the next pass for a device the pod started before", func() {
			// The pod won the race against the DRA driver: the device is published now but was not
			// in the pod's allocation, so its container was never given a node for it.
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devB})
			makeClaim(claimName, devA)
			makeXpumPod("xpum-adopt-late", core.PodRunning, nil, claimName)

			r := newRefreshReconciler()
			reconcileNode(r)

			record, _ := deviceRecord("xpum-adopt-late")
			Expect(record).To(Equal(devA))
			Expect(podExists("xpum-adopt-late")).To(BeTrue(), "adoption itself never restarts")

			reconcileNode(r)

			Expect(podExists("xpum-adopt-late")).To(BeFalse())
		})

		It("records an allocated device that is absent from the slices as held", func() {
			// The allocation is evidence the device was there when the container was created, which
			// is when the injection happened. A publishing gap since then says nothing about what is
			// in the container, and recording it as missing would restart the pod the moment the
			// slice came back.
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			publishSlice(publishedDevice{name: devA})
			makeClaim(claimName, devA, devB)
			makeXpumPod("xpum-adopt-gap", core.PodRunning, nil, claimName)

			r := newRefreshReconciler()
			reconcileNode(r)

			record, _ := deviceRecord("xpum-adopt-gap")
			Expect(record).To(Equal(devA + "," + devB))

			By("the missing slice coming back")
			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devB})
			reconcileNode(r)

			Expect(podExists("xpum-adopt-gap")).To(BeTrue())
		})

		It("waits rather than guessing when the claim is not allocated yet", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			publishSlice(publishedDevice{name: devA})
			makeClaim(claimName)
			makeXpumPod("xpum-adopt-unallocated", core.PodRunning, nil, claimName)

			res := reconcileNode(newRefreshReconciler())

			// A record written now would be a guess, and it would be believed for the pod's life.
			_, ok := deviceRecord("xpum-adopt-unallocated")
			Expect(ok).To(BeFalse())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		})

		It("does not adopt a pod that is not Running", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			publishSlice(publishedDevice{name: devA})
			makeClaim(claimName, devA)
			makeXpumPod("xpum-pending", core.PodPending, nil, claimName)

			reconcileNode(newRefreshReconciler())

			_, ok := deviceRecord("xpum-pending")
			Expect(ok).To(BeFalse())
			Expect(podExists("xpum-pending")).To(BeTrue())
		})
	})

	Context("a GPU the container was never given", func() {
		It("restarts the pod when a card that booted in survivability mode is recovered", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA, devB)
			makeXpumPod("xpum-boot", core.PodRunning, ptr.To(devA), claimName)

			// The reflash finished and the driver dropped the taint.
			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devB})

			reconcileNode(newRefreshReconciler())

			Expect(podExists("xpum-boot")).To(BeFalse(),
				"only a new container can be given a device node for that card")
		})

		It("restarts the pod for a GPU that appears after it started", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-new-device", core.PodRunning, ptr.To(devA), claimName)

			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devC})

			reconcileNode(newRefreshReconciler())

			Expect(podExists("xpum-new-device")).To(BeFalse())
		})

		It("restarts a pod that was given nothing once a GPU becomes usable", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-empty", core.PodRunning, ptr.To(""), claimName)

			publishSlice(publishedDevice{name: devA})

			reconcileNode(newRefreshReconciler())

			Expect(podExists("xpum-empty")).To(BeFalse(),
				"an empty record is a container that was given no GPUs, not one that was never adopted")
		})

		It("waits for a device with no driver bound rather than restarting for it", func() {
			// An unbound device published as usable was the other half of the same bug: a restart
			// cannot deliver a card that has no DRM device, so it fires, fails to converge and burns
			// the node's whole restart budget while the reflash is still running.
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA, devB)
			makeXpumPod("xpum-unbound-wait", core.PodRunning, ptr.To(devA), claimName)

			r := newRefreshReconciler()

			By("devB published with no driver bound")
			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devB, unbound: true})
			reconcileNode(r)

			Expect(podExists("xpum-unbound-wait")).To(BeTrue())

			By("devB coming back on xe")
			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devB})
			reconcileNode(r)

			Expect(podExists("xpum-unbound-wait")).To(BeFalse(),
				"now there is a device node to hand over, and this container has none")
		})

		It("leaves a converged pod alone", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devB})
			makeClaim(claimName, devA, devB)
			makeXpumPod("xpum-converged", core.PodRunning, ptr.To(devA+","+devB), claimName)

			r := newRefreshReconciler()
			reconcileNode(r)
			reconcileNode(r)

			Expect(podExists("xpum-converged")).To(BeTrue())
			record, _ := deviceRecord("xpum-converged")
			Expect(record).To(Equal(devA+","+devB), "the record is written once and never rewritten")
		})
	})

	Context("a GPU that re-enumerated behind a device node the container holds", func() {
		It("leaves it to XPU Manager's own rescan under OnRecoveredDevice", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-rebind", core.PodRunning, ptr.To(devA), claimName)

			r := newRefreshReconciler()

			By("xpumd reporting the card into survivability while the reflash runs")
			publishSlice(publishedDevice{name: devA, taint: deviceTaintKeyXpumdReflash})
			reconcileNode(r)

			Expect(podExists("xpum-rebind")).To(BeTrue(),
				"restarting mid-recovery would not get the device back and would interrupt monitoring")

			By("the reflash completing and the taint clearing")
			publishSlice(publishedDevice{name: devA})
			reconcileNode(r)

			Expect(podExists("xpum-rebind")).To(BeTrue(),
				"the container still holds the node, and the card comes back on the minor it freed")

			record, _ := deviceRecord("xpum-rebind")
			Expect(record).To(Equal(devA))
		})

		It("restarts the pod under Always", func() {
			makePolicy(v1alpha.XpumRestartAlways, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-rebind-always", core.PodRunning, ptr.To(devA), claimName)

			r := newRefreshReconciler()

			By("the card being reset")
			publishSlice(publishedDevice{name: devA, taint: deviceTaintKeyReset})
			reconcileNode(r)

			Expect(podExists("xpum-rebind-always")).To(BeTrue())

			By("the reset completing")
			publishSlice(publishedDevice{name: devA})
			reconcileNode(r)

			Expect(podExists("xpum-rebind-always")).To(BeFalse())
		})

		It("sees a KMD unbind, which carries no taint at all", func() {
			// The case that exposed the taint-keyed edge: a reflash, a driver reload and a
			// manageBinding switch all show up as the driver attribute going xe -> "" -> xe, with no
			// device taint anywhere. Keying the edge on the taint missed every one of them, so Always
			// never restarted for the most common re-enumeration there is.
			makePolicy(v1alpha.XpumRestartAlways, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-unbind", core.PodRunning, ptr.To(devA), claimName)

			r := newRefreshReconciler()

			By("the KMD being unbound")
			publishSlice(publishedDevice{name: devA, unbound: true})
			reconcileNode(r)

			Expect(podExists("xpum-unbind")).To(BeTrue(),
				"there is nothing behind an unbound device for a new container to be given either")

			By("xe binding it again")
			publishSlice(publishedDevice{name: devA})
			reconcileNode(r)

			Expect(podExists("xpum-unbind")).To(BeFalse())
		})

		It("leaves an unbind to XPU Manager's rescan under OnRecoveredDevice", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-unbind-rescan", core.PodRunning, ptr.To(devA), claimName)

			r := newRefreshReconciler()

			publishSlice(publishedDevice{name: devA, unbound: true})
			reconcileNode(r)
			publishSlice(publishedDevice{name: devA})
			reconcileNode(r)

			Expect(podExists("xpum-unbind-rescan")).To(BeTrue())
		})

		It("keeps the edge when a guard defers the restart", func() {
			// The edge is consumed when the restart is performed or declined, not when it is
			// detected. Consuming it on detection lost the restart outright: the next pass recomputed
			// the rebind set from an edge that had already been deleted and found the node converged.
			makePolicy(v1alpha.XpumRestartAlways, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-deferred", core.PodRunning, ptr.To(devA), claimName)

			r := newRefreshReconciler()
			r.lastRestart[refreshNode] = time.Now()

			By("a full unbind and rebind inside the cooldown window")
			publishSlice(publishedDevice{name: devA, unbound: true})
			reconcileNode(r)
			publishSlice(publishedDevice{name: devA})
			res := reconcileNode(r)

			Expect(podExists("xpum-deferred")).To(BeTrue(), "the cooldown holds this pass back")
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			By("the cooldown expiring")
			r.lastRestart[refreshNode] = time.Now().Add(-2 * xpumRestartCooldown)
			reconcileNode(r)

			Expect(podExists("xpum-deferred")).To(BeFalse(),
				"the rebind was deferred, not dropped")
		})

		It("needs the taint edge, not just a slice write, under Always", func() {
			// Without a remembered taint there is no rebind: republishing an untainted device must
			// not be mistaken for a card that went away and came back.
			makePolicy(v1alpha.XpumRestartAlways, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-no-edge", core.PodRunning, ptr.To(devA), claimName)

			r := newRefreshReconciler()

			publishSlice(publishedDevice{name: devA})
			reconcileNode(r)
			publishSlice(publishedDevice{name: devA})
			reconcileNode(r)

			Expect(podExists("xpum-no-edge")).To(BeTrue())
		})
	})

	Context("changes that must not restart anything", func() {
		It("ignores a device that disappears from the slices", func() {
			// A slice can vanish and come back for publisher reasons — a DRA driver upgrade — with
			// nothing changed inside any running container. Checked under Always, the mode that
			// tracks rebinds, because that is where absence could leak in.
			makePolicy(v1alpha.XpumRestartAlways, resourceModeDRA)
			makeClaim(claimName, devA, devB)
			makeXpumPod("xpum-absent", core.PodRunning, ptr.To(devA+","+devB), claimName)

			publishSlice(publishedDevice{name: devA})

			r := newRefreshReconciler()
			reconcileNode(r)

			Expect(podExists("xpum-absent")).To(BeTrue())

			By("the slice coming back unchanged")
			publishSlice(publishedDevice{name: devA}, publishedDevice{name: devB})
			reconcileNode(r)

			Expect(podExists("xpum-absent")).To(BeTrue(),
				"a republished slice must not restart every xpum pod in the cluster")
		})

		It("ignores a taint the operator does not recover from", func() {
			makePolicy(v1alpha.XpumRestartAlways, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-unrelated", core.PodRunning, ptr.To(devA), claimName)

			r := newRefreshReconciler()

			By("a temperature excursion tainting the device")
			publishSlice(publishedDevice{name: devA, taint: "health-Temperature"})
			reconcileNode(r)

			By("the excursion clearing")
			publishSlice(publishedDevice{name: devA})
			reconcileNode(r)

			Expect(podExists("xpum-unrelated")).To(BeTrue(),
				"a transient fault does not re-enumerate the card, so there is nothing to pick up")
		})
	})

	Context("restartOnDeviceRecovery", func() {
		It("does nothing at all when Disabled", func() {
			makePolicy(v1alpha.XpumRestartDisabled, resourceModeDRA)
			publishSlice(publishedDevice{name: devA})
			makeClaim(claimName, devA)
			makeXpumPod("xpum-disabled", core.PodRunning, nil, claimName)

			reconcileNode(newRefreshReconciler())

			_, ok := deviceRecord("xpum-disabled")
			Expect(ok).To(BeFalse())
			Expect(podExists("xpum-disabled")).To(BeTrue())
		})

		It("is inert when GPUs are registered through the device plugin", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, "dp")
			publishSlice(publishedDevice{name: devA})
			makeClaim(claimName, devA)
			makeXpumPod("xpum-dp", core.PodRunning, nil, claimName)

			reconcileNode(newRefreshReconciler())

			_, ok := deviceRecord("xpum-dp")
			Expect(ok).To(BeFalse())
		})
	})

	Context("guards", func() {
		It("gives up on a node whose divergence survives repeated restarts", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-attempts", core.PodRunning, ptr.To(""), claimName)
			publishSlice(publishedDevice{name: devA})

			r := newRefreshReconciler()
			r.restartAttempts[refreshNode] = maxXpumRestartAttempts

			reconcileNode(r)

			Expect(podExists("xpum-attempts")).To(BeTrue(),
				"a restart that does not help will not help the next time either")
		})

		It("spaces out restarts of the same node", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-cooldown", core.PodRunning, ptr.To(""), claimName)
			publishSlice(publishedDevice{name: devA})

			r := newRefreshReconciler()
			r.lastRestart[refreshNode] = time.Now()

			res := reconcileNode(r)

			Expect(podExists("xpum-cooldown")).To(BeTrue())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		})

		It("holds back when too many xpum pods are already restarting", func() {
			makePolicy(v1alpha.XpumRestartOnRecoveredDevice, resourceModeDRA)
			makeClaim(claimName, devA)
			makeXpumPod("xpum-capped", core.PodRunning, ptr.To(""), claimName)
			publishSlice(publishedDevice{name: devA})

			for i := range maxConcurrentXpumRestarts {
				// Pods on other nodes that have not reached Running: the fleet is mid-restart.
				other := &core.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "xpum-other-" + string(rune('a'+i)),
						Namespace: refreshNS,
						Labels:    map[string]string{xpuLabel: xpuValue},
					},
					Spec: core.PodSpec{
						NodeName:   "other-node-" + string(rune('a'+i)),
						Containers: []core.Container{{Name: xpumdContainerName, Image: "xpumd:test"}},
					},
				}
				Expect(k8sClient.Create(ctx, other)).To(Succeed())
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, other, client.GracePeriodSeconds(0))
				})
			}

			res := reconcileNode(newRefreshReconciler())

			Expect(podExists("xpum-capped")).To(BeTrue(),
				"cluster monitoring must not go dark everywhere at once")
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		})
	})
})

var _ = Describe("XPU Manager device record", func() {
	DescribeTable("is read as the set of devices the container was given",
		func(value string, expected []string) {
			held := parseDeviceRecord(value)

			names := make([]string, 0, len(held))
			for name := range held {
				names = append(names, name)
			}

			Expect(names).To(ConsistOf(expected))
		},
		Entry("empty: a container that was given nothing", "", []string{}),
		Entry("one device", "dev-a", []string{"dev-a"}),
		Entry("several, in any order", "dev-b,dev-a", []string{"dev-a", "dev-b"}),
	)

	It("renders a set in a stable order", func() {
		Expect(formatDeviceRecord([]string{"dev-b", "dev-a"})).To(Equal("dev-a,dev-b"))
		Expect(formatDeviceRecord(nil)).To(BeEmpty())
	})
})

var _ = Describe("What the slices say about a device", func() {
	// The DRA driver publishes the boot-time survivability taint capitalised, while the keys xpumd
	// sources are lowercase. Both spellings are written out literally here rather than referenced
	// through their constants, so a change to either one has to be a deliberate edit to this table.
	DescribeTable("a recovery taint is matched only in the spelling the driver publishes",
		func(key string, expectRecovery bool) {
			dev := &resv1.Device{
				Name:   "dev-0",
				Taints: []resv1.DeviceTaint{{Key: key, Effect: resv1.DeviceTaintEffectNoExecute}},
			}

			Expect(deviceNeedsRecovery(dev)).To(Equal(expectRecovery))
		},
		Entry("as the driver publishes it", "health-Survivability", true),
		Entry("folded", "health-survivability", false),
		Entry("shouted", "HEALTH-SURVIVABILITY", false),
		Entry("sourced from xpumd", "health-xpumd-gpu.survivability", true),
		Entry("a wedged card", "health-xpumd-gpu.wedged", true),
		Entry("a transient condition", "health-Temperature", false),
		Entry("nothing recognisable", "example.com/other", false),
	)

	DescribeTable("and the driver attribute only answers what it can",
		func(driver string, monitorable bool) {
			dev := &resv1.Device{
				Name: "dev-0",
				Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
					deviceAttrDriver: {StringValue: ptr.To(driver)},
				},
			}

			Expect(xpumdMonitorableDriver(dev)).To(Equal(monitorable))
		},
		// xe binds to a card in survivability mode too, which is why the taint and not this
		// attribute decides whether a *bound* device is usable.
		Entry("xe", "xe", true),
		Entry("i915", "i915", true),
		Entry("passed through", "vfio-pci", false),
		Entry("passed through, underscored", "VFIO_PCI", false),
		Entry("passed through via xe", "xe-vfio", false),
		// Published as empty means no KMD is bound, so there is no DRM device behind it. Reading
		// this as monitorable is what restarted pods for devices mid-reflash and hid every unbind
		// from the rebind tracking.
		Entry("nothing bound", "", false),
		// An allowlist, so a KMD nobody here knows about costs a missed restart rather than a
		// restart loop for a device that can never arrive.
		Entry("unknown driver names", "something-new", false),
	)

	It("treats a device with no driver attribute as monitorable", func() {
		Expect(xpumdMonitorableDriver(&resv1.Device{Name: "dev-0"})).To(BeTrue())
	})
})
