/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	testNamespace = "kube-system"
	testRange     = "192.168.100.0/24"
	testPoolName  = "192.168.100.0-24"
)

func TestManagedPoolsFromProfiles(t *testing.T) {
	profiles := map[string]cniNetConf{
		"whereabouts-profile": {rawBytes: []byte(`{
			"name":"whereabouts-profile",
			"ipam":{"type":"whereabouts","range":"192.168.100.0/24","network_name":"tenant-a"}
		}`)},
		"other": {rawBytes: []byte(`{"name":"other","ipam":{"type":"host-local","range":"10.0.0.0/24"}}`)},
	}

	pools, err := managedPoolsFromProfiles(profiles)
	if err != nil {
		t.Fatalf("managedPoolsFromProfiles() error = %v", err)
	}
	pool, found := pools["tenant-a-192.168.100.0-24"]
	if !found {
		t.Fatalf("expected managed pool, got %#v", pools)
	}
	if pool.Range != testRange || pool.NetworkName != "tenant-a" || !pool.OverlappingRanges {
		t.Fatalf("unexpected managed pool: %#v", pool)
	}
}

func TestManagedPoolsNormalizesWhereaboutsRanges(t *testing.T) {
	for _, tt := range []struct {
		name     string
		ipRange  string
		wantName string
		wantCIDR string
	}{
		{
			name:     "range end syntax",
			ipRange:  "192.168.130.119-192.168.130.128/24",
			wantName: "192.168.130.0-24",
			wantCIDR: "192.168.130.0/24",
		},
		{
			name:     "host address in CIDR",
			ipRange:  "192.168.101.225/28",
			wantName: "192.168.101.224-28",
			wantCIDR: "192.168.101.224/28",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			profiles := map[string]cniNetConf{
				"whereabouts-profile": {rawBytes: []byte(`{
					"name":"whereabouts-profile",
					"ipam":{"type":"whereabouts","range":"` + tt.ipRange + `"}
				}`)},
			}

			pools, err := managedPoolsFromProfiles(profiles)
			if err != nil {
				t.Fatalf("managedPoolsFromProfiles() error = %v", err)
			}
			pool, found := pools[tt.wantName]
			if !found {
				t.Fatalf("expected managed pool %q, got %#v", tt.wantName, pools)
			}
			if pool.Range != tt.wantCIDR {
				t.Fatalf("managed pool range = %q, want %q", pool.Range, tt.wantCIDR)
			}
		})
	}
}

func TestManagedPoolsRejectsRangeStartOutsideCIDR(t *testing.T) {
	profiles := map[string]cniNetConf{
		"whereabouts-profile": {rawBytes: []byte(`{
			"name":"whereabouts-profile",
			"ipam":{"type":"whereabouts","range":"192.168.129.119-192.168.130.128/24"}
		}`)},
	}
	if _, err := managedPoolsFromProfiles(profiles); err == nil {
		t.Fatal("managedPoolsFromProfiles() accepted a range start outside the range end CIDR")
	}
}

func TestManagedPoolsRejectsNodeSlice(t *testing.T) {
	profiles := map[string]cniNetConf{
		"sliced": {rawBytes: []byte(`{
			"name":"sliced",
			"ipam":{"type":"whereabouts","range":"192.168.100.0/24","node_slice_size":"/26"}
		}`)},
	}
	if _, err := managedPoolsFromProfiles(profiles); err == nil {
		t.Fatal("managedPoolsFromProfiles() accepted unsupported node_slice_size")
	}
}

func TestGCReconcileKeepsLiveClaimAllocation(t *testing.T) {
	claimUID := types.UID("claim-live")
	controller, dynamicClient, now := newTestGC(t, []*resourceapi.ResourceClaim{liveClaim(claimUID)},
		ipPool(map[string]interface{}{"1": allocation(string(claimUID), "default/pod", "net1")}),
		overlapReservation("192.168.100.1"),
	)

	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	*now = now.Add(10 * time.Minute)
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}

	assertPoolAllocationIDs(t, dynamicClient, map[string]string{"1": string(claimUID)})
	if _, err := dynamicClient.Resource(overlappingRangeGVR).Namespace(testNamespace).Get(context.Background(), "192.168.100.1", metav1.GetOptions{}); err != nil {
		t.Fatalf("live overlap reservation was removed: %v", err)
	}
}

func TestGCReconcileCollectsOrphanAfterGracePeriod(t *testing.T) {
	controller, dynamicClient, now := newTestGC(t, nil,
		ipPool(map[string]interface{}{"1": allocation("claim-gone", "default/deleted-pod", "net1")}),
		overlapReservation("192.168.100.1"),
	)

	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	*now = now.Add(30 * time.Second)
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() before grace period error = %v", err)
	}
	assertPoolAllocationIDs(t, dynamicClient, map[string]string{"1": "claim-gone"})

	*now = now.Add(31 * time.Second)
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() after grace period error = %v", err)
	}
	assertPoolAllocationIDs(t, dynamicClient, map[string]string{})
	// The overlap reservation gets its own grace period, starting after the
	// corresponding allocation has been removed.
	*now = now.Add(61 * time.Second)
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() for overlap reservation error = %v", err)
	}
	if _, err := dynamicClient.Resource(overlappingRangeGVR).Namespace(testNamespace).Get(context.Background(), "192.168.100.1", metav1.GetOptions{}); err == nil {
		t.Fatal("orphaned overlap reservation was not removed")
	}
}

func TestGCReconcileDoesNotDeleteReallocatedOffset(t *testing.T) {
	controller, dynamicClient, now := newTestGC(t, nil,
		ipPool(map[string]interface{}{"1": allocation("claim-old", "default/old-pod", "net1")}),
	)
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}

	pool, err := dynamicClient.Resource(ipPoolGVR).Namespace(testNamespace).Get(context.Background(), testPoolName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedMap(pool.Object, map[string]interface{}{"1": allocation("claim-new", "default/new-pod", "net1")}, "spec", "allocations"); err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicClient.Resource(ipPoolGVR).Namespace(testNamespace).Update(context.Background(), pool, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(10 * time.Minute)
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	assertPoolAllocationIDs(t, dynamicClient, map[string]string{"1": "claim-new"})
}

func TestGCReconcileFailsClosedWhenClaimsCannotBeListed(t *testing.T) {
	controller, dynamicClient, _ := newTestGC(t, nil,
		ipPool(map[string]interface{}{"1": allocation("claim-unknown", "default/pod", "net1")}),
	)
	controller.kubeClient.(*kubernetesfake.Clientset).PrependReactor("list", "resourceclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("API unavailable")
	})

	if err := controller.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() succeeded despite ResourceClaim list failure")
	}
	assertPoolAllocationIDs(t, dynamicClient, map[string]string{"1": "claim-unknown"})
}

func TestGCReconcileFailsClosedWhenClaimRelistFails(t *testing.T) {
	controller, dynamicClient, now := newTestGC(t, nil,
		ipPool(map[string]interface{}{"1": allocation("claim-unknown", "default/pod", "net1")}),
	)
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	*now = now.Add(2 * time.Minute)

	listCalls := 0
	controller.kubeClient.(*kubernetesfake.Clientset).PrependReactor("list", "resourceclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		listCalls++
		if listCalls == 2 {
			return true, nil, errors.New("API unavailable during safety re-list")
		}
		return false, nil, nil
	})
	if err := controller.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() succeeded despite pre-deletion ResourceClaim re-list failure")
	}
	assertPoolAllocationIDs(t, dynamicClient, map[string]string{"1": "claim-unknown"})
}

func TestIPAtOffset(t *testing.T) {
	for _, tt := range []struct {
		cidr   string
		offset string
		want   string
	}{
		{cidr: "192.168.100.0/24", offset: "17", want: "192.168.100.17"},
		{cidr: "2001:db8::/64", offset: "2", want: "2001:db8::2"},
	} {
		got, err := ipAtOffset(tt.cidr, tt.offset)
		if err != nil {
			t.Fatalf("ipAtOffset(%q, %q) error = %v", tt.cidr, tt.offset, err)
		}
		if got.String() != tt.want {
			t.Fatalf("ipAtOffset(%q, %q) = %q, want %q", tt.cidr, tt.offset, got, tt.want)
		}
	}
}

func newTestGC(t *testing.T, claims []*resourceapi.ResourceClaim, objects ...runtime.Object) (*GCController, *dynamicfake.FakeDynamicClient, *time.Time) {
	t.Helper()
	kubeObjects := make([]runtime.Object, 0, len(claims))
	for _, claim := range claims {
		kubeObjects = append(kubeObjects, claim)
	}
	kubeClient := kubernetesfake.NewSimpleClientset(kubeObjects...)
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ipPoolGVR:           "IPPoolList",
		overlappingRangeGVR: "OverlappingRangeIPReservationList",
	}, objects...)
	pools := map[string]managedPool{testPoolName: {
		Name:              testPoolName,
		Range:             testRange,
		OverlappingRanges: true,
		Profile:           "whereabouts-profile",
	}}
	controller, err := NewGCController(kubeClient, dynamicClient, testNamespace, pools, time.Minute, time.Minute)
	if err != nil {
		t.Fatalf("NewGCController() error = %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	controller.now = func() time.Time { return now }
	return controller, dynamicClient, &now
}

func liveClaim(uid types.UID) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation:  &resourceapi.AllocationResult{},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: "pod", UID: "pod-uid"}},
		},
	}
}

func allocation(id, podRef, ifName string) map[string]interface{} {
	return map[string]interface{}{"id": id, "podref": podRef, "ifname": ifName}
}

func ipPool(allocations map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "whereabouts.cni.cncf.io/v1alpha1",
		"kind":       "IPPool",
		"metadata": map[string]interface{}{
			"name": testPoolName, "namespace": testNamespace,
		},
		"spec": map[string]interface{}{"range": testRange, "allocations": allocations},
	}}
}

func overlapReservation(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "whereabouts.cni.cncf.io/v1alpha1",
		"kind":       "OverlappingRangeIPReservation",
		"metadata": map[string]interface{}{
			"name": name, "namespace": testNamespace, "uid": name + "-uid",
		},
	}}
}

func assertPoolAllocationIDs(t *testing.T, dynamicClient *dynamicfake.FakeDynamicClient, want map[string]string) {
	t.Helper()
	pool, err := dynamicClient.Resource(ipPoolGVR).Namespace(testNamespace).Get(context.Background(), testPoolName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	allocations, _, err := unstructured.NestedMap(pool.Object, "spec", "allocations")
	if err != nil {
		t.Fatalf("read allocations: %v", err)
	}
	if len(allocations) != len(want) {
		t.Fatalf("allocations = %#v, want IDs %#v", allocations, want)
	}
	for offset, wantID := range want {
		allocation, found := allocations[offset].(map[string]interface{})
		if !found || allocation["id"] != wantID {
			t.Fatalf("allocation %q = %#v, want id %q", offset, allocations[offset], wantID)
		}
	}
}
