/*
Copyright The Kubernetes Authors

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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
	netutils "k8s.io/utils/net"
)

var (
	ipPoolGVR = schema.GroupVersionResource{
		Group: "whereabouts.cni.cncf.io", Version: "v1alpha1", Resource: "ippools",
	}
	overlappingRangeGVR = schema.GroupVersionResource{
		Group: "whereabouts.cni.cncf.io", Version: "v1alpha1", Resource: "overlappingrangeipreservations",
	}
)

type managedPool struct {
	Name              string
	Range             string
	NetworkName       string
	OverlappingRanges bool
	Profile           string
}

type whereaboutsNetConf struct {
	Name string `json:"name"`
	IPAM struct {
		Type     string `json:"type"`
		Range    string `json:"range"`
		IPRanges []struct {
			Range string `json:"range"`
		} `json:"ipRanges"`
		NetworkName             string `json:"network_name"`
		NodeSliceSize           string `json:"node_slice_size"`
		EnableOverlappingRanges *bool  `json:"enable_overlapping_ranges"`
	} `json:"ipam"`
}

func managedPoolsFromProfiles(profiles map[string]cniNetConf) (map[string]managedPool, error) {
	pools := map[string]managedPool{}
	for profileName, profile := range profiles {
		var conf whereaboutsNetConf
		if err := json.Unmarshal(profile.rawBytes, &conf); err != nil {
			return nil, fmt.Errorf("decode profile %q for garbage collection: %w", profileName, err)
		}
		if conf.IPAM.Type != "whereabouts" {
			continue
		}
		if conf.IPAM.NodeSliceSize != "" {
			return nil, fmt.Errorf("profile %q uses node_slice_size, which is not supported by webhook garbage collection", profileName)
		}

		ranges := make([]string, 0, len(conf.IPAM.IPRanges)+1)
		if conf.IPAM.Range != "" {
			ranges = append(ranges, conf.IPAM.Range)
		}
		for _, ipRange := range conf.IPAM.IPRanges {
			if ipRange.Range != "" {
				ranges = append(ranges, ipRange.Range)
			}
		}
		if len(ranges) == 0 {
			return nil, fmt.Errorf("whereabouts profile %q does not define range or ipRanges", profileName)
		}

		overlapping := true
		if conf.IPAM.EnableOverlappingRanges != nil {
			overlapping = *conf.IPAM.EnableOverlappingRanges
		}
		for _, ipRange := range ranges {
			normalizedRange, err := normalizeWhereaboutsRange(ipRange)
			if err != nil {
				return nil, fmt.Errorf("whereabouts profile %q has invalid range %q: %w", profileName, ipRange, err)
			}
			name := whereaboutsPoolName(normalizedRange, conf.IPAM.NetworkName)
			pool := managedPool{
				Name:              name,
				Range:             normalizedRange,
				NetworkName:       conf.IPAM.NetworkName,
				OverlappingRanges: overlapping,
				Profile:           profileName,
			}
			if existing, found := pools[name]; found && existing != pool {
				return nil, fmt.Errorf("whereabouts profiles %q and %q map to conflicting IPPool %q", existing.Profile, profileName, name)
			}
			pools[name] = pool
		}
	}
	return pools, nil
}

// normalizeWhereaboutsRange mirrors the normalization done by whereabouts
// while loading its CNI configuration. In particular, the range-end syntax
// "startIP-endIP/prefix" is converted to the containing network CIDR before
// whereabouts derives the IPPool name and stores spec.range.
func normalizeWhereaboutsRange(ipRange string) (string, error) {
	if parts := strings.SplitN(ipRange, "-", 2); len(parts) == 2 {
		firstIP := netutils.ParseIPSloppy(parts[0])
		if firstIP == nil {
			return "", fmt.Errorf("invalid range start IP %q", parts[0])
		}
		_, ipNet, err := netutils.ParseCIDRSloppy(parts[1])
		if err != nil {
			return "", fmt.Errorf("invalid range end CIDR %q: %w", parts[1], err)
		}
		if !ipNet.Contains(firstIP) {
			return "", fmt.Errorf("range start IP %s is outside %s", firstIP, ipNet)
		}
		return ipNet.String(), nil
	}

	_, ipNet, err := netutils.ParseCIDRSloppy(ipRange)
	if err != nil {
		return "", err
	}
	return ipNet.String(), nil
}

func whereaboutsPoolName(ipRange, networkName string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(ipRange, ":", "-"), "/", "-")
	if networkName == "" {
		return normalized
	}
	return networkName + "-" + normalized
}

type allocationSnapshot struct {
	Pool   managedPool
	Offset string
	ID     string
	PodRef string
	IfName string
	IP     net.IP
}

func (a allocationSnapshot) key() string {
	return strings.Join([]string{a.Pool.Name, a.Offset, a.ID, a.PodRef, a.IfName}, "\x00")
}

type GCController struct {
	kubeClient        kubernetes.Interface
	dynamicClient     dynamic.Interface
	namespace         string
	managedPools      map[string]managedPool
	interval          time.Duration
	gracePeriod       time.Duration
	now               func() time.Time
	candidates        map[string]time.Time
	overlapCandidates map[string]time.Time
}

func NewGCController(kubeClient kubernetes.Interface, dynamicClient dynamic.Interface, namespace string, pools map[string]managedPool, interval, gracePeriod time.Duration) (*GCController, error) {
	if kubeClient == nil || dynamicClient == nil {
		return nil, fmt.Errorf("Kubernetes clients are required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("whereabouts namespace is required")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("garbage collection interval must be positive")
	}
	if gracePeriod < 0 {
		return nil, fmt.Errorf("garbage collection grace period must not be negative")
	}
	return &GCController{
		kubeClient:        kubeClient,
		dynamicClient:     dynamicClient,
		namespace:         namespace,
		managedPools:      pools,
		interval:          interval,
		gracePeriod:       gracePeriod,
		now:               time.Now,
		candidates:        map[string]time.Time{},
		overlapCandidates: map[string]time.Time{},
	}, nil
}

func (c *GCController) Run(ctx context.Context) {
	log.Printf("Starting whereabouts ResourceClaim garbage collector for %d IPPool(s)", len(c.managedPools))
	if err := c.Reconcile(ctx); err != nil {
		log.Printf("whereabouts garbage collection failed: %v", err)
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Reconcile(ctx); err != nil {
				log.Printf("whereabouts garbage collection failed: %v", err)
			}
		}
	}
}

func (c *GCController) Reconcile(ctx context.Context) error {
	liveClaims, err := c.listLiveClaimUIDs(ctx)
	if err != nil {
		// Fail closed: an incomplete claim list must never be interpreted as all
		// claims having disappeared.
		return fmt.Errorf("list ResourceClaims: %w", err)
	}

	now := c.now()
	seenCandidates := map[string]struct{}{}
	ready := make([]allocationSnapshot, 0)
	for _, pool := range c.managedPools {
		allocations, err := c.listPoolAllocations(ctx, pool)
		if err != nil {
			return err
		}
		for _, allocation := range allocations {
			key := allocation.key()
			if _, live := liveClaims[types.UID(allocation.ID)]; live {
				delete(c.candidates, key)
				continue
			}
			seenCandidates[key] = struct{}{}
			firstSeen, found := c.candidates[key]
			if !found {
				c.candidates[key] = now
				continue
			}
			if now.Sub(firstSeen) >= c.gracePeriod {
				ready = append(ready, allocation)
			}
		}
	}
	for key := range c.candidates {
		if _, seen := seenCandidates[key]; !seen {
			delete(c.candidates, key)
		}
	}

	if len(ready) > 0 {
		// Re-list immediately before mutation so a newly recreated or newly
		// reserved claim wins over a stale candidate.
		liveClaims, err = c.listLiveClaimUIDs(ctx)
		if err != nil {
			return fmt.Errorf("re-list ResourceClaims before deletion: %w", err)
		}
	}
	for _, allocation := range ready {
		if _, live := liveClaims[types.UID(allocation.ID)]; live {
			delete(c.candidates, allocation.key())
			continue
		}
		deleted, err := c.deleteAllocation(ctx, allocation)
		if err != nil {
			return err
		}
		if deleted {
			log.Printf("Garbage-collected whereabouts allocation IP=%s claimUID=%s podRef=%s pool=%s", allocation.IP, allocation.ID, allocation.PodRef, allocation.Pool.Name)
		}
		delete(c.candidates, allocation.key())
	}

	return c.cleanupOverlappingReservations(ctx)
}

func (c *GCController) listLiveClaimUIDs(ctx context.Context) (map[types.UID]struct{}, error) {
	claims, err := c.kubeClient.ResourceV1().ResourceClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	live := make(map[types.UID]struct{}, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.UID == "" || claim.Status.Allocation == nil || !hasPodConsumer(claim.Status.ReservedFor) {
			continue
		}
		live[claim.UID] = struct{}{}
	}
	return live, nil
}

func hasPodConsumer(consumers []resourceapi.ResourceClaimConsumerReference) bool {
	for _, consumer := range consumers {
		if consumer.APIGroup == "" && consumer.Resource == "pods" && consumer.Name != "" && consumer.UID != "" {
			return true
		}
	}
	return false
}

func (c *GCController) listPoolAllocations(ctx context.Context, pool managedPool) ([]allocationSnapshot, error) {
	obj, err := c.dynamicClient.Resource(ipPoolGVR).Namespace(c.namespace).Get(ctx, pool.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get whereabouts IPPool %s/%s: %w", c.namespace, pool.Name, err)
	}
	actualRange, _, _ := unstructured.NestedString(obj.Object, "spec", "range")
	if actualRange != pool.Range {
		return nil, fmt.Errorf("managed IPPool %s/%s has range %q, expected %q for profile %q", c.namespace, pool.Name, actualRange, pool.Range, pool.Profile)
	}
	values, _, err := unstructured.NestedMap(obj.Object, "spec", "allocations")
	if err != nil {
		return nil, fmt.Errorf("read allocations from IPPool %s/%s: %w", c.namespace, pool.Name, err)
	}
	result := make([]allocationSnapshot, 0, len(values))
	for offset, value := range values {
		allocation, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("IPPool %s/%s allocation %q has invalid type %T", c.namespace, pool.Name, offset, value)
		}
		id, _ := allocation["id"].(string)
		podRef, _ := allocation["podref"].(string)
		ifName, _ := allocation["ifname"].(string)
		if id == "" {
			// Allocations without a claim UID are not owned by the DRA webhook.
			continue
		}
		ip, err := ipAtOffset(pool.Range, offset)
		if err != nil {
			return nil, fmt.Errorf("decode allocation %q from IPPool %s/%s: %w", offset, c.namespace, pool.Name, err)
		}
		result = append(result, allocationSnapshot{Pool: pool, Offset: offset, ID: id, PodRef: podRef, IfName: ifName, IP: ip})
	}
	return result, nil
}

func (c *GCController) deleteAllocation(ctx context.Context, allocation allocationSnapshot) (bool, error) {
	deleted := false
	resource := c.dynamicClient.Resource(ipPoolGVR).Namespace(c.namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := resource.Get(ctx, allocation.Pool.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		values, _, err := unstructured.NestedMap(obj.Object, "spec", "allocations")
		if err != nil {
			return err
		}
		value, found := values[allocation.Offset]
		if !found {
			return nil
		}
		current, ok := value.(map[string]interface{})
		if !ok || current["id"] != allocation.ID || current["podref"] != allocation.PodRef || current["ifname"] != allocation.IfName {
			// The offset has been changed or reallocated since it became a
			// candidate. Never delete the new owner.
			return nil
		}
		delete(values, allocation.Offset)
		if err := unstructured.SetNestedMap(obj.Object, values, "spec", "allocations"); err != nil {
			return err
		}
		if _, err := resource.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("delete allocation %s from IPPool %s/%s: %w", allocation.Offset, c.namespace, allocation.Pool.Name, err)
	}
	return deleted, nil
}

func (c *GCController) cleanupOverlappingReservations(ctx context.Context) error {
	expected, err := c.expectedOverlappingReservations(ctx)
	if err != nil {
		return err
	}

	resource := c.dynamicClient.Resource(overlappingRangeGVR).Namespace(c.namespace)
	items, err := resource.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list OverlappingRangeIPReservations: %w", err)
	}
	now := c.now()
	seenCandidates := map[string]struct{}{}
	ready := make([]unstructured.Unstructured, 0)
	for i := range items.Items {
		item := &items.Items[i]
		if !c.isManagedOverlappingReservation(item.GetName()) {
			continue
		}
		if _, found := expected[item.GetName()]; found {
			delete(c.overlapCandidates, item.GetName())
			continue
		}
		seenCandidates[item.GetName()] = struct{}{}
		firstSeen, found := c.overlapCandidates[item.GetName()]
		if !found {
			c.overlapCandidates[item.GetName()] = now
			continue
		}
		if now.Sub(firstSeen) >= c.gracePeriod {
			ready = append(ready, *item)
		}
	}
	for name := range c.overlapCandidates {
		if _, seen := seenCandidates[name]; !seen {
			delete(c.overlapCandidates, name)
		}
	}

	if len(ready) > 0 {
		// Rebuild the desired set immediately before deletion. This protects a
		// reservation which was created just before its IPPool allocation.
		expected, err = c.expectedOverlappingReservations(ctx)
		if err != nil {
			return err
		}
	}
	for i := range ready {
		item := &ready[i]
		if _, found := expected[item.GetName()]; found {
			delete(c.overlapCandidates, item.GetName())
			continue
		}
		uid := item.GetUID()
		if err := resource.Delete(ctx, item.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete orphaned OverlappingRangeIPReservation %s/%s: %w", c.namespace, item.GetName(), err)
		}
		delete(c.overlapCandidates, item.GetName())
		log.Printf("Garbage-collected orphaned OverlappingRangeIPReservation %s/%s", c.namespace, item.GetName())
	}
	return nil
}

func (c *GCController) expectedOverlappingReservations(ctx context.Context) (map[string]struct{}, error) {
	expected := map[string]struct{}{}
	for _, pool := range c.managedPools {
		if !pool.OverlappingRanges {
			continue
		}
		allocations, err := c.listPoolAllocations(ctx, pool)
		if err != nil {
			return nil, err
		}
		for _, allocation := range allocations {
			expected[overlappingReservationName(allocation.IP, pool.NetworkName)] = struct{}{}
		}
	}
	return expected, nil
}

func (c *GCController) isManagedOverlappingReservation(name string) bool {
	for _, pool := range c.managedPools {
		if !pool.OverlappingRanges {
			continue
		}
		encodedIP := name
		if pool.NetworkName != "" {
			prefix := pool.NetworkName + "-"
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			encodedIP = strings.TrimPrefix(name, prefix)
		}
		ip := net.ParseIP(encodedIP)
		if ip == nil {
			ip = net.ParseIP(strings.ReplaceAll(encodedIP, "-", ":"))
		}
		_, ipNet, err := net.ParseCIDR(pool.Range)
		if err == nil && ip != nil && ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

func overlappingReservationName(ip net.IP, networkName string) string {
	name := strings.ReplaceAll(ip.String(), ":", "-")
	if networkName != "" {
		return networkName + "-" + name
	}
	return name
}

func ipAtOffset(cidr, offset string) (net.IP, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	offsetInt, ok := new(big.Int).SetString(offset, 10)
	if !ok || offsetInt.Sign() < 0 {
		return nil, fmt.Errorf("invalid non-negative offset %q", offset)
	}
	width := net.IPv6len
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		width = net.IPv4len
	}
	value := new(big.Int).SetBytes(ip)
	value.Add(value, offsetInt)
	if value.BitLen() > width*8 {
		return nil, fmt.Errorf("offset %s overflows range address width", offset)
	}
	bytes := value.Bytes()
	result := make(net.IP, width)
	copy(result[width-len(bytes):], bytes)
	if !ipNet.Contains(result) {
		return nil, fmt.Errorf("offset %s is outside range %s", offset, cidr)
	}
	return result, nil
}

func runGCWithLeaderElection(ctx context.Context, kubeClient kubernetes.Interface, controller *GCController, namespace, leaseName string, enabled bool) {
	if !enabled {
		controller.Run(ctx)
		return
	}
	identity := os.Getenv("POD_NAME")
	if identity == "" {
		identity, _ = os.Hostname()
	}
	identity += "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: leaseName, Namespace: namespace},
		Client:    kubeClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Name:            "webhook-whereabouts-gc",
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) { controller.Run(leaderCtx) },
			OnStoppedLeading: func() { log.Printf("Lost whereabouts garbage collector leadership") },
		},
	})
}
