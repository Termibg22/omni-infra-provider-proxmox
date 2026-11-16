// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"time"

	"github.com/luthermonson/go-proxmox"
)

type NodeStatus = nodeStatus

var (
	ErrTaskFailed = errTaskFailed
	TaskFailure   = taskFailure
)

func PickNode(nodes []NodeStatus) NodeStatus {
	return pickNode(nodes)
}

func BuildTagsOption(userTags []string, machineRequestSet string) (string, bool) {
	return buildTagsOption(userTags, machineRequestSet)
}

func BuildFirmwareOptions(data Data, selectedStorage string) map[string]any {
	options := buildFirmwareOptions(data, selectedStorage)

	result := map[string]any{}
	for _, option := range options {
		result[option.Name] = option.Value
	}

	return result
}

func BuildDiskOptions(disks []Disk, storages []string) map[string]any {
	options := buildDiskOptions(disks, storages)

	result := map[string]any{}
	for _, option := range options {
		result[option.Name] = option.Value
	}

	return result
}

func BuildNICOptions(nics []NIC) map[string]any {
	options := buildNICOptions(nics)

	result := map[string]any{}
	for _, option := range options {
		result[option.Name] = option.Value
	}

	return result
}

func MatchStorage(nodeName string, storages proxmox.Storages, selector string) (string, error) {
	return matchStorage(nodeName, storages, selector)
}

func BuildCloudInitNetworkConfig(data Data, nics []NIC, macByIndex map[int]string, allocatedIPByIndex map[int]string) (string, error) {
	return buildCloudInitNetworkConfig(data, nics, macByIndex, allocatedIPByIndex)
}

func ExtractVMMACAddresses(nets map[string]string, count int) map[int]string {
	return extractVMMACAddresses(nets, count)
}

type IPPoolAllocator = ipPoolAllocator

func NewIPPoolAllocator() *IPPoolAllocator {
	return newIPPoolAllocator()
}

func (p *Provisioner) AllocateVMID(ctx context.Context, cluster *proxmox.Cluster, data Data) (int, error) {
	return p.allocateVMID(ctx, cluster, data)
}

func (p *Provisioner) ResolveNodeGroupNodes(ctx context.Context, groupName string) ([]string, error) {
	return p.resolveNodeGroupNodes(ctx, groupName)
}

func (p *Provisioner) FilterNodesByGroupAndTopology(ctx context.Context, nodes proxmox.NodeStatuses, data Data) (proxmox.NodeStatuses, error) {
	return p.filterNodesByGroupAndTopology(ctx, nil, nodes, data)
}

func BuildUSBDeviceOptions(devices []USBDevice) map[string]any {
	options := buildUSBDeviceOptions(devices)

	result := map[string]any{}
	for _, option := range options {
		result[option.Name] = option.Value
	}

	return result
}

func PoolCreateDecision(exists bool, poolID, machineRequestSet string, autoCreate ...*bool) (bool, error) {
	return poolCreateDecision(exists, poolID, machineRequestSet, autoCreate...)
}

type Scheduler = scheduler

func NewScheduler() *Scheduler {
	return newScheduler()
}

func NewSchedulerWithClock(now func() time.Time, ttl time.Duration) *Scheduler {
	return newSchedulerWithClock(now, ttl)
}

func (s *scheduler) Pick(nodes []NodeStatus, set, requestID string, memory uint64, strategy string, materialized map[string]struct{}) NodeStatus {
	parsed, _ := parseStrategy(strategy) //nolint:errcheck

	return s.pick(nodes, set, requestID, memory, parsed, materialized)
}

func ParseStrategy(s string) (string, error) {
	parsed, err := parseStrategy(s)

	return string(parsed), err
}

func (s *scheduler) Release(requestID string) {
	s.release(requestID)
}

func ShouldCountSetVMs(data Data, hasSet bool) bool {
	return shouldCountSetVMs(data, hasSet)
}

const MaxStartAttempts = maxStartAttempts

func SetTaskPollInterval(t interface{ Cleanup(func()) }, d time.Duration) {
	prev := taskPollInterval
	taskPollInterval = d

	t.Cleanup(func() { taskPollInterval = prev })
}
