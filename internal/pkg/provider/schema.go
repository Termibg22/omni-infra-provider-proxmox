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
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/client"
	omniinfra "github.com/siderolabs/omni/client/pkg/omni/resources/infra"
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

// NodeTopology describes the storages, bridges/VNets, and hardware mappings available on a single Proxmox node.
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
	NodeGroups     map[string][]string
	NodeTopologies map[string]NodeTopology
	Nodes          []string
	DiskStorages   []string
	ISOStorages    []string
	NetworkBridges []string
	Pools          []string
	PCIMappings    []string
	USBMappings    []string
	HARules        []string
	HAGroups       []string
	CPUTypes       []string
	SecurityGroups []string
	Templates      []string
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
		DiskStorages:   nonEmptyOrFallback(slices.Sorted(maps.Keys(diskSet)), o.DiskStorages),
		ISOStorages:    nonEmptyOrFallback(slices.Sorted(maps.Keys(isoSet)), o.ISOStorages),
		NetworkBridges: nonEmptyOrFallback(slices.Sorted(maps.Keys(bridgeSet)), o.NetworkBridges),
		PCIMappings:    nonEmptyOrFallback(slices.Sorted(maps.Keys(pciSet)), o.PCIMappings),
		USBMappings:    nonEmptyOrFallback(slices.Sorted(maps.Keys(usbSet)), o.USBMappings),
	}
}

func nonEmptyOrFallback(vals, fallback []string) []string {
	if len(vals) > 0 {
		return vals
	}

	return fallback
}

// SchemaRefresherConfig holds settings for the background dynamic schema refresher.
type SchemaRefresherConfig struct {
	CustomNodeGroups map[string][]string
	ProxmoxClient    *proxmox.Client
	Logger           *zap.Logger
	OmniEndpoint     string
	ProviderID       string
	BaseSchema       string
	LastSchema       string
	ClientOptions    []client.Option
	Interval         time.Duration
}

// StartSchemaRefresher periodically queries the Proxmox cluster for newly added/removed
// nodes, node groups, storages, network bridges, SDN VNets, pools, hardware mappings, templates,
// firewall security groups, CPU models, and HA rules, updating the ProviderStatus schema in Omni.
func StartSchemaRefresher(ctx context.Context, cfg SchemaRefresherConfig) {
	if cfg.Interval <= 0 || cfg.ProxmoxClient == nil || cfg.OmniEndpoint == "" {
		return
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		lastSchema := cfg.LastSchema

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				opts := DiscoverClusterOptions(ctx, cfg.ProxmoxClient, logger, cfg.CustomNodeGroups)

				newSchema, err := BuildDynamicSchema(cfg.BaseSchema, opts)
				if err != nil {
					logger.Warn("failed to rebuild dynamic schema during periodic refresh", zap.Error(err))

					continue
				}

				if newSchema == lastSchema {
					continue
				}

				if err := publishSchemaToOmni(ctx, cfg.OmniEndpoint, cfg.ClientOptions, cfg.ProviderID, newSchema); err != nil {
					logger.Warn("failed to publish updated dynamic schema to Omni", zap.Error(err))

					continue
				}

				lastSchema = newSchema

				logger.Info("updated Proxmox provider schema in Omni with latest cluster topology")
			}
		}
	}()
}

func publishSchemaToOmni(ctx context.Context, omniEndpoint string, clientOpts []client.Option, providerID, newSchema string) error {
	c, err := client.New(omniEndpoint, clientOpts...)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck

	return UpdateProviderStatusSchema(ctx, c.Omni().State(), providerID, newSchema)
}

// UpdateProviderStatusSchema updates the Schema field on the ProviderStatus resource in COSI state.
func UpdateProviderStatusSchema(ctx context.Context, st state.State, providerID, newSchema string) error {
	providerStatus := omniinfra.NewProviderStatus(providerID)

	_, err := safe.StateUpdateWithConflicts(ctx, st, providerStatus.Metadata(), func(res *omniinfra.ProviderStatus) error {
		res.TypedSpec().Value.Schema = newSchema

		return nil
	})

	return err
}

type nodeTopologySets struct {
	diskStorages   map[string]struct{}
	isoStorages    map[string]struct{}
	networkBridges map[string]struct{}
	pciMappings    map[string]struct{}
	usbMappings    map[string]struct{}
}

type discoveryAccumulator struct {
	nodesSet       map[string]struct{}
	diskStorageSet map[string]struct{}
	isoStorageSet  map[string]struct{}
	bridgeSet      map[string]struct{}
	poolSet        map[string]struct{}
	pciSet         map[string]struct{}
	usbSet         map[string]struct{}
	haRuleSet      map[string]struct{}
	haGroupSet     map[string]struct{}
	cpuModelSet    map[string]struct{}
	secGroupSet    map[string]struct{}
	templateSet    map[string]struct{}
	nodeGroups     map[string][]string
	perNode        map[string]*nodeTopologySets
	sdnZoneNodes   map[string][]string
	cpuTypes       []string
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

	a.nodesSet[nodeName] = struct{}{}

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
	acc.discoverClusterMappingsAndModels(ctx, cluster)

	opts := acc.buildOptions()

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
		nodeTopologies[nodeName] = NodeTopology{
			DiskStorages:   slices.Sorted(maps.Keys(nt.diskStorages)),
			ISOStorages:    slices.Sorted(maps.Keys(nt.isoStorages)),
			NetworkBridges: slices.Sorted(maps.Keys(nt.networkBridges)),
			PCIMappings:    slices.Sorted(maps.Keys(nt.pciMappings)),
			USBMappings:    slices.Sorted(maps.Keys(nt.usbMappings)),
		}
	}

	return ClusterOptions{
		Nodes:          allNodes,
		NodeGroups:     a.nodeGroups,
		NodeTopologies: nodeTopologies,
		DiskStorages:   slices.Sorted(maps.Keys(a.diskStorageSet)),
		ISOStorages:    slices.Sorted(maps.Keys(a.isoStorageSet)),
		NetworkBridges: slices.Sorted(maps.Keys(a.bridgeSet)),
		Pools:          slices.Sorted(maps.Keys(a.poolSet)),
		PCIMappings:    slices.Sorted(maps.Keys(a.pciSet)),
		USBMappings:    slices.Sorted(maps.Keys(a.usbSet)),
		HARules:        slices.Sorted(maps.Keys(a.haRuleSet)),
		HAGroups:       slices.Sorted(maps.Keys(a.haGroupSet)),
		SecurityGroups: slices.Sorted(maps.Keys(a.secGroupSet)),
		Templates:      slices.Sorted(maps.Keys(a.templateSet)),
		CPUTypes:       a.cpuTypes,
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

	for _, ns := range nodes {
		if ns == nil || ns.Node == "" {
			continue
		}

		a.ensureNode(ns.Node)

		if ns.Status != "online" {
			continue
		}

		node, nodeErr := px.Node(ctx, ns.Node)
		if nodeErr != nil {
			logger.Debug("failed to get node during schema discovery", zap.String("node", ns.Node), zap.Error(nodeErr))

			continue
		}

		a.discoverSingleNode(ctx, node)
	}
}

func (a *discoveryAccumulator) discoverSingleNode(ctx context.Context, node *proxmox.Node) {
	nt := a.ensureNode(node.Name)

	if nodeStorages, err := node.Storages(ctx); err == nil {
		for _, st := range nodeStorages {
			if st != nil && st.Name != "" && st.Enabled != 0 {
				classifyStorageContent(st.Name, st.Content, a.diskStorageSet, a.isoStorageSet)
				classifyStorageContent(st.Name, st.Content, nt.diskStorages, nt.isoStorages)
			}
		}
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
}

func (a *discoveryAccumulator) discoverClusterMappingsAndModels(ctx context.Context, cluster *proxmox.Cluster) {
	a.discoverSDNAndFirewall(ctx, cluster)
	a.discoverClusterMappings(ctx, cluster)
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

func (a *discoveryAccumulator) discoverClusterMappings(ctx context.Context, cluster *proxmox.Cluster) {
	a.discoverPCIMappings(ctx, cluster)
	a.discoverUSBMappings(ctx, cluster)
}

func (a *discoveryAccumulator) discoverPCIMappings(ctx context.Context, cluster *proxmox.Cluster) {
	pciMappings, err := cluster.PCIMappings(ctx, "")
	if err != nil {
		return
	}

	for _, m := range pciMappings {
		if m == nil || m.ID == "" {
			continue
		}

		a.pciSet[m.ID] = struct{}{}
		mappedNodes := extractNodesFromMappingEntries(m.Map)

		for _, n := range mappedNodes {
			if nt := a.ensureNode(n); nt != nil {
				nt.pciMappings[m.ID] = struct{}{}
			}
		}

		if len(mappedNodes) > 0 {
			a.addNodeGroup("pci-"+m.ID, mappedNodes)
		}
	}
}

func (a *discoveryAccumulator) discoverUSBMappings(ctx context.Context, cluster *proxmox.Cluster) {
	usbMappings, err := cluster.USBMappings(ctx, "")
	if err != nil {
		return
	}

	for _, m := range usbMappings {
		if m == nil || m.ID == "" {
			continue
		}

		a.usbSet[m.ID] = struct{}{}

		for _, n := range extractNodesFromMappingEntries(m.Map) {
			if nt := a.ensureNode(n); nt != nil {
				nt.usbMappings[m.ID] = struct{}{}
			}
		}
	}
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
		case "images", "rootdir":
			diskStorages[name] = struct{}{}
		case "iso":
			isoStorages[name] = struct{}{}
		}
	}
}

// BuildDynamicSchema enriches the base Proxmox-grouped JSON schema with real cluster enum values
// using non-empty sentinel options ('auto'/'none') for optional dropdowns so UI frameworks (Reka UI)
// always allow clearing a selection without throwing empty-value errors.
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

	// 01. General
	if genProps := nestedProperties(props, "general"); genProps != nil {
		setEnum(genProps, "node_group", withSentinelOption("auto", groupNames), "auto")
		setEnum(genProps, "node", withSentinelOption("auto", opts.Nodes), "auto")
		setEnum(genProps, "pool", withSentinelOption("auto", opts.Pools), "auto")
	}

	// 02. OS & Provisioning
	if osProps := nestedProperties(props, "os"); osProps != nil {
		setEnum(osProps, "iso_storage", withSentinelOption("auto", opts.ISOStorages), "auto")
		setEnum(osProps, "template_vmid", withSentinelOption("none", opts.Templates), "none")
	}

	// 04. CPU & Memory
	if compProps := nestedProperties(props, "compute"); compProps != nil && len(opts.CPUTypes) > 0 {
		setEnum(compProps, "cpu_type", opts.CPUTypes, "x86-64-v2-AES")
	}

	// 05. Disks (VM-disk-capable storages only)
	setArrayObjectPropertyEnum(
		props,
		"disks",
		"storage",
		withSentinelOption("auto", opts.DiskStorages),
		"auto",
	)

	// 06. Network
	if netProps := nestedProperties(props, "network"); netProps != nil {
		setArrayObjectPropertyEnum(netProps, "nics", "bridge", opts.NetworkBridges, pickDefaultBridge(opts.NetworkBridges))
	}

	// 07. High Availability
	setNestedArrayItemEnum(props, "ha", "node_affinity_nodes", opts.Nodes)
	setNestedArrayItemEnum(props, "ha", "rules", opts.HARules)
	setNestedPropertyEnum(props, "ha", "group", withSentinelOption("none", opts.HAGroups), "none")

	// 08. Storage Replication
	setNestedPropertyEnum(props, "replication", "target_node", withSentinelOption("auto", opts.Nodes), "auto")

	// 09. Firewall & Security
	if fwProps := nestedProperties(props, "firewall"); fwProps != nil {
		setTopLevelArrayItemEnum(fwProps, "security_groups", opts.SecurityGroups)
	}

	// 10. Hardware Passthrough
	if ptProps := nestedProperties(props, "passthrough"); ptProps != nil {
		setArrayObjectPropertyEnum(ptProps, "pci_devices", "mapping", opts.PCIMappings, firstOrEmpty(opts.PCIMappings))
		setArrayObjectPropertyEnum(ptProps, "usb_devices", "mapping", opts.USBMappings, firstOrEmpty(opts.USBMappings))
	}

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
	if len(values) == 0 {
		return
	}

	prop, ok := props[field].(map[string]any)
	if !ok {
		return
	}

	prop["enum"] = values

	if defaultVal != "" && slices.Contains(values, defaultVal) {
		prop["default"] = defaultVal
	}
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

	items["enum"] = values
}

func setArrayObjectPropertyEnum(props map[string]any, arrayField, itemProp string, values []string, defaultVal string) {
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

	items["enum"] = values
	delete(items, "pattern")
}

func setNestedPropertyEnum(props map[string]any, objectField, propField string, values []string, defaultVal string) {
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

	setEnum(objProps, propField, values, defaultVal)
}

func firstOrEmpty(values []string) string {
	if len(values) > 0 {
		return values[0]
	}

	return ""
}

func withSentinelOption(sentinel string, values []string) []string {
	if slices.Contains(values, sentinel) {
		return values
	}

	out := make([]string, 0, len(values)+1)
	out = append(out, sentinel)
	out = append(out, values...)

	return out
}

func pickDefaultBridge(bridges []string) string {
	if slices.Contains(bridges, "vmbr0") {
		return "vmbr0"
	}

	return firstOrEmpty(bridges)
}
