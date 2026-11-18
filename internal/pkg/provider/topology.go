// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

// WithCustomNodeGroups registers static logical node groups from config.yaml on the Provisioner.
func WithCustomNodeGroups(groups map[string][]string) ProvisionerOption {
	return func(p *Provisioner) {
		p.customNodeGroups = groups
	}
}

// resolveNodeGroupNodes resolves the member node names of a logical node_group from:
// 1. Custom nodeGroups in config.yaml
// 2. Proxmox HA Groups (/cluster/ha/groups)
// 3. Proxmox HA Node-Affinity Rules (/cluster/ha/rules)
// 4. Proxmox SDN Zones (sdn-<zone>)
// 5. Proxmox PCI Resource Mappings (pci-<mapping>).
func (p *Provisioner) resolveNodeGroupNodes(ctx context.Context, groupName string) ([]string, error) {
	groupName = strings.TrimSpace(groupName)
	if groupName == "" {
		return nil, nil
	}

	if custom, ok := p.customNodeGroups[groupName]; ok && len(custom) > 0 {
		return normalizeGroupNodes(custom), nil
	}

	cluster := new(proxmox.Cluster).New(p.proxmoxClient)

	if nodes := resolveNodeGroupFromHA(ctx, cluster, groupName); len(nodes) > 0 {
		return nodes, nil
	}

	if nodes := resolveNodeGroupFromSDNOrPCI(ctx, cluster, groupName); len(nodes) > 0 {
		return nodes, nil
	}

	return nil, fmt.Errorf("node_group %q not found in config, Proxmox HA groups/rules, SDN zones, or PCI mappings", groupName)
}

func resolveNodeGroupFromHA(ctx context.Context, cluster *proxmox.Cluster, groupName string) []string {
	if haGroups, err := cluster.HAGroups(ctx); err == nil {
		for _, g := range haGroups {
			if g != nil && g.Group == groupName && g.Nodes != "" {
				return normalizeGroupNodes(strings.Split(g.Nodes, ","))
			}
		}
	}

	if haRules, err := cluster.HARules(ctx, "", ""); err == nil {
		for _, r := range haRules {
			if r != nil && r.Rule == groupName && r.Nodes != "" {
				return normalizeGroupNodes(strings.Split(r.Nodes, ","))
			}
		}
	}

	return nil
}

func resolveNodeGroupFromSDNOrPCI(ctx context.Context, cluster *proxmox.Cluster, groupName string) []string {
	if zoneName, ok := strings.CutPrefix(groupName, "sdn-"); ok {
		if zones, err := cluster.SDNZones(ctx); err == nil {
			for _, z := range zones {
				if z != nil && z.Name == zoneName && len(z.Nodes) > 0 {
					return normalizeGroupNodes([]string(z.Nodes))
				}
			}
		}
	}

	if pciID, ok := strings.CutPrefix(groupName, "pci-"); ok {
		if pciMappings, err := cluster.PCIMappings(ctx, ""); err == nil {
			for _, m := range pciMappings {
				if m != nil && m.ID == pciID {
					return normalizeGroupNodes(extractNodesFromMappingEntries(m.Map))
				}
			}
		}
	}

	return nil
}

func normalizeGroupNodes(raw []string) []string {
	var out []string

	for _, entry := range raw {
		nodeName, _, _ := strings.Cut(strings.TrimSpace(entry), ":")
		if nodeName != "" && !slices.Contains(out, nodeName) {
			out = append(out, nodeName)
		}
	}

	return out
}

func (p *Provisioner) provisioningCandidates(ctx context.Context, logger *zap.Logger, nodes proxmox.NodeStatuses, data Data) (proxmox.NodeStatuses, error) {
	candidates, err := p.filterNodesByGroupAndTopology(ctx, logger, nodes, data)
	if err != nil {
		return nil, err
	}

	var source *cloneSource
	if data.IsTemplateClone() {
		source, err = p.getCloneSource(ctx, data)
		if err != nil {
			return nil, err
		}
	}

	disks := data.EffectiveDisks()

	var (
		matches  proxmox.NodeStatuses
		failures []error
	)

	for _, candidate := range candidates {
		if candidate == nil || candidate.Status != "online" || (data.Node != "" && data.Node != candidate.Node) {
			continue
		}

		storages, err := p.nodeStoragesForPlacement(ctx, candidate.Node, source)
		if err != nil {
			failures = append(failures, err)

			continue
		}

		if err := validateNodeStorage(candidate.Node, storages, disks, data, source); err != nil {
			failures = append(failures, err)

			continue
		}

		matches = append(matches, candidate)
	}

	if len(matches) != 0 {
		return matches, nil
	}

	if len(failures) == 0 {
		return candidates, nil
	}

	return nil, fmt.Errorf("no eligible node for the requested storage and template: %w", errors.Join(failures...))
}

func (p *Provisioner) nodeStoragesForPlacement(ctx context.Context, node string, source *cloneSource) (proxmox.Storages, error) {
	if source != nil && node == source.vm.Node {
		return source.storages, nil
	}

	var storages proxmox.Storages
	if err := p.proxmoxClient.Get(ctx, fmt.Sprintf("/nodes/%s/storage", node), &storages); err != nil {
		return nil, fmt.Errorf("cannot read storage on %q: %w", node, err)
	}

	return storages, nil
}

func validateNodeStorage(node string, storages proxmox.Storages, disks []Disk, data Data, source *cloneSource) error {
	for index, disk := range disks {
		var err error
		if index == 0 && source != nil {
			_, err = source.bootStorage(node, storages, disk, data.FullClone == nil || *data.FullClone)
		} else {
			_, err = resolveDiskStorage(node, storages, disk.Storage, disk.StorageSelector)
		}

		if err != nil {
			return fmt.Errorf("node %q disk %d: %w", node, index, err)
		}
	}

	_, err := isoStorageCandidates(node, storages, data.ISOStorage)

	return err
}

type cloneSource struct {
	vm       *proxmox.VirtualMachine
	storages proxmox.Storages
}

func (p *Provisioner) getCloneSource(ctx context.Context, data Data) (*cloneSource, error) {
	vm, err := p.findTemplateVM(ctx, data)
	if err != nil {
		return nil, err
	}

	if vm.VirtualMachineConfig == nil {
		return nil, fmt.Errorf("cannot read disks on template %d", vm.VMID)
	}

	storages, err := p.nodeStoragesForPlacement(ctx, vm.Node, nil)
	if err != nil {
		return nil, err
	}

	return &cloneSource{vm: vm, storages: storages}, nil
}

func (source *cloneSource) bootStorage(node string, storages proxmox.Storages, boot Disk, full bool) (string, error) {
	candidates := storages
	drives := source.vm.VirtualMachineConfig.MergeDisks()

	if node != source.vm.Node {
		var err error

		candidates, err = source.remoteStorages(node, storages, drives)
		if err != nil {
			return "", err
		}
	}

	if !full {
		slot := boot.EffectiveBus() + "0"

		storage, _, ok := strings.Cut(driveVolume(drives[slot]), ":")
		if !ok || storage == "" {
			return "", fmt.Errorf("cannot determine storage for boot disk %s on template %d", slot, source.vm.VMID)
		}

		candidates = slices.DeleteFunc(slices.Clone(candidates), func(st *proxmox.Storage) bool { return st == nil || st.Name != storage })
	}

	name, err := resolveDiskStorage(node, candidates, boot.Storage, boot.StorageSelector)
	if err != nil && !full {
		return "", fmt.Errorf("linked clones keep the template boot disk on its original storage; use a full clone to change storage: %w", err)
	}

	return name, err
}

// remoteStorages returns stores shared by both nodes and verifies every attached
// template volume. Proxmox checks mounted ISO media, EFI and TPM disks too.
func (source *cloneSource) remoteStorages(node string, target proxmox.Storages, drives map[string]string) (proxmox.Storages, error) {
	isActiveShared := func(storage *proxmox.Storage) bool {
		return storage != nil && storage.Enabled != 0 && storage.Active != 0 && storage.Shared != 0
	}

	shared := make(map[string]bool)

	for _, storage := range source.storages {
		if isActiveShared(storage) {
			shared[storage.Name] = true
		}
	}

	candidates := slices.DeleteFunc(slices.Clone(target), func(storage *proxmox.Storage) bool {
		return !isActiveShared(storage) || !shared[storage.Name]
	})

	drives["efidisk0"] = source.vm.VirtualMachineConfig.EFIDisk0
	drives["tpmstate0"] = source.vm.VirtualMachineConfig.TPMState0

	for slot, spec := range drives {
		volume := driveVolume(spec)
		if volume == "" || volume == "none" || volume == "cdrom" {
			continue
		}

		name, _, ok := strings.Cut(volume, ":")
		if !ok || !slices.ContainsFunc(candidates, func(storage *proxmox.Storage) bool { return storage.Name == name }) {
			return nil, fmt.Errorf("template %d volume %s (%s) is not on active shared storage accessible from both %q and %q", source.vm.VMID, slot, volume, source.vm.Node, node)
		}
	}

	return candidates, nil
}

func driveVolume(spec string) string {
	for part := range strings.SplitSeq(spec, ",") {
		if file, ok := strings.CutPrefix(part, "file="); ok {
			return file
		}

		if !strings.Contains(part, "=") && part != "" {
			return part
		}
	}

	return ""
}

// filterNodesByGroupAndTopology restricts candidate Proxmox nodes to:
// 1. The configured `node_group` (erroring if no online node in the group is available), and
// 2. Nodes that possess the requested PCI/USB resource mappings when hardware passthrough is requested.
func (p *Provisioner) filterNodesByGroupAndTopology(
	ctx context.Context,
	_ *zap.Logger,
	nodes proxmox.NodeStatuses,
	data Data,
) (proxmox.NodeStatuses, error) {
	candidates := nodes

	if data.NodeGroup != "" {
		groupNodes, err := p.resolveNodeGroupNodes(ctx, data.NodeGroup)
		if err != nil {
			return nil, err
		}

		filtered := make(proxmox.NodeStatuses, 0, len(candidates))
		for _, n := range candidates {
			if n != nil && slices.Contains(groupNodes, n.Node) {
				filtered = append(filtered, n)
			}
		}

		if len(filtered) == 0 {
			return nil, fmt.Errorf("no nodes from node_group %q (%v) are present in the cluster", data.NodeGroup, groupNodes)
		}

		candidates = filtered
	}

	// PCI/USB passthrough requires a node where every requested mapping exists.
	if len(data.PCIDevices) > 0 || len(data.USBDevices) > 0 {
		hwFiltered, err := p.filterNodesWithHardwareMappings(ctx, candidates, data)
		if err != nil {
			return nil, err
		}

		if len(hwFiltered) == 0 {
			return nil, fmt.Errorf("no node provides all requested PCI/USB mappings")
		}

		candidates = hwFiltered
	}

	return candidates, nil
}

func (p *Provisioner) filterNodesWithHardwareMappings(ctx context.Context, candidates proxmox.NodeStatuses, data Data) (proxmox.NodeStatuses, error) {
	if err := data.validateHardware(); err != nil {
		return nil, err
	}

	cluster := new(proxmox.Cluster).New(p.proxmoxClient)
	allowedByMapping := make(map[string][]string)

	if data.usesResourceMappings("pci") {
		pciMappings, err := cluster.PCIMappings(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("cannot read Proxmox PCI resource mappings: %w", err)
		}

		for _, m := range pciMappings {
			if m != nil && m.ID != "" {
				allowedByMapping["pci:"+m.ID] = extractNodesFromMappingEntries(m.Map)
			}
		}
	}

	if data.usesResourceMappings("usb") {
		usbMappings, err := cluster.USBMappings(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("cannot read Proxmox USB resource mappings: %w", err)
		}

		for _, m := range usbMappings {
			if m != nil && m.ID != "" {
				allowedByMapping["usb:"+m.ID] = extractNodesFromMappingEntries(m.Map)
			}
		}
	}

	if err := p.addPhysicalDeviceNodes(ctx, candidates, data, allowedByMapping); err != nil {
		return nil, err
	}

	if len(allowedByMapping) == 0 {
		return nil, nil
	}

	var matched proxmox.NodeStatuses

	for _, n := range candidates {
		if n == nil || !nodeHasAllMappings(n.Node, data, allowedByMapping) {
			continue
		}

		matched = append(matched, n)
	}

	return matched, nil
}

func nodeHasAllMappings(nodeName string, data Data, allowedByMapping map[string][]string) bool {
	for _, pci := range data.PCIDevices {
		if nodes := allowedByMapping["pci:"+pci.Mapping]; !slices.Contains(nodes, nodeName) {
			return false
		}
	}

	for _, usb := range data.USBDevices {
		if nodes := allowedByMapping["usb:"+usb.Mapping]; !slices.Contains(nodes, nodeName) {
			return false
		}
	}

	return true
}

// cloneFromTemplate finds the source Proxmox VM template across the cluster and issues a Clone task
// onto targetNode with the allocated vmid, primary storage, and resource pool.
func (p *Provisioner) cloneFromTemplate(
	ctx context.Context,
	logger *zap.Logger,
	pctx provision.Context[*resources.Machine],
	data Data,
	vmid int,
	pool string,
) error {
	source, err := p.getCloneSource(ctx, data)
	if err != nil {
		return err
	}

	fullClone := true
	if data.FullClone != nil {
		fullClone = *data.FullClone
	}

	targetNode := pctx.State.TypedSpec().Value.Node

	storages, err := p.nodeStoragesForPlacement(ctx, targetNode, source)
	if err != nil {
		return err
	}

	primaryStorage, err := source.bootStorage(targetNode, storages, data.EffectiveDisks()[0], fullClone)
	if err != nil {
		return err
	}

	tplVM := source.vm

	cloneOpts := &proxmox.VirtualMachineCloneOptions{
		NewID:       vmid,
		Name:        pendingCloneName(pctx.State.TypedSpec().Value.Uuid),
		Target:      targetNode,
		Full:        proxmox.IntOrBool(fullClone),
		Pool:        pool,
		Description: data.Description,
	}

	if fullClone {
		cloneOpts.Storage = primaryStorage
	}

	logger.Info("cloning VM from Proxmox template",
		zap.Int("templateVMID", int(tplVM.VMID)),
		zap.Int("newVMID", vmid),
		zap.String("targetNode", targetNode),
		zap.String("targetStorage", cloneOpts.Storage),
	)

	_, task, err := tplVM.Clone(ctx, cloneOpts)
	if err != nil {
		return fmt.Errorf("failed to clone template VM %d: %w", tplVM.VMID, err)
	}

	pctx.State.TypedSpec().Value.VmCreateTask = string(task.UPID)
	pctx.State.TypedSpec().Value.Vmid = int32(vmid)
	pctx.SetMachineInfraID(fmt.Sprintf("%s/%d", targetNode, vmid))

	return provision.NewRetryInterval(time.Second * 10)
}

func (p *Provisioner) findTemplateVM(ctx context.Context, data Data) (*proxmox.VirtualMachine, error) {
	tplID, rawSpec := data.ParsedTemplateVMID()
	if rawSpec == "" {
		return nil, fmt.Errorf("template_vmid must be set when provisioning_mode is 'clone'")
	}

	if tplID > 999999999 {
		return nil, fmt.Errorf("template VMID %d exceeds the maximum 999999999", tplID)
	}

	cluster := new(proxmox.Cluster).New(p.proxmoxClient)
	if resources, err := cluster.Resources(ctx, "vm"); err == nil {
		match, matchErr := matchTemplate(resources, tplID, rawSpec)
		if matchErr != nil {
			return nil, matchErr
		}

		if match != nil {
			return p.getVM(ctx, match.Node, int32(match.VMID))
		}
	} else if tplID == 0 {
		return nil, fmt.Errorf("list VM templates: %w", err)
	}

	if tplID > 0 {
		nodeName, found, err := p.ha.FindVMNode(ctx, int32(tplID))
		if err != nil {
			return nil, err
		}

		if found {
			vm, getErr := p.getVM(ctx, nodeName, int32(tplID))
			if getErr != nil {
				return nil, getErr
			}

			if vm.VirtualMachineConfig == nil || !bool(vm.VirtualMachineConfig.Template) {
				return nil, fmt.Errorf("VM %d is not a template", tplID)
			}

			return vm, nil
		}
	}

	return nil, fmt.Errorf("proxmox VM template %q not found in cluster", rawSpec)
}

func matchTemplate(inventory proxmox.ClusterResources, id int, name string) (*proxmox.ClusterResource, error) {
	var match *proxmox.ClusterResource

	for _, r := range inventory {
		if r == nil || r.Node == "" || r.Type != "qemu" || r.Template != 1 {
			continue
		}

		if (id > 0 && int(r.VMID) == id) || (id == 0 && strings.EqualFold(r.Name, name)) {
			if match != nil {
				return nil, fmt.Errorf("multiple VM templates match %q; select a template by VMID", name)
			}

			match = r
		}
	}

	return match, nil
}

// applyClonedVMConfigAndResize applies CPU, memory, NICs, additional disks and tags
// onto a newly cloned VM and resizes the primary boot disk if the requested size exceeds the template's disk.
func (p *Provisioner) applyClonedVMConfigAndResize(
	ctx context.Context,
	logger *zap.Logger,
	pctx provision.Context[*resources.Machine],
	vm *proxmox.VirtualMachine,
	data Data,
) error {
	if err := p.validatePhysicalDevicesOnNode(ctx, vm.Node, data); err != nil {
		return err
	}

	onBoot := 1
	if data.OnBoot != nil && !*data.OnBoot {
		onBoot = 0
	}

	vmOptions := []proxmox.VirtualMachineOption{
		{Name: "name", Value: pctx.GetRequestID()},
		{Name: "smbios1", Value: "uuid=" + pctx.State.TypedSpec().Value.Uuid},
		{Name: "cpu", Value: buildCPUOptionValue(data)},
		{Name: "cores", Value: cmp.Or(data.Cores, 2)},
		{Name: "sockets", Value: cmp.Or(data.Sockets, 1)},
		{Name: "memory", Value: cmp.Or(data.Memory, uint64(4096))},
		{Name: "onboot", Value: onBoot},
		{Name: "agent", Value: "enabled=true"},
	}

	if data.Protection {
		vmOptions = append(vmOptions, proxmox.VirtualMachineOption{Name: "protection", Value: 1})
	}

	if data.NUMA {
		vmOptions = append(vmOptions, proxmox.VirtualMachineOption{Name: "numa", Value: 1})
	}

	if data.Balloon != nil && !*data.Balloon {
		vmOptions = append(vmOptions, proxmox.VirtualMachineOption{Name: "balloon", Value: 0})
	}

	vmOptions = append(vmOptions, buildNICOptions(data.EffectiveNICs())...)
	vmOptions = append(vmOptions, buildPCIDeviceOptions(data.PCIDevices)...)
	vmOptions = append(vmOptions, buildUSBDeviceOptions(data.USBDevices)...)
	vmOptions = append(vmOptions, buildEnterpriseVMOptions(data)...)

	machineRequestSet, _ := pctx.GetMachineRequestSetID()
	if tagsVal, ok := buildTagsOption(data.Tags, machineRequestSet); ok {
		vmOptions = append(vmOptions, proxmox.VirtualMachineOption{Name: "tags", Value: tagsVal})
	}

	cfgTask, err := vm.Config(ctx, vmOptions...)
	if err != nil {
		return fmt.Errorf("failed to configure cloned VM %d: %w", vm.VMID, err)
	}

	if err := p.waitForTaskToFinish(ctx, cfgTask); err != nil {
		return fmt.Errorf("failed waiting for cloned VM %d configuration task: %w", vm.VMID, err)
	}

	// Re-read disks on every retry and preserve existing volume IDs. Reissuing
	// allocation strings after a partial failure would replace attached disks.
	return p.reconcileVMDisks(ctx, logger, vm, data, nil)
}

func buildCPUOptionValue(data Data) string {
	cpuType := cmp.Or(data.CPUType, "x86-64-v2-AES")
	flags := strings.TrimSpace(data.CPUFlags)

	if flags == "" {
		return cpuType
	}

	return fmt.Sprintf("%s,flags=%s", cpuType, flags)
}

func applySecurityGroups(ctx context.Context, _ *zap.Logger, vm *proxmox.VirtualMachine, groups []string) error {
	if len(groups) == 0 {
		return nil
	}

	existingRules, err := vm.FirewallRules(ctx)
	if err != nil {
		return fmt.Errorf("failed to read VM firewall rules: %w", err)
	}

	existingGroups := map[string]struct{}{}

	for _, r := range existingRules {
		if r != nil && r.Type == "group" {
			existingGroups[r.Action] = struct{}{}
		}
	}

	for _, sg := range groups {
		sg = strings.TrimSpace(sg)
		if sg == "" {
			continue
		}

		if _, exists := existingGroups[sg]; exists {
			continue
		}

		if err := vm.NewFirewallRule(ctx, &proxmox.FirewallRule{
			Type:    "group",
			Comment: poolComment,
			Action:  sg,
			Enable:  1,
		}); err != nil {
			return fmt.Errorf("failed to attach security group %q to VM %d: %w", sg, vm.VMID, err)
		}

		existingGroups[sg] = struct{}{}
	}

	return nil
}

func buildEnterpriseVMOptions(data Data) []proxmox.VirtualMachineOption {
	var opts []proxmox.VirtualMachineOption

	// VirtIO Hardware RNG (/dev/urandom) for fast cryptographic entropy in Talos
	if data.RNG == nil || *data.RNG {
		opts = append(opts, proxmox.VirtualMachineOption{
			Name:  "rng0",
			Value: "source=/dev/urandom,max_bytes=1024,period=1000",
		})
	}

	if data.CPULimit > 0 {
		opts = append(opts, proxmox.VirtualMachineOption{
			Name:  "cpulimit",
			Value: fmt.Sprintf("%g", data.CPULimit),
		})
	}

	if data.CPUUnits > 0 {
		opts = append(opts, proxmox.VirtualMachineOption{
			Name:  "cpuunits",
			Value: data.CPUUnits,
		})
	}

	if s := strings.TrimSpace(data.StartupOrder); s != "" {
		opts = append(opts, proxmox.VirtualMachineOption{
			Name:  "startup",
			Value: s,
		})
	}

	if m := strings.TrimSpace(data.WatchdogModel); m != "" {
		wVal := fmt.Sprintf("model=%s", m)
		if act := strings.TrimSpace(data.WatchdogAction); act != "" {
			wVal = fmt.Sprintf("%s,action=%s", wVal, act)
		}

		opts = append(opts, proxmox.VirtualMachineOption{
			Name:  "watchdog",
			Value: wVal,
		})
	}

	return opts
}

func (p *Provisioner) applyVMFirewallOptions(ctx context.Context, _ *zap.Logger, vm *proxmox.VirtualMachine, fw *VMFirewallOptions) error {
	if fw == nil {
		return nil
	}
	// Use an explicit map: go-proxmox's omitempty boolean fields drop false,
	// so disabling a previously enabled firewall option would otherwise do nothing.
	opts := map[string]any{}

	for name, value := range map[string]*bool{"enable": fw.Enable, "dhcp": fw.DHCP, "macfilter": fw.MACFilter, "ipfilter": fw.IPFilter, "ndp": fw.NDP, "radv": fw.Radv} {
		if value != nil {
			n := 0
			if *value {
				n = 1
			}

			opts[name] = n
		}
	}

	if fw.PolicyIn != "" {
		opts["policy_in"] = fw.PolicyIn
	}

	if fw.PolicyOut != "" {
		opts["policy_out"] = fw.PolicyOut
	}

	if fw.LogLevelIn != "" {
		opts["log_level_in"] = fw.LogLevelIn
	}

	if len(opts) == 0 {
		return nil
	}

	if err := p.proxmoxClient.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/firewall/options", vm.Node, vm.VMID), opts, nil); err != nil {
		return fmt.Errorf("failed to configure VM %d firewall: %w", vm.VMID, err)
	}

	return nil
}

func (p *Provisioner) reconcileReplicationJob(ctx context.Context, logger *zap.Logger, vmid int32, currentNode string, data Data) error {
	if vmid == 0 || data.Replication == nil {
		return nil
	}

	if !data.Replication.Enabled {
		return p.cleanupReplicationJobs(ctx, logger, vmid)
	}

	targetNode := strings.TrimSpace(data.Replication.TargetNode)
	if targetNode == "" {
		targetNode = p.pickReplicationTargetNode(ctx, currentNode, data)
	}

	if targetNode == "" || targetNode == currentNode {
		return fmt.Errorf("replication for VM %d requires a different eligible target node", vmid)
	}

	jobID := fmt.Sprintf("%d-0", vmid)
	schedule := cmp.Or(strings.TrimSpace(data.Replication.Schedule), "*/15")
	cluster := new(proxmox.Cluster).New(p.proxmoxClient)

	jobs, err := cluster.ReplicationJobs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list replication jobs: %w", err)
	}

	var rate *proxmox.StringOrFloat64

	if data.Replication.RateLimit > 0 {
		value := proxmox.StringOrFloat64(data.Replication.RateLimit)
		rate = &value
	}

	for _, job := range jobs {
		if job == nil || job.ID != jobID {
			continue
		}

		if job.Comment != poolComment {
			return fmt.Errorf("replication job %q is not managed by this provider", jobID)
		}

		if job.Target != targetNode {
			return fmt.Errorf("changing replication target on existing job %q requires recreating the job", jobID)
		}

		opts := &proxmox.ReplicationJobUpdateOption{Schedule: &schedule, Comment: poolComment, Rate: rate}
		if rate == nil {
			opts.Delete = "rate"
		}

		if err := cluster.ReplicationJobUpdate(ctx, jobID, opts); err != nil {
			return fmt.Errorf("failed to update replication job %q: %w", jobID, err)
		}

		return nil
	}

	if err := cluster.NewReplicationJob(ctx, &proxmox.ReplicationJobOptions{ID: jobID, Type: "local", Target: targetNode, Schedule: &schedule, Comment: poolComment, Rate: rate}); err != nil {
		return fmt.Errorf("failed to create replication job %q: %w", jobID, err)
	}

	return nil
}

func (p *Provisioner) pickReplicationTargetNode(ctx context.Context, currentNode string, data Data) string {
	if data.NodeGroup != "" {
		if groupNodes, err := p.resolveNodeGroupNodes(ctx, data.NodeGroup); err == nil {
			for _, n := range groupNodes {
				if n != currentNode {
					return n
				}
			}
		}
	}

	if nodes, err := p.proxmoxClient.Nodes(ctx); err == nil {
		for _, n := range nodes {
			if n != nil && n.Status == "online" && n.Node != currentNode {
				return n.Node
			}
		}
	}

	return ""
}

func (p *Provisioner) cleanupReplicationJobs(ctx context.Context, _ *zap.Logger, vmid int32) error {
	if vmid == 0 {
		return nil
	}

	cluster := new(proxmox.Cluster).New(p.proxmoxClient)

	jobs, err := cluster.ReplicationJobs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list replication jobs during cleanup: %w", err)
	}

	prefix := fmt.Sprintf("%d-", vmid)
	for _, job := range jobs {
		if job == nil || !strings.HasPrefix(job.ID, prefix) || job.Comment != poolComment {
			continue
		}

		if err := cluster.ReplicationJobDelete(ctx, job.ID, true, false); err != nil {
			return fmt.Errorf("failed to delete managed replication job %q: %w", job.ID, err)
		}
	}

	return nil
}

func applyDiskVolumeConfigPatches(ctx context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine], disks []Disk) error {
	// Disk 0 is the system boot disk; disks[1..] are additional data disks
	for i := 1; i < len(disks); i++ {
		volName := strings.TrimSpace(disks[i].VolumeName)
		if volName == "" {
			continue
		}

		patchName, patchYAML := diskVolumeConfigPatch(pctx.GetRequestID(), disks[i])

		if err := pctx.CreateConfigPatch(ctx, patchName, patchYAML); err != nil {
			return fmt.Errorf("failed to create UserVolumeConfig patch for %q: %w", volName, err)
		}
	}

	return nil
}

func diskVolumeConfigPatch(requestID string, disk Disk) (string, []byte) {
	volName := strings.TrimSpace(disk.VolumeName)
	serial := sanitizeVolumeSerial(volName)
	// Leave room for partition metadata, then grow to use the available space.
	minSize := "512MiB"
	if size := disk.EffectiveSize(); size > 1 {
		minSize = fmt.Sprintf("%dGiB", size-1)
	}

	patchYAML := fmt.Sprintf(`apiVersion: v1alpha1
kind: UserVolumeConfig
name: %s
provisioning:
  diskSelector:
    match: '!system_disk && disk.serial == %q'
  minSize: %s
  grow: true
`, volName, serial, minSize)

	return fmt.Sprintf("900-proxmox-volume-%s-%s", requestID, volName), []byte(patchYAML)
}

func sanitizeVolumeSerial(volName string) string {
	volName = strings.TrimSpace(volName)
	if volName == "" {
		return ""
	}

	var b strings.Builder

	b.WriteString("vol-")

	for _, r := range volName {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}

	out := b.String()
	if len(out) > 20 {
		out = out[:20]
	}

	return out
}
