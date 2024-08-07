/*
Copyright 2022 The Katalyst Authors.

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

package dynamicpolicy

import (
	"context"
	"fmt"
	"math"
	"sort"

	v1 "k8s.io/api/core/v1"
	pluginapi "k8s.io/kubelet/pkg/apis/resourceplugin/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"

	apiconsts "github.com/kubewharf/katalyst-api/pkg/consts"
	cpuconsts "github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/consts"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/state"
	cpuutil "github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/util"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/util"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	qosutil "github.com/kubewharf/katalyst-core/pkg/util/qos"
)

func (p *DynamicPolicy) sharedCoresHintHandler(ctx context.Context,
	req *pluginapi.ResourceRequest,
) (*pluginapi.ResourceHintsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("got nil request")
	}

	if !qosutil.AnnotationsIndicateNUMABinding(req.Annotations) {
		return util.PackResourceHintsResponse(req, string(v1.ResourceCPU),
			map[string]*pluginapi.ListOfTopologyHints{
				string(v1.ResourceCPU): nil, // indicates that there is no numa preference
			})
	}

	return p.sharedCoresWithNUMABindingHintHandler(ctx, req)
}

func (p *DynamicPolicy) reclaimedCoresHintHandler(ctx context.Context,
	req *pluginapi.ResourceRequest,
) (*pluginapi.ResourceHintsResponse, error) {
	return p.sharedCoresHintHandler(ctx, req)
}

func (p *DynamicPolicy) dedicatedCoresHintHandler(ctx context.Context,
	req *pluginapi.ResourceRequest,
) (*pluginapi.ResourceHintsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("dedicatedCoresHintHandler got nil req")
	}

	switch req.Annotations[apiconsts.PodAnnotationMemoryEnhancementNumaBinding] {
	case apiconsts.PodAnnotationMemoryEnhancementNumaBindingEnable:
		return p.dedicatedCoresWithNUMABindingHintHandler(ctx, req)
	default:
		return p.dedicatedCoresWithoutNUMABindingHintHandler(ctx, req)
	}
}

func (p *DynamicPolicy) dedicatedCoresWithNUMABindingHintHandler(_ context.Context,
	req *pluginapi.ResourceRequest,
) (*pluginapi.ResourceHintsResponse, error) {
	// currently, we set cpuset of sidecar to the cpuset of its main container,
	// so there is no numa preference here.
	if req.ContainerType == pluginapi.ContainerType_SIDECAR {
		return util.PackResourceHintsResponse(req, string(v1.ResourceCPU),
			map[string]*pluginapi.ListOfTopologyHints{
				string(v1.ResourceCPU): nil, // indicates that there is no numa preference
			})
	}

	reqInt, _, err := util.GetQuantityFromResourceReq(req)
	if err != nil {
		return nil, fmt.Errorf("getReqQuantityFromResourceReq failed with error: %v", err)
	}

	machineState := p.state.GetMachineState()
	var hints map[string]*pluginapi.ListOfTopologyHints

	allocationInfo := p.state.GetAllocationInfo(req.PodUid, req.ContainerName)
	if allocationInfo != nil {
		hints = cpuutil.RegenerateHints(allocationInfo, reqInt)

		// regenerateHints failed. need to clear container record and re-calculate.
		if hints == nil {
			podEntries := p.state.GetPodEntries()
			delete(podEntries[req.PodUid], req.ContainerName)
			if len(podEntries[req.PodUid]) == 0 {
				delete(podEntries, req.PodUid)
			}

			var err error
			machineState, err = generateMachineStateFromPodEntries(p.machineInfo.CPUTopology, podEntries)
			if err != nil {
				general.Errorf("pod: %s/%s, container: %s GenerateMachineStateFromPodEntries failed with error: %v",
					req.PodNamespace, req.PodName, req.ContainerName, err)
				return nil, fmt.Errorf("GenerateMachineStateFromPodEntries failed with error: %v", err)
			}
		}
	}

	// if hints exists in extra state-file, prefer to use them
	if hints == nil {
		availableNUMAs := machineState.GetFilteredNUMASet(state.CheckNUMABinding)

		var extraErr error
		hints, extraErr = util.GetHintsFromExtraStateFile(req.PodName, string(v1.ResourceCPU), p.extraStateFileAbsPath, availableNUMAs)
		if extraErr != nil {
			general.Infof("pod: %s/%s, container: %s GetHintsFromExtraStateFile failed with error: %v",
				req.PodNamespace, req.PodName, req.ContainerName, extraErr)
		}
	}

	// otherwise, calculate hint for container without allocated memory
	if hints == nil {
		var calculateErr error
		// calculate hint for container without allocated cpus
		hints, calculateErr = p.calculateHints(reqInt, machineState, req.Annotations)
		if calculateErr != nil {
			return nil, fmt.Errorf("calculateHints failed with error: %v", calculateErr)
		}
	}

	return util.PackResourceHintsResponse(req, string(v1.ResourceCPU), hints)
}

func (p *DynamicPolicy) dedicatedCoresWithoutNUMABindingHintHandler(_ context.Context,
	_ *pluginapi.ResourceRequest,
) (*pluginapi.ResourceHintsResponse, error) {
	// todo: support dedicated_cores without NUMA binding
	return nil, fmt.Errorf("not support dedicated_cores without NUMA binding")
}

// calculateHints is a helper function to calculate the topology hints
// with the given container requests.
func (p *DynamicPolicy) calculateHints(reqInt int, machineState state.NUMANodeMap,
	reqAnnotations map[string]string,
) (map[string]*pluginapi.ListOfTopologyHints, error) {
	numaNodes := make([]int, 0, len(machineState))
	for numaNode := range machineState {
		numaNodes = append(numaNodes, numaNode)
	}
	sort.Ints(numaNodes)

	hints := map[string]*pluginapi.ListOfTopologyHints{
		string(v1.ResourceCPU): {
			Hints: []*pluginapi.TopologyHint{},
		},
	}

	minNUMAsCountNeeded, _, err := util.GetNUMANodesCountToFitCPUReq(reqInt, p.machineInfo.CPUTopology)
	if err != nil {
		return nil, fmt.Errorf("GetNUMANodesCountToFitCPUReq failed with error: %v", err)
	}

	// because it's hard to control memory allocation accurately,
	// we only support numa_binding but not exclusive container with request smaller than 1 NUMA
	if qosutil.AnnotationsIndicateNUMABinding(reqAnnotations) &&
		!qosutil.AnnotationsIndicateNUMAExclusive(reqAnnotations) &&
		minNUMAsCountNeeded > 1 {
		return nil, fmt.Errorf("NUMA not exclusive binding container has request larger than 1 NUMA")
	}

	numaPerSocket, err := p.machineInfo.NUMAsPerSocket()
	if err != nil {
		return nil, fmt.Errorf("NUMAsPerSocket failed with error: %v", err)
	}

	bitmask.IterateBitMasks(numaNodes, func(mask bitmask.BitMask) {
		maskCount := mask.Count()
		if maskCount < minNUMAsCountNeeded {
			return
		} else if qosutil.AnnotationsIndicateNUMABinding(reqAnnotations) &&
			!qosutil.AnnotationsIndicateNUMAExclusive(reqAnnotations) &&
			maskCount > 1 {
			// because it's hard to control memory allocation accurately,
			// we only support numa_binding but not exclusive container with request smaller than 1 NUMA
			return
		}

		maskBits := mask.GetBits()
		numaCountNeeded := mask.Count()

		allAvailableCPUsInMask := machine.NewCPUSet()
		for _, nodeID := range maskBits {
			if machineState[nodeID] == nil {
				general.Warningf("NUMA: %d has nil state", nodeID)
				return
			} else if qosutil.AnnotationsIndicateNUMAExclusive(reqAnnotations) && machineState[nodeID].AllocatedCPUSet.Size() > 0 {
				general.Warningf("numa_exclusive container skip mask: %s with NUMA: %d allocated: %d",
					mask.String(), nodeID, machineState[nodeID].AllocatedCPUSet.Size())
				return
			}

			allAvailableCPUsInMask = allAvailableCPUsInMask.Union(machineState[nodeID].GetAvailableCPUSet(p.reservedCPUs))
		}

		if allAvailableCPUsInMask.Size() < reqInt {
			general.InfofV(4, "available cpuset: %s of size: %d excluding NUMA binding pods which is smaller than request: %d",
				allAvailableCPUsInMask.String(), allAvailableCPUsInMask.Size(), reqInt)
			return
		}

		crossSockets, err := machine.CheckNUMACrossSockets(maskBits, p.machineInfo.CPUTopology)
		if err != nil {
			general.Errorf("CheckNUMACrossSockets failed with error: %v", err)
			return
		} else if numaCountNeeded <= numaPerSocket && crossSockets {
			general.InfofV(4, "needed: %d; min-needed: %d; NUMAs: %v cross sockets with numaPerSocket: %d",
				numaCountNeeded, minNUMAsCountNeeded, maskBits, numaPerSocket)
			return
		}

		hints[string(v1.ResourceCPU)].Hints = append(hints[string(v1.ResourceCPU)].Hints, &pluginapi.TopologyHint{
			Nodes:     machine.MaskToUInt64Array(mask),
			Preferred: len(maskBits) == minNUMAsCountNeeded,
		})
	})

	return hints, nil
}

func (p *DynamicPolicy) sharedCoresWithNUMABindingHintHandler(_ context.Context,
	req *pluginapi.ResourceRequest,
) (*pluginapi.ResourceHintsResponse, error) {
	// currently, we set cpuset of sidecar to the cpuset of its main container,
	// so there is no numa preference here.
	if req.ContainerType == pluginapi.ContainerType_SIDECAR {
		return util.PackResourceHintsResponse(req, string(v1.ResourceCPU),
			map[string]*pluginapi.ListOfTopologyHints{
				string(v1.ResourceCPU): nil, // indicates that there is no numa preference
			})
	}

	reqInt, _, err := util.GetQuantityFromResourceReq(req)
	if err != nil {
		return nil, fmt.Errorf("getReqQuantityFromResourceReq failed with error: %v", err)
	}

	machineState := p.state.GetMachineState()
	podEntries := p.state.GetPodEntries()

	var hints map[string]*pluginapi.ListOfTopologyHints

	allocationInfo := p.state.GetAllocationInfo(req.PodUid, req.ContainerName)
	if allocationInfo != nil {
		hints = cpuutil.RegenerateHints(allocationInfo, reqInt)

		// regenerateHints failed. need to clear container record and re-calculate.
		if hints == nil {
			delete(podEntries[req.PodUid], req.ContainerName)
			if len(podEntries[req.PodUid]) == 0 {
				delete(podEntries, req.PodUid)
			}

			var err error
			// [TODO]: generateMachineStateFromPodEntries adapts to shared_cores with numa_binding
			machineState, err = generateMachineStateFromPodEntries(p.machineInfo.CPUTopology, podEntries)
			if err != nil {
				general.Errorf("pod: %s/%s, container: %s GenerateMachineStateFromPodEntries failed with error: %v",
					req.PodNamespace, req.PodName, req.ContainerName, err)
				return nil, fmt.Errorf("GenerateMachineStateFromPodEntries failed with error: %v", err)
			}
		}
	}

	if hints == nil {
		var calculateErr error
		hints, calculateErr = p.calculateHintsForNUMABindingSharedCores(reqInt, podEntries, machineState, req.Annotations)
		if calculateErr != nil {
			return nil, fmt.Errorf("calculateHintsForNUMABindingSharedCores failed with error: %v", calculateErr)
		}
	}

	return util.PackResourceHintsResponse(req, string(v1.ResourceCPU), hints)
}

func (p *DynamicPolicy) populateHintsByPreferPolicy(numaNodes []int, preferPolicy string,
	hints map[string]*pluginapi.ListOfTopologyHints, machineState state.NUMANodeMap, reqInt int,
) {
	preferIndexes, maxLeft, minLeft := []int{}, -1, math.MaxInt

	for _, nodeID := range numaNodes {
		availableCPUQuantity := machineState[nodeID].GetAvailableCPUQuantity(p.reservedCPUs)

		if availableCPUQuantity < reqInt {
			general.Warningf("numa_binding shared_cores container skip NUMA: %d available: %d",
				nodeID, availableCPUQuantity)
			continue
		}

		hints[string(v1.ResourceCPU)].Hints = append(hints[string(v1.ResourceCPU)].Hints, &pluginapi.TopologyHint{
			Nodes: []uint64{uint64(nodeID)},
		})

		curLeft := availableCPUQuantity - reqInt

		general.Infof("NUMA: %d, left cpu quantity: %d", nodeID, curLeft)

		if preferPolicy == cpuconsts.CPUNUMAHintPreferPolicyPacking {
			if curLeft < minLeft {
				minLeft = curLeft
				preferIndexes = []int{len(hints[string(v1.ResourceCPU)].Hints) - 1}
			} else if curLeft == minLeft {
				preferIndexes = append(preferIndexes, len(hints[string(v1.ResourceCPU)].Hints)-1)
			}
		} else {
			if curLeft > maxLeft {
				maxLeft = curLeft
				preferIndexes = []int{len(hints[string(v1.ResourceCPU)].Hints) - 1}
			} else if curLeft == maxLeft {
				preferIndexes = append(preferIndexes, len(hints[string(v1.ResourceCPU)].Hints)-1)
			}
		}
	}

	if len(preferIndexes) >= 0 {
		for _, preferIndex := range preferIndexes {
			hints[string(v1.ResourceCPU)].Hints[preferIndex].Preferred = true
		}
	}
}

func (p *DynamicPolicy) filterNUMANodesByHintPreferLowThreshold(reqInt int,
	machineState state.NUMANodeMap, numaNodes []int,
) ([]int, []int) {
	filteredNUMANodes := make([]int, 0, len(numaNodes))
	filteredOutNUMANodes := make([]int, 0, len(numaNodes))

	for _, nodeID := range numaNodes {
		availableCPUQuantity := machineState[nodeID].GetAvailableCPUQuantity(p.reservedCPUs)
		allocatableCPUQuantity := machineState[nodeID].GetFilteredDefaultCPUSet(nil, nil).Difference(p.reservedCPUs).Size()

		if allocatableCPUQuantity == 0 {
			general.Warningf("numa: %d allocatable cpu quantity is zero", nodeID)
			continue
		}

		availableRatio := float64(availableCPUQuantity) / float64(allocatableCPUQuantity)

		general.Infof("NUMA: %d, availableCPUQuantity: %d, allocatableCPUQuantity: %d, availableRatio: %.2f, cpuNUMAHintPreferLowThreshold:%.2f",
			nodeID, availableCPUQuantity, allocatableCPUQuantity, availableRatio, p.cpuNUMAHintPreferLowThreshold)

		if availableRatio >= p.cpuNUMAHintPreferLowThreshold {
			filteredNUMANodes = append(filteredNUMANodes, nodeID)
		} else {
			filteredOutNUMANodes = append(filteredOutNUMANodes, nodeID)
		}
	}

	return filteredNUMANodes, filteredOutNUMANodes
}

func (p *DynamicPolicy) filterNUMANodesByNonBindingSharedRequestedQuantity(nonBindingSharedRequestedQuantity,
	nonBindingNUMAsCPUQuantity int,
	nonBindingNUMAs machine.CPUSet,
	machineState state.NUMANodeMap, numaNodes []int,
) []int {
	filteredNUMANodes := make([]int, 0, len(numaNodes))

	for _, nodeID := range numaNodes {
		if nonBindingNUMAs.Contains(nodeID) {
			allocatableCPUQuantity := machineState[nodeID].GetFilteredDefaultCPUSet(nil, nil).Difference(p.reservedCPUs).Size()

			// take this non-binding NUMA for candicate shared_cores with numa_binding,
			// won't cause normal shared_cores in short supply
			if nonBindingNUMAsCPUQuantity-allocatableCPUQuantity >= nonBindingSharedRequestedQuantity {
				filteredNUMANodes = append(filteredNUMANodes, nodeID)
			} else {
				general.Infof("filter out NUMA: %d since taking it will cause normal shared_cores in short supply;"+
					" nonBindingNUMAsCPUQuantity: %d, targetNUMAAllocatableCPUQuantity: %d, nonBindingSharedRequestedQuantity: %d",
					nodeID, nonBindingNUMAsCPUQuantity, allocatableCPUQuantity, nonBindingSharedRequestedQuantity)
			}
		} else {
			filteredNUMANodes = append(filteredNUMANodes, nodeID)
		}
	}

	return filteredNUMANodes
}

func (p *DynamicPolicy) calculateHintsForNUMABindingSharedCores(reqInt int, podEntries state.PodEntries,
	machineState state.NUMANodeMap,
	reqAnnotations map[string]string,
) (map[string]*pluginapi.ListOfTopologyHints, error) {
	nonBindingNUMAsCPUQuantity := machineState.GetFilteredAvailableCPUSet(p.reservedCPUs, nil, state.CheckNUMABinding).Size()
	nonBindingNUMAs := machineState.GetFilteredNUMASet(state.CheckNUMABinding)
	nonBindingSharedRequestedQuantity := state.GetNonBindingSharedRequestedQuantityFromPodEntries(podEntries)

	numaNodes := p.filterNUMANodesByNonBindingSharedRequestedQuantity(nonBindingSharedRequestedQuantity,
		nonBindingNUMAsCPUQuantity, nonBindingNUMAs, machineState,
		machineState.GetFilteredNUMASetWithAnnotations(state.CheckNUMABindingSharedCoresAntiAffinity, reqAnnotations).ToSliceInt())

	hints := map[string]*pluginapi.ListOfTopologyHints{
		string(v1.ResourceCPU): {
			Hints: []*pluginapi.TopologyHint{},
		},
	}

	minNUMAsCountNeeded, _, err := util.GetNUMANodesCountToFitCPUReq(reqInt, p.machineInfo.CPUTopology)
	if err != nil {
		return nil, fmt.Errorf("GetNUMANodesCountToFitCPUReq failed with error: %v", err)
	}

	// if a numa_binding shared_cores has request larger than 1 NUMA,
	// its performance may degrade to be like normal shared_cores
	if minNUMAsCountNeeded > 1 {
		return nil, fmt.Errorf("numa_binding shared_cores container has request larger than 1 NUMA")
	}
	switch p.cpuNUMAHintPreferPolicy {
	case cpuconsts.CPUNUMAHintPreferPolicyPacking, cpuconsts.CPUNUMAHintPreferPolicySpreading:
		general.Infof("apply %s policy on NUMAs: %+v", p.cpuNUMAHintPreferPolicy, numaNodes)
		p.populateHintsByPreferPolicy(numaNodes, p.cpuNUMAHintPreferPolicy, hints, machineState, reqInt)
	case cpuconsts.CPUNUMAHintPreferPolicyDynamicPacking:
		filteredNUMANodes, filteredOutNUMANodes := p.filterNUMANodesByHintPreferLowThreshold(reqInt, machineState, numaNodes)

		if len(filteredNUMANodes) > 0 {
			general.Infof("dynamically apply packing policy on NUMAs: %+v", filteredNUMANodes)
			p.populateHintsByPreferPolicy(filteredNUMANodes, cpuconsts.CPUNUMAHintPreferPolicyPacking, hints, machineState, reqInt)
			p.populateNotPreferredHintsByAvailableNUMANodes(filteredOutNUMANodes, hints)
		} else {
			general.Infof("empty filteredNUMANodes, dynamically apply spreading policy on NUMAs: %+v", numaNodes)
			p.populateHintsByPreferPolicy(numaNodes, cpuconsts.CPUNUMAHintPreferPolicySpreading, hints, machineState, reqInt)
		}
	default:
		general.Infof("unknown policy: %s, apply default spreading policy on NUMAs: %+v", p.cpuNUMAHintPreferPolicy, numaNodes)
		p.populateHintsByPreferPolicy(numaNodes, cpuconsts.CPUNUMAHintPreferPolicySpreading, hints, machineState, reqInt)
	}

	return hints, nil
}

func (p *DynamicPolicy) populateNotPreferredHintsByAvailableNUMANodes(numaNodes []int,
	hints map[string]*pluginapi.ListOfTopologyHints,
) {
	for _, nodeID := range numaNodes {
		hints[string(v1.ResourceCPU)].Hints = append(hints[string(v1.ResourceCPU)].Hints, &pluginapi.TopologyHint{
			Nodes: []uint64{uint64(nodeID)},
		})
	}
}
