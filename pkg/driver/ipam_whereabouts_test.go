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
	"testing"

	whereaboutstypes "github.com/k8snetworkplumbingwg/whereabouts/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestToWhereaboutsIPAMConfig(t *testing.T) {
	ipamConfig, namespace, err := toWhereaboutsIPAMConfig("tenant-a/pod-a", &apis.WhereaboutsConfig{
		Range:       "10.0.0.0/24",
		RangeStart:  "10.0.0.10",
		RangeEnd:    "10.0.0.20",
		Exclude:     []string{"10.0.0.11"},
		NetworkName: "red-network",
	})
	if err != nil {
		t.Fatalf("toWhereaboutsIPAMConfig() unexpected error: %v", err)
	}
	if namespace != whereaboutsNamespace {
		t.Fatalf("namespace = %q, want %q", namespace, whereaboutsNamespace)
	}
	if ipamConfig.PodNamespace != "tenant-a" || ipamConfig.PodName != "pod-a" {
		t.Fatalf("unexpected pod identity: %+v", ipamConfig)
	}
	if !ipamConfig.OverlappingRanges {
		t.Fatalf("OverlappingRanges should be enabled")
	}
	if ipamConfig.NetworkName != "red-network" {
		t.Fatalf("NetworkName = %q, want red-network", ipamConfig.NetworkName)
	}
}

func TestSplitWhereaboutsPodRef(t *testing.T) {
	namespace, name, err := splitWhereaboutsPodRef("tenant-a/pod-a")
	if err != nil {
		t.Fatalf("splitWhereaboutsPodRef() unexpected error: %v", err)
	}
	if namespace != "tenant-a" || name != "pod-a" {
		t.Fatalf("splitWhereaboutsPodRef() = %s/%s", namespace, name)
	}

	if _, _, err := splitWhereaboutsPodRef("broken"); err == nil {
		t.Fatalf("splitWhereaboutsPodRef() expected error for invalid podRef")
	}
}

func TestBuildIPAMContainerID(t *testing.T) {
	id := buildIPAMContainerID("claim-uid", "pod-uid", "dev0")
	if id != "claim-uid/pod-uid/dev0" {
		t.Fatalf("unexpected containerID %q", id)
	}

	_ = whereaboutstypes.Allocate
}
