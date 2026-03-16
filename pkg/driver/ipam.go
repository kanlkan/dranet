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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
)

type ipamProvider interface {
	Allocate(ctx context.Context, req ipamAllocateRequest) (ipamAllocateResult, error)
	Release(ctx context.Context, req ipamReleaseRequest) error
}

type ipamAllocateRequest struct {
	ClaimUID   types.UID
	Namespace  string
	PodUID     types.UID
	PodRef     string
	DeviceName string
	IfName     string
	IPAM       apis.IPAMConfig
}

type ipamAllocateResult struct {
	Addresses []string
	Lease     IPAMAllocation
}

type ipamReleaseRequest struct {
	Namespace string
	IfName    string
	IPAM      apis.IPAMConfig
	Lease     IPAMAllocation
}

func (np *NetworkDriver) getIPAMProvider(ipamType string) (ipamProvider, error) {
	np.ipamMu.Lock()
	defer np.ipamMu.Unlock()

	if np.ipamProviders == nil {
		np.ipamProviders = map[string]ipamProvider{}
	}
	if provider, ok := np.ipamProviders[ipamType]; ok {
		return provider, nil
	}

	switch ipamType {
	case apis.IPAMTypeWhereabouts:
		if np.restConfig == nil {
			return nil, fmt.Errorf("rest config is not set; whereabouts provider cannot be initialized")
		}
		provider, err := newWhereaboutsProvider(np.restConfig, np.kubeClient)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize whereabouts provider: %w", err)
		}
		np.ipamProviders[ipamType] = provider
		return provider, nil
	default:
		return nil, fmt.Errorf("unsupported ipam type %q", ipamType)
	}
}

func buildIPAMContainerID(claimUID types.UID, podUID types.UID, deviceName string) string {
	return fmt.Sprintf("%s/%s/%s", claimUID, podUID, deviceName)
}
