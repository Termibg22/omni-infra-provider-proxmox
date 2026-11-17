// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/luthermonson/go-proxmox"
	"go.uber.org/zap"
)

// DefaultCPUTypes lists common QEMU/Proxmox CPU models offered before custom models.
var DefaultCPUTypes = []string{
	"x86-64-v2-AES",
	"x86-64-v3",
	"x86-64-v4",
	"host",
	"max",
	"kvm64",
	"qemu64",
}

// NodeTopology describes the storage, networks and hardware choices available on a node.
// PCIMappings and USBMappings include resource mapping IDs and node/address physical device selections.
type NodeTopology struct {
	DiskStorages   []string
	ISOStorages    []string
	NetworkBridges []string
	PCIMappings    []string
	USBMappings    []string
}

// ClusterOptions holds the real resources and per-node/per-group topology discovered from a Proxmox cluster
// used to populate enum selectors and cascading dependencies in the Omni UI JSON Schema.
type ClusterOptions struct {
	NodeTopologies    map[string]NodeTopology
	NodeGroups        map[string][]string
	NetworkBridges    []string
	Nodes             []string
	DiskStorages      []string
	ISOStorages       []string
	Pools             []string
	PCIMappings       []string
	USBMappings       []string
	HARules           []string
	HAGroups          []string
	CPUTypes          []string
	SecurityGroups    []string
	Templates         []string
	DiscoveryComplete bool
}

// TopologyForNodes merges the capabilities (storages, bridges, PCI/USB mappings) available across the given node names.
func (o ClusterOptions) TopologyForNodes(nodeNames []string) NodeTopology {
	if len(o.NodeTopologies) == 0 || len(nodeNames) == 0 {
		return NodeTopology{
			DiskStorages:   o.DiskStorages,
			ISOStorages:    o.ISOStorages,
			NetworkBridges: o.NetworkBridges,
			PCIMappings:    o.PCIMappings,
			USBMappings:    o.USBMappings,
		}
	}

	diskSet := map[string]struct{}{}
	isoSet := map[string]struct{}{}
	bridgeSet := map[string]struct{}{}
	pciSet := map[string]struct{}{}
	usbSet := map[string]struct{}{}

	for _, name := range nodeNames {
		topo, ok := o.NodeTopologies[name]
		if !ok {
			continue
		}

		for _, s := range topo.DiskStorages {
			diskSet[s] = struct{}{}
		}

		for _, s := range topo.ISOStorages {
			isoSet[s] = struct{}{}
		}

		for _, b := range topo.NetworkBridges {
			bridgeSet[b] = struct{}{}
		}

		for _, p := range topo.PCIMappings {
			pciSet[p] = struct{}{}
		}

		for _, u := range topo.USBMappings {
			usbSet[u] = struct{}{}
		}
	}

	return NodeTopology{
		DiskStorages:   slices.Sorted(maps.Keys(diskSet)),
		ISOStorages:    slices.Sorted(maps.Keys(isoSet)),
		NetworkBridges: slices.Sorted(maps.Keys(bridgeSet)),
		PCIMappings:    slices.Sorted(maps.Keys(pciSet)),
		USBMappings:    slices.Sorted(maps.Keys(usbSet)),
	}
}

type nodeTopologySets struct {
	diskStorages   map[string]struct{}
	isoStorages    map[string]struct{}
	networkBridges map[string]struct{}
	pciMappings    map[string]struct{}
	usbMappings    map[string]struct{}
}

type discoveryAccumulator struct {
	nodesSet          map[string]struct{}
	diskStorageSet    map[string]struct{}
	isoStorageSet     map[string]struct{}
	bridgeSet         map[string]struct{}
	poolSet           map[string]struct{}
	pciSet            map[string]struct{}
	usbSet            map[string]struct{}
	haRuleSet         map[string]struct{}
	haGroupSet        map[string]struct{}
	cpuModelSet       map[string]struct{}
	secGroupSet       map[string]struct{}
	templateSet       map[string]struct{}
	nodeGroups        map[string][]string
	perNode           map[string]*nodeTopologySets
	sdnZoneNodes      map[string][]string
	cpuTypes          []string
	discoveryComplete bool
}

func newDiscoveryAccumulator(customGroups map[string][]string) *discoveryAccumulator {
	acc := &discoveryAccumulator{
		nodesSet:       map[string]struct{}{},
		diskStorageSet: map[string]struct{}{},
		isoStorageSet:  map[string]struct{}{},
		bridgeSet:      map[string]struct{}{},
		poolSet:        map[string]struct{}{},
		pciSet:         map[string]struct{}{},
		usbSet:         map[string]struct{}{},
		haRuleSet:      map[string]struct{}{},
		haGroupSet:     map[string]struct{}{},
		cpuModelSet:    map[string]struct{}{},
		secGroupSet:    map[string]struct{}{},
		templateSet:    map[string]struct{}{},
		nodeGroups:     map[string][]string{},
		perNode:        map[string]*nodeTopologySets{},
		sdnZoneNodes:   map[string][]string{},
		cpuTypes:       slices.Clone(DefaultCPUTypes),
	}

	for _, c := range DefaultCPUTypes {
		acc.cpuModelSet[c] = struct{}{}
	}

	for groupName, groupNodes := range customGroups {
		acc.addNodeGroup(groupName, groupNodes)
	}

	return acc
}

func (a *discoveryAccumulator) ensureNode(nodeName string) *nodeTopologySets {
	if nodeName == "" {
		return nil
	}

	nt, ok := a.perNode[nodeName]
	if !ok {
		nt = &nodeTopologySets{
			diskStorages:   map[string]struct{}{},
			isoStorages:    map[string]struct{}{},
			networkBridges: map[string]struct{}{},
			pciMappings:    map[string]struct{}{},
			usbMappings:    map[string]struct{}{},
		}
		a.perNode[nodeName] = nt
	}

	return nt
}

func (a *discoveryAccumulator) addNodeGroup(name string, nodes []string) {
	name = strings.TrimSpace(name)
	if name == "" || len(nodes) == 0 {
		return
	}

	seen := map[string]struct{}{}

	for _, n := range nodes {
		// Handle Proxmox HAGroup "node:priority" syntax
		nodeName, _, _ := strings.Cut(strings.TrimSpace(n), ":")
		if nodeName != "" {
			seen[nodeName] = struct{}{}
			a.ensureNode(nodeName)
		}
	}

	if len(seen) > 0 {
		a.nodeGroups[name] = slices.Sorted(maps.Keys(seen))
	}
}

// DiscoverClusterOptions queries the Proxmox cluster for nodes, node groups, storages, network bridges,
// SDN VNets/Zones, resource pools, hardware mappings, VM templates, firewall security groups, CPU models, and HA rules.
func DiscoverClusterOptions(ctx context.Context, px *proxmox.Client, logger *zap.Logger, customNodeGroups ...map[string][]string) ClusterOptions {
	if logger == nil {
		logger = zap.NewNop()
	}

	var customGroups map[string][]string
	if len(customNodeGroups) > 0 {
		customGroups = customNodeGroups[0]
	}

	if px == nil {
		return ClusterOptions{
			CPUTypes:   slices.Clone(DefaultCPUTypes),
			NodeGroups: customGroups,
		}
	}

	acc := newDiscoveryAccumulator(customGroups)
	cluster := new(proxmox.Cluster).New(px)

	acc.discoverFromClusterResources(ctx, cluster)
	acc.discoverClusterStoragesAndPools(ctx, px, logger)
	acc.discoverFromNodes(ctx, px, logger)
	acc.discoverClusterMappingsAndModels(ctx, cluster, logger)

	opts := acc.buildOptions()
	if !opts.DiscoveryComplete {
		opts.Nodes = nil
		opts.NodeGroups = nil
		opts.DiskStorages = nil
		opts.ISOStorages = nil
		opts.NodeTopologies = nil
		opts.PCIMappings = nil
		opts.USBMappings = nil
	}

	logger.Info("discovered Proxmox cluster options for UI schema",
		zap.Strings("nodes", opts.Nodes),
		zap.Int("nodeGroups", len(opts.NodeGroups)),
		zap.Strings("diskStorages", opts.DiskStorages),
		zap.Strings("isoStorages", opts.ISOStorages),
		zap.Strings("networkBridges", opts.NetworkBridges),
		zap.Strings("pools", opts.Pools),
		zap.Strings("pciMappings", opts.PCIMappings),
		zap.Strings("usbMappings", opts.USBMappings),
		zap.Strings("haRules", opts.HARules),
		zap.Strings("securityGroups", opts.SecurityGroups),
		zap.Strings("templates", opts.Templates),
	)

	return opts
}

func (a *discoveryAccumulator) buildOptions() ClusterOptions {
	allNodes := slices.Sorted(maps.Keys(a.nodesSet))
	nodeTopologies := make(map[string]NodeTopology, len(a.perNode))

	for nodeName, nt := range a.perNode {
		if _, current := a.nodesSet[nodeName]; !current {
			continue
		}

		nodeTopologies[nodeName] = NodeTopology{
			DiskStorages:   slices.Sorted(maps.Keys(nt.diskStorages)),
			ISOStorages:    slices.Sorted(maps.Keys(nt.isoStorages)),
			NetworkBridges: slices.Sorted(maps.Keys(nt.networkBridges)),
			PCIMappings:    slices.Sorted(maps.Keys(nt.pciMappings)),
			USBMappings:    slices.Sorted(maps.Keys(nt.usbMappings)),
		}
	}

	groups := map[string][]string{}

	for name, members := range a.nodeGroups {
		for _, member := range members {
			if _, exists := a.nodesSet[member]; exists {
				groups[name] = append(groups[name], member)
			}
		}
	}

	return ClusterOptions{
		DiscoveryComplete: a.discoveryComplete,
		Nodes:             allNodes,
		NodeGroups:        groups,
		NodeTopologies:    nodeTopologies,
		DiskStorages:      slices.Sorted(maps.Keys(a.diskStorageSet)),
		ISOStorages:       slices.Sorted(maps.Keys(a.isoStorageSet)),
		NetworkBridges:    slices.Sorted(maps.Keys(a.bridgeSet)),
		Pools:             slices.Sorted(maps.Keys(a.poolSet)),
		PCIMappings:       slices.Sorted(maps.Keys(a.pciSet)),
		USBMappings:       slices.Sorted(maps.Keys(a.usbSet)),
		HARules:           slices.Sorted(maps.Keys(a.haRuleSet)),
		HAGroups:          slices.Sorted(maps.Keys(a.haGroupSet)),
		SecurityGroups:    slices.Sorted(maps.Keys(a.secGroupSet)),
		Templates:         slices.Sorted(maps.Keys(a.templateSet)),
		CPUTypes:          a.cpuTypes,
	}
}

func (a *discoveryAccumulator) discoverFromClusterResources(ctx context.Context, cluster *proxmox.Cluster) {
	resources, err := cluster.Resources(ctx)
	if err != nil {
		return
	}

	for _, res := range resources {
		if res == nil {
			continue
		}

		a.recordClusterResource(res)
	}
}

func (a *discoveryAccumulator) recordClusterResource(res *proxmox.ClusterResource) {
	switch res.Type {
	case "node":
		if res.Node != "" {
			a.nodesSet[res.Node] = struct{}{}
		}

		a.ensureNode(res.Node)
	case "pool":
		if res.Pool != "" {
			a.poolSet[res.Pool] = struct{}{}
		}
	case "storage":
		if res.Storage != "" && res.Status != "disabled" {
			classifyStorageContent(res.Storage, res.Content, a.diskStorageSet, a.isoStorageSet)

			if nt := a.ensureNode(res.Node); nt != nil {
				classifyStorageContent(res.Storage, res.Content, nt.diskStorages, nt.isoStorages)
			}
		}
	case "sdn":
		if res.ID != "" {
			parts := strings.Split(res.ID, "/")
			vnet := parts[len(parts)-1]
			a.bridgeSet[vnet] = struct{}{}
		}
	case "qemu":
		if res.Template == 1 && res.VMID > 0 {
			label := fmt.Sprintf("%d", res.VMID)
			if res.Name != "" {
				label = fmt.Sprintf("%d (%s)", res.VMID, res.Name)
			}

			a.templateSet[label] = struct{}{}
		}
	}
}

func (a *discoveryAccumulator) discoverClusterStoragesAndPools(ctx context.Context, px *proxmox.Client, logger *zap.Logger) {
	if storages, err := px.ClusterStorages(ctx); err != nil {
		logger.Debug("failed to list cluster storages for schema discovery", zap.Error(err))
	} else {
		for _, st := range storages {
			a.recordClusterStorage(st)
		}
	}

	if pools, err := px.Pools(ctx); err != nil {
		logger.Debug("failed to list proxmox pools for schema discovery", zap.Error(err))
	} else {
		for _, pool := range pools {
			if pool != nil && pool.PoolID != "" {
				a.poolSet[pool.PoolID] = struct{}{}
			}
		}
	}
}

func (a *discoveryAccumulator) recordClusterStorage(st *proxmox.ClusterStorage) {
	if st == nil || st.Storage == "" {
		return
	}

	classifyStorageContent(st.Storage, st.Content, a.diskStorageSet, a.isoStorageSet)

	if st.Nodes == "" {
		return
	}

	for n := range strings.SplitSeq(st.Nodes, ",") {
		if nt := a.ensureNode(strings.TrimSpace(n)); nt != nil {
			classifyStorageContent(st.Storage, st.Content, nt.diskStorages, nt.isoStorages)
		}
	}
}

func (a *discoveryAccumulator) discoverFromNodes(ctx context.Context, px *proxmox.Client, logger *zap.Logger) {
	nodes, err := px.Nodes(ctx)
	if err != nil {
		logger.Warn("failed to list proxmox nodes for schema discovery", zap.Error(err))

		return
	}

	a.nodesSet = map[string]struct{}{}
	a.diskStorageSet = map[string]struct{}{}
	a.isoStorageSet = map[string]struct{}{}
	a.perNode = map[string]*nodeTopologySets{}
	a.discoveryComplete = true

	for _, ns := range nodes {
		if ns == nil || ns.Node == "" {
			continue
		}

		a.nodesSet[ns.Node] = struct{}{}
		a.ensureNode(ns.Node)

		if ns.Status != "online" {
			continue
		}

		a.discoverPhysicalDevices(ctx, px, ns.Node, logger)

		node, nodeErr := px.Node(ctx, ns.Node)
		if nodeErr != nil {
			logger.Debug("failed to get node during schema discovery", zap.String("node", ns.Node), zap.Error(nodeErr))

			a.discoveryComplete = false

			continue
		}

		if !a.discoverSingleNode(ctx, node) {
			a.discoveryComplete = false
		}
	}
}

func (a *discoveryAccumulator) discoverSingleNode(ctx context.Context, node *proxmox.Node) bool {
	complete := true
	nt := a.ensureNode(node.Name)

	if nodeStorages, err := node.Storages(ctx); err == nil {
		for _, st := range nodeStorages {
			if st != nil && st.Name != "" && st.Enabled != 0 && st.Active != 0 {
				classifyStorageContent(st.Name, st.Content, a.diskStorageSet, a.isoStorageSet)
				classifyStorageContent(st.Name, st.Content, nt.diskStorages, nt.isoStorages)
			}
		}
	} else {
		complete = false
	}

	if networks, err := node.Networks(ctx); err == nil {
		for _, nw := range networks {
			if nw == nil || nw.Iface == "" {
				continue
			}

			if nw.Type == "bridge" || nw.Type == "OVSBridge" || strings.HasPrefix(nw.Iface, "vmbr") {
				a.bridgeSet[nw.Iface] = struct{}{}
				nt.networkBridges[nw.Iface] = struct{}{}
			}
		}
	}

	return complete
}

func (a *discoveryAccumulator) discoverClusterMappingsAndModels(ctx context.Context, cluster *proxmox.Cluster, logger *zap.Logger) {
	a.discoverSDNAndFirewall(ctx, cluster)
	a.discoverClusterMappings(ctx, cluster, logger)
	a.discoverClusterModelsAndRules(ctx, cluster)
}

func (a *discoveryAccumulator) discoverSDNAndFirewall(ctx context.Context, cluster *proxmox.Cluster) {
	if zones, err := cluster.SDNZones(ctx); err == nil {
		for _, z := range zones {
			if z == nil || z.Name == "" {
				continue
			}

			if len(z.Nodes) > 0 {
				a.sdnZoneNodes[z.Name] = slices.Clone([]string(z.Nodes))
				a.addNodeGroup("sdn-"+z.Name, []string(z.Nodes))
			}
		}
	}

	if vnets, err := cluster.SDNVNets(ctx); err == nil {
		for _, vnet := range vnets {
			a.recordSDNVNet(vnet)
		}
	}

	if fwGroups, err := cluster.FWGroups(ctx); err == nil {
		for _, g := range fwGroups {
			if g != nil && g.Group != "" {
				a.secGroupSet[g.Group] = struct{}{}
			}
		}
	}
}

func (a *discoveryAccumulator) recordSDNVNet(vnet *proxmox.VNet) {
	if vnet == nil || vnet.Name == "" {
		return
	}

	a.bridgeSet[vnet.Name] = struct{}{}

	zoneNodes := a.sdnZoneNodes[vnet.Zone]
	if len(zoneNodes) == 0 {
		for _, nt := range a.perNode {
			nt.networkBridges[vnet.Name] = struct{}{}
		}

		return
	}

	for _, n := range zoneNodes {
		if nt := a.ensureNode(n); nt != nil {
			nt.networkBridges[vnet.Name] = struct{}{}
		}
	}
}

func (a *discoveryAccumulator) discoverClusterMappings(ctx context.Context, cluster *proxmox.Cluster, logger *zap.Logger) {
	a.discoverPCIMappings(ctx, cluster, logger)
	a.discoverUSBMappings(ctx, cluster, logger)
}

func (a *discoveryAccumulator) discoverPCIMappings(ctx context.Context, cluster *proxmox.Cluster, logger *zap.Logger) {
	pciMappings, err := cluster.PCIMappings(ctx, "")
	if err != nil {
		logger.Warn("cannot discover PCI mappings; check Proxmox resource mapping permissions", zap.Error(err))

		return
	}

	for _, m := range pciMappings {
		if m == nil || m.ID == "" {
			continue
		}

		mappedNodes := a.currentMappingNodes(m.Map)

		for _, n := range mappedNodes {
			if nt := a.ensureNode(n); nt != nil {
				nt.pciMappings[m.ID] = struct{}{}
			}
		}

		if len(mappedNodes) > 0 {
			a.pciSet[m.ID] = struct{}{}
			a.addNodeGroup("pci-"+m.ID, mappedNodes)
		}
	}
}

func (a *discoveryAccumulator) discoverUSBMappings(ctx context.Context, cluster *proxmox.Cluster, logger *zap.Logger) {
	usbMappings, err := cluster.USBMappings(ctx, "")
	if err != nil {
		logger.Warn("cannot discover USB mappings; check Proxmox resource mapping permissions", zap.Error(err))

		return
	}

	for _, m := range usbMappings {
		if m == nil || m.ID == "" {
			continue
		}

		for _, n := range a.currentMappingNodes(m.Map) {
			if nt := a.ensureNode(n); nt != nil {
				a.usbSet[m.ID] = struct{}{}
				nt.usbMappings[m.ID] = struct{}{}
			}
		}
	}
}

func (a *discoveryAccumulator) currentMappingNodes(entries []string) []string {
	return slices.DeleteFunc(extractNodesFromMappingEntries(entries), func(node string) bool {
		_, current := a.nodesSet[node]

		return !current
	})
}

func extractNodesFromMappingEntries(entries []string) []string {
	var nodes []string

	for _, entry := range entries {
		for part := range strings.SplitSeq(entry, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && k == "node" && v != "" {
				nodes = append(nodes, v)
			}
		}
	}

	return nodes
}

func (a *discoveryAccumulator) discoverClusterModelsAndRules(ctx context.Context, cluster *proxmox.Cluster) {
	a.discoverCustomCPUModels(ctx, cluster)
	a.discoverHAGroupsAndRules(ctx, cluster)
}

func (a *discoveryAccumulator) discoverCustomCPUModels(ctx context.Context, cluster *proxmox.Cluster) {
	customCPUs, err := cluster.CustomCPUModels(ctx)
	if err != nil {
		return
	}

	for _, m := range customCPUs {
		if m == nil || m.CPUType == "" {
			continue
		}

		name := m.CPUType
		if !strings.HasPrefix(name, "custom-") {
			name = "custom-" + name
		}

		if _, exists := a.cpuModelSet[name]; !exists {
			a.cpuModelSet[name] = struct{}{}
			a.cpuTypes = append(a.cpuTypes, name)
		}
	}
}

func (a *discoveryAccumulator) discoverHAGroupsAndRules(ctx context.Context, cluster *proxmox.Cluster) {
	if haGroups, err := cluster.HAGroups(ctx); err == nil {
		for _, g := range haGroups {
			if g != nil && g.Group != "" {
				a.haGroupSet[g.Group] = struct{}{}

				if g.Nodes != "" {
					a.addNodeGroup(g.Group, strings.Split(g.Nodes, ","))
				}
			}
		}
	}

	if haRules, err := cluster.HARules(ctx, "", ""); err == nil {
		for _, r := range haRules {
			if r == nil || r.Rule == "" {
				continue
			}

			a.haRuleSet[r.Rule] = struct{}{}

			if r.Type == "node-affinity" && r.Nodes != "" {
				a.addNodeGroup(r.Rule, strings.Split(r.Nodes, ","))
			}
		}
	}
}

func classifyStorageContent(name, content string, diskStorages, isoStorages map[string]struct{}) {
	for part := range strings.SplitSeq(content, ",") {
		switch strings.TrimSpace(part) {
		case "images":
			diskStorages[name] = struct{}{}
		case "iso":
			isoStorages[name] = struct{}{}
		}
	}
}

// BuildDynamicSchema enriches the base Proxmox-grouped JSON schema with real cluster enum values
// including an empty choice so users can clear optional selectors.
func BuildDynamicSchema(baseSchema string, opts ClusterOptions) (string, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(baseSchema), &root); err != nil {
		return "", err
	}

	props, ok := root["properties"].(map[string]any)
	if !ok {
		return baseSchema, nil
	}

	groupNames := slices.Sorted(maps.Keys(opts.NodeGroups))
	setEnum(props, "storage", opts.DiskStorages, "")
	setEnum(props, "network_bridge", opts.NetworkBridges, pickDefaultBridge(opts.NetworkBridges))

	// 01. General
	if genProps := sectionOrRoot(props, "general"); genProps != nil {
		setEnum(genProps, "node_group", groupNames, "")
		setEnum(genProps, "node", opts.Nodes, "")
		setEnum(genProps, "pool", opts.Pools, "")
	}

	// 02. OS & Provisioning
	if osProps := sectionOrRoot(props, "os"); osProps != nil {
		setEnum(osProps, "iso_storage", opts.ISOStorages, "")
		setEnum(osProps, "template_vmid", opts.Templates, "")
	}

	// 04. CPU & Memory
	if compProps := sectionOrRoot(props, "compute"); compProps != nil && len(opts.CPUTypes) > 0 {
		setEnum(compProps, "cpu_type", opts.CPUTypes, "x86-64-v2-AES")
	}

	// 05. Disks (VM-disk-capable storages only)
	setArrayObjectPropertyEnum(
		props,
		"disks",
		"storage",
		opts.DiskStorages,
		"",
	)
	setArrayObjectPropertyEnum(props, "additional_disks", "storage", opts.DiskStorages, "")

	// 06. Network
	if netProps := sectionOrRoot(props, "network"); netProps != nil {
		setArrayObjectPropertyEnum(netProps, "nics", "bridge", opts.NetworkBridges, pickDefaultBridge(opts.NetworkBridges))
	}

	setArrayObjectPropertyEnum(props, "additional_nics", "bridge", opts.NetworkBridges, pickDefaultBridge(opts.NetworkBridges))

	// 07. High Availability
	setNestedArrayItemEnum(props, "ha", "node_affinity_nodes", opts.Nodes)
	setNestedArrayItemEnum(props, "ha", "rules", opts.HARules)
	setNestedPropertyEnum(props, "ha", "group", opts.HAGroups, "")

	// 08. Storage Replication
	setNestedPropertyEnum(props, "replication", "target_node", opts.Nodes, "")

	// 09. Firewall & Security
	if fwProps := sectionOrRoot(props, "firewall"); fwProps != nil {
		setTopLevelArrayItemEnum(fwProps, "security_groups", opts.SecurityGroups)
	}

	// 10. Hardware Passthrough
	if ptProps := sectionOrRoot(props, "passthrough"); ptProps != nil {
		setHardwareMappingOptions(ptProps, "pci_devices", opts.PCIMappings, opts)
		setHardwareMappingOptions(ptProps, "usb_devices", opts.USBMappings, opts)
	}

	addHardwareTopologyConstraints(root, props, opts)

	return marshalSchemaInOrder(root, []byte(baseSchema))
}

func nestedProperties(props map[string]any, objectField string) map[string]any {
	obj, ok := props[objectField].(map[string]any)
	if !ok {
		return nil
	}

	objProps, ok := obj["properties"].(map[string]any)
	if !ok {
		return nil
	}

	return objProps
}

func setEnum(props map[string]any, field string, values []string, defaultVal string) {
	prop, ok := props[field].(map[string]any)
	if !ok {
		return
	}

	setClearableChoices(prop, values)

	if defaultVal != "" && slices.Contains(values, defaultVal) {
		prop["default"] = defaultVal
	}
}

// Omni's enum renderer passes option labels to Reka UI, which forbids empty
// option labels. A non-empty whitespace title makes the row visually blank,
// while oneOf maps it back to the actual empty string in provider data.
// Other titles must equal their values so saved selections display correctly.
func setClearableChoices(prop map[string]any, values []string) {
	choices := []any{map[string]any{"const": "", "title": " "}}

	for _, value := range values {
		if value != "" && value != "auto" {
			choices = append(choices, map[string]any{"const": value, "title": value})
		}
	}

	delete(prop, "enum")
	prop["oneOf"] = choices
	prop["default"] = ""
}

func setTopLevelArrayItemEnum(props map[string]any, arrayField string, values []string) {
	if len(values) == 0 {
		return
	}

	arr, ok := props[arrayField].(map[string]any)
	if !ok {
		return
	}

	items, ok := arr["items"].(map[string]any)
	if !ok {
		return
	}

	setClearableChoices(items, values)
	delete(items, "minLength")
}

func setArrayObjectPropertyEnum(props map[string]any, arrayField, itemProp string, values []string, defaultVal string) {
	arr, ok := props[arrayField].(map[string]any)
	if !ok {
		return
	}

	items, ok := arr["items"].(map[string]any)
	if !ok {
		return
	}

	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		return
	}

	setEnum(itemProps, itemProp, values, defaultVal)
}

func setNestedArrayItemEnum(props map[string]any, objectField, arrayField string, values []string) {
	if len(values) == 0 {
		return
	}

	obj, ok := props[objectField].(map[string]any)
	if !ok {
		return
	}

	objProps, ok := obj["properties"].(map[string]any)
	if !ok {
		return
	}

	arr, ok := objProps[arrayField].(map[string]any)
	if !ok {
		return
	}

	items, ok := arr["items"].(map[string]any)
	if !ok {
		return
	}

	setClearableChoices(items, values)
	delete(items, "pattern")
}

func setNestedPropertyEnum(props map[string]any, objectField, propField string, values []string, defaultVal string) {
	obj, ok := props[objectField].(map[string]any)
	if !ok {
		return
	}

	objProps, ok := obj["properties"].(map[string]any)
	if !ok {
		return
	}

	setEnum(objProps, propField, values, defaultVal)
}

func firstOrEmpty(values []string) string {
	if len(values) > 0 {
		return values[0]
	}

	return ""
}

func pickDefaultBridge(bridges []string) string {
	if slices.Contains(bridges, "vmbr0") {
		return "vmbr0"
	}

	return firstOrEmpty(bridges)
}

func sectionOrRoot(props map[string]any, section string) map[string]any {
	if nested := nestedProperties(props, section); nested != nil {
		return nested
	}

	return props
}
