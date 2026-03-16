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

package driver

import (
	"context"
	"fmt"
	"net"
	"strings"

	whereaboutsclient "github.com/k8snetworkplumbingwg/whereabouts/pkg/generated/clientset/versioned"
	whereaboutskubernetes "github.com/k8snetworkplumbingwg/whereabouts/pkg/storage/kubernetes"
	whereaboutstypes "github.com/k8snetworkplumbingwg/whereabouts/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/dranet/pkg/apis"
)

const whereaboutsNamespace = metav1.NamespaceSystem

type whereaboutsProvider struct {
	client *whereaboutskubernetes.Client
}

func newWhereaboutsProvider(restConfig *rest.Config, kubeClient kubernetes.Interface) (*whereaboutsProvider, error) {
	cfg := rest.CopyConfig(restConfig)
	// CRDs are served as JSON, make sure requests don't force protobuf content-type.
	cfg.AcceptContentTypes = "application/json"
	cfg.ContentType = "application/json"

	whereaboutsClient, err := whereaboutsclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &whereaboutsProvider{
		client: whereaboutskubernetes.NewKubernetesClient(whereaboutsClient, kubeClient),
	}, nil
}

func (p *whereaboutsProvider) Allocate(ctx context.Context, req ipamAllocateRequest) (ipamAllocateResult, error) {
	if req.IPAM.Whereabouts == nil {
		return ipamAllocateResult{}, fmt.Errorf("whereabouts config is required")
	}

	containerID := buildIPAMContainerID(req.ClaimUID, req.PodUID, req.DeviceName)
	ipamConf, namespace, err := toWhereaboutsIPAMConfig(req.PodRef, req.IPAM.Whereabouts)
	if err != nil {
		return ipamAllocateResult{}, err
	}

	ipam := &whereaboutskubernetes.KubernetesIPAM{
		Client:      *p.client,
		Config:      ipamConf,
		Namespace:   namespace,
		ContainerID: containerID,
		IfName:      req.IfName,
	}

	allocatedIPs, err := whereaboutskubernetes.IPManagementKubernetesUpdate(ctx, whereaboutstypes.Allocate, ipam, ipamConf)
	if err != nil {
		return ipamAllocateResult{}, fmt.Errorf("whereabouts allocate failed: %w", err)
	}
	if len(allocatedIPs) == 0 {
		return ipamAllocateResult{}, fmt.Errorf("whereabouts allocate returned no IPs")
	}

	addresses := make([]string, 0, len(allocatedIPs))
	for _, allocatedIP := range allocatedIPs {
		addresses = append(addresses, allocatedIP.String())
	}

	return ipamAllocateResult{
		Addresses: addresses,
		Lease: IPAMAllocation{
			Provider:    apis.IPAMTypeWhereabouts,
			ContainerID: containerID,
			PodRef:      req.PodRef,
		},
	}, nil
}

func (p *whereaboutsProvider) Release(ctx context.Context, req ipamReleaseRequest) error {
	if req.IPAM.Whereabouts == nil {
		return nil
	}

	ipamConf, namespace, err := toWhereaboutsIPAMConfig(req.Lease.PodRef, req.IPAM.Whereabouts)
	if err != nil {
		return err
	}

	ipam := &whereaboutskubernetes.KubernetesIPAM{
		Client:      *p.client,
		Config:      ipamConf,
		Namespace:   namespace,
		ContainerID: req.Lease.ContainerID,
		IfName:      req.IfName,
	}

	if _, err := whereaboutskubernetes.IPManagementKubernetesUpdate(ctx, whereaboutstypes.Deallocate, ipam, ipamConf); err != nil {
		return fmt.Errorf("whereabouts release failed: %w", err)
	}
	return nil
}

func toWhereaboutsIPAMConfig(podRef string, cfg *apis.WhereaboutsConfig) (whereaboutstypes.IPAMConfig, string, error) {
	if cfg == nil {
		return whereaboutstypes.IPAMConfig{}, "", fmt.Errorf("whereabouts config is nil")
	}

	podNamespace, podName, err := splitWhereaboutsPodRef(podRef)
	if err != nil {
		return whereaboutstypes.IPAMConfig{}, "", err
	}

	ipamConfig := whereaboutstypes.IPAMConfig{
		Type:              apis.IPAMTypeWhereabouts,
		Range:             cfg.Range,
		OmitRanges:        cfg.Exclude,
		RangeStart:        net.ParseIP(cfg.RangeStart),
		RangeEnd:          net.ParseIP(cfg.RangeEnd),
		OverlappingRanges: true,
		PodName:           podName,
		PodNamespace:      podNamespace,
		NetworkName:       cfg.NetworkName,
	}
	if cfg.RangeStart != "" && ipamConfig.RangeStart == nil {
		return whereaboutstypes.IPAMConfig{}, "", fmt.Errorf("invalid whereabouts rangeStart %q", cfg.RangeStart)
	}
	if cfg.RangeEnd != "" && ipamConfig.RangeEnd == nil {
		return whereaboutstypes.IPAMConfig{}, "", fmt.Errorf("invalid whereabouts rangeEnd %q", cfg.RangeEnd)
	}
	ipamConfig.IPRanges = []whereaboutstypes.RangeConfiguration{{
		Range:      cfg.Range,
		OmitRanges: cfg.Exclude,
		RangeStart: ipamConfig.RangeStart,
		RangeEnd:   ipamConfig.RangeEnd,
	}}

	return ipamConfig, whereaboutsNamespace, nil
}

func splitWhereaboutsPodRef(podRef string) (string, string, error) {
	namespace, name, ok := strings.Cut(podRef, "/")
	if !ok || namespace == "" || name == "" {
		return "", "", fmt.Errorf("invalid podRef %q", podRef)
	}
	return namespace, name, nil
}
