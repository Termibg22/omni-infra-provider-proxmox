// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"cmp"
	"context"
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

// filterNodesByGroupAndTopology restricts candidate Proxmox nodes to:
// 1. The configured `node_group` (erroring if no online node in the group is available), and
// 2. Nodes that possess the requested PCI/USB resource mappings when hardware passthrough is requested.
func (p *Provisioner) filterNodesByGroupAndTopology(
	ctx context.Context,
	logger *zap.Logger,
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

	// If PCI or USB passthrough mappings are requested, prefer nodes where all requested mappings exist
	if len(data.PCIDevices) > 0 || len(data.USBDevices) > 0 {
		hwFiltered := p.filterNodesWithHardwareMappings(ctx, candidates, data)
		if len(hwFiltered) > 0 {
			candidates = hwFiltered
		} else {
			logger.Debug("no node matched all PCI/USB mappings during pre-filtering, keeping group candidates")
		}
	}

	return candidates, nil
}

func (p *Provisioner) filterNodesWithHardwareMappings(ctx context.Context, candidates proxmox.NodeStatuses, data Data) proxmox.NodeStatuses {
	cluster := new(proxmox.Cluster).New(p.proxmoxClient)
	allowedByMapping := make(map[string][]string)

	if len(data.PCIDevices) > 0 {
		if pciMappings, err := cluster.PCIMappings(ctx, ""); err == nil {
			for _, m := range pciMappings {
				if m != nil && m.ID != "" {
					allowedByMapping["pci:"+m.ID] = extractNodesFromMappingEntries(m.Map)
				}
			}
		}
	}

	if len(data.USBDevices) > 0 {
		if usbMappings, err := cluster.USBMappings(ctx, ""); err == nil {
			for _, m := range usbMappings {
				if m != nil && m.ID != "" {
					allowedByMapping["usb:"+m.ID] = extractNodesFromMappingEntries(m.Map)
				}
			}
		}
	}

	if len(allowedByMapping) == 0 {
		return candidates
	}

	var matched proxmox.NodeStatuses

	for _, n := range candidates {
		if n == nil || !nodeHasAllMappings(n.Node, data, allowedByMapping) {
			continue
		}

		matched = append(matched, n)
	}

	return matched
}

func nodeHasAllMappings(nodeName string, data Data, allowedByMapping map[string][]string) bool {
	for _, pci := range data.PCIDevices {
		if nodes, ok := allowedByMapping["pci:"+pci.Mapping]; ok && len(nodes) > 0 {
			if !slices.Contains(nodes, nodeName) {
				return false
			}
		}
	}

	for _, usb := range data.USBDevices {
		if nodes, ok := allowedByMapping["usb:"+usb.Mapping]; ok && len(nodes) > 0 {
			if !slices.Contains(nodes, nodeName) {
				return false
			}
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
	primaryStorage string,
	pool string,
) error {
	tplVM, err := p.findTemplateVM(ctx, data)
	if err != nil {
		return err
	}

	fullClone := true
	if data.FullClone != nil {
		fullClone = *data.FullClone
	}

	targetNode := pctx.State.TypedSpec().Value.Node

	cloneOpts := &proxmox.VirtualMachineCloneOptions{
		NewID:       vmid,
		Name:        pctx.GetRequestID(),
		Target:      targetNode,
		Full:        proxmox.IntOrBool(fullClone),
		Storage:     primaryStorage,
		Pool:        pool,
		Description: data.Description,
	}

	logger.Info("cloning VM from Proxmox template",
		zap.Int("templateVMID", int(tplVM.VMID)),
		zap.Int("newVMID", vmid),
		zap.String("targetNode", targetNode),
		zap.String("storage", primaryStorage),
	)

	_, task, err := tplVM.Clone(ctx, cloneOpts)
	if err != nil {
		return fmt.Errorf("failed to clone template VM %d: %w", tplVM.VMID, err)
	}

	pctx.State.TypedSpec().Value.VmCreateTask = string(task.UPID)
	pctx.State.TypedSpec().Value.Vmid = int32(vmid)
	pctx.SetMachineInfraID(fmt.Sprintf("%s/%d", targetNode, vmid))
	applyDiskVolumeConfigPatches(ctx, logger, pctx, data.EffectiveDisks())

	return provision.NewRetryInterval(time.Second * 10)
}

func (p *Provisioner) findTemplateVM(ctx context.Context, data Data) (*proxmox.VirtualMachine, error) {
	tplID, rawSpec := data.ParsedTemplateVMID()
	if rawSpec == "" {
		return nil, fmt.Errorf("template_vmid must be set when provisioning_mode is 'clone'")
	}

	cluster := new(proxmox.Cluster).New(p.proxmoxClient)
	if resources, err := cluster.Resources(ctx, "vm"); err == nil {
		for _, r := range resources {
			if r == nil || r.Node == "" {
				continue
			}

			if (tplID > 0 && int(r.VMID) == tplID) || (tplID == 0 && strings.EqualFold(r.Name, rawSpec)) {
				return p.getVM(ctx, r.Node, int32(r.VMID))
			}
		}
	}

	if tplID > 0 {
		nodeName, found, err := p.ha.FindVMNode(ctx, int32(tplID))
		if err != nil {
			return nil, err
		}

		if found {
			return p.getVM(ctx, nodeName, int32(tplID))
		}
	}

	return nil, fmt.Errorf("proxmox VM template %q not found in cluster", rawSpec)
}

// applyClonedVMConfigAndResize applies CPU, memory, NICs, additional disks, firmware, and tags
// onto a newly cloned VM and resizes the primary boot disk if the requested size exceeds the template's disk.
func (p *Provisioner) applyClonedVMConfigAndResize(
	ctx context.Context,
	logger *zap.Logger,
	pctx provision.Context[*resources.Machine],
	vm *proxmox.VirtualMachine,
	data Data,
) error {
	node, err := p.proxmoxClient.Node(ctx, vm.Node)
	if err != nil {
		return err
	}

	effectiveDisks := data.EffectiveDisks()
	resolvedStorages := make([]string, len(effectiveDisks))

	for i, d := range effectiveDisks {
		st, pickErr := p.resolveStorage(ctx, node, d.Storage, d.StorageSelector)
		if pickErr != nil {
			return fmt.Errorf("failed to resolve storage for disk %d on cloned VM: %w", i, pickErr)
		}

		resolvedStorages[i] = st
	}

	onBoot := 1
	if data.OnBoot != nil && !*data.OnBoot {
		onBoot = 0
	}

	vmOptions := []proxmox.VirtualMachineOption{
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

	// Attach any additional disks beyond the template's primary disk (scsi1, scsi2, ...)
	if len(effectiveDisks) > 1 {
		allDiskOpts := buildDiskOptions(effectiveDisks, resolvedStorages)
		vmOptions = append(vmOptions, allDiskOpts[1:]...)
	}

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

	if len(effectiveDisks) > 0 && effectiveDisks[0].EffectiveSize() > 0 {
		primaryDiskName := fmt.Sprintf("%s0", effectiveDisks[0].EffectiveBus())
		sizeSpec := fmt.Sprintf("%dG", effectiveDisks[0].EffectiveSize())

		if resizeTask, resizeErr := vm.ResizeDisk(ctx, primaryDiskName, sizeSpec); resizeErr != nil {
			logger.Debug("skipping disk resize on cloned VM (disk may already be >= target size)",
				zap.String("disk", primaryDiskName), zap.String("size", sizeSpec), zap.Error(resizeErr))
		} else if waitErr := p.waitForTaskToFinish(ctx, resizeTask); waitErr != nil {
			logger.Debug("disk resize task on cloned VM reported non-fatal error", zap.Error(waitErr))
		}
	}

	return nil
}

func buildCPUOptionValue(data Data) string {
	cpuType := cmp.Or(data.CPUType, "x86-64-v2-AES")
	flags := strings.TrimSpace(data.CPUFlags)

	if flags == "" {
		return cpuType
	}

	return fmt.Sprintf("%s,flags=%s", cpuType, flags)
}

func applySecurityGroups(ctx context.Context, logger *zap.Logger, vm *proxmox.VirtualMachine, groups []string) {
	if len(groups) == 0 {
		return
	}

	existingRules, err := vm.FirewallRules(ctx)
	if err != nil {
		logger.Warn("failed to read existing VM firewall rules for security_groups", zap.Error(err))
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
			Type:   "group",
			Action: sg,
			Enable: 1,
		}); err != nil {
			logger.Warn("failed to attach Proxmox firewall security group to VM",
				zap.String("securityGroup", sg), zap.Int("vmid", int(vm.VMID)), zap.Error(err))
		}
	}
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

func applyVMFirewallOptions(ctx context.Context, logger *zap.Logger, vm *proxmox.VirtualMachine, fw *VMFirewallOptions) {
	if fw == nil {
		return
	}

	opt := &proxmox.FirewallVirtualMachineOption{
		PolicyIn:   fw.PolicyIn,
		PolicyOut:  fw.PolicyOut,
		LogLevelIn: fw.LogLevelIn,
	}

	if fw.Enable != nil {
		opt.Enable = proxmox.IntOrBool(*fw.Enable)
	}

	if fw.DHCP != nil {
		opt.Dhcp = proxmox.IntOrBool(*fw.DHCP)
	}

	if fw.MACFilter != nil {
		mf := proxmox.IntOrBool(*fw.MACFilter)
		opt.Macfilter = &mf
	}

	if fw.IPFilter != nil {
		opt.Ipfilter = proxmox.IntOrBool(*fw.IPFilter)
	}

	if fw.NDP != nil {
		opt.NDP = proxmox.IntOrBool(*fw.NDP)
	}

	if fw.Radv != nil {
		opt.Radv = proxmox.IntOrBool(*fw.Radv)
	}

	if err := vm.FirewallOptionSet(ctx, opt); err != nil {
		logger.Warn("failed to set Proxmox VM firewall options", zap.Int("vmid", int(vm.VMID)), zap.Error(err))
	}
}

func (p *Provisioner) reconcileReplicationJob(ctx context.Context, logger *zap.Logger, vmid int32, currentNode string, data Data) {
	if vmid == 0 {
		return
	}

	if data.Replication == nil || !data.Replication.Enabled {
		p.cleanupReplicationJobs(ctx, logger, vmid)

		return
	}

	targetNode := strings.TrimSpace(data.Replication.TargetNode)
	if targetNode == "" || targetNode == currentNode {
		targetNode = p.pickReplicationTargetNode(ctx, currentNode, data)
	}

	if targetNode == "" || targetNode == currentNode {
		logger.Debug("no secondary node available for storage replication job", zap.Int32("vmid", vmid))

		return
	}

	jobID := fmt.Sprintf("%d-0", vmid)
	schedule := cmp.Or(strings.TrimSpace(data.Replication.Schedule), "*/15")
	cluster := new(proxmox.Cluster).New(p.proxmoxClient)

	jobOpts := &proxmox.ReplicationJobOptions{
		ID:       jobID,
		Type:     "local",
		Target:   targetNode,
		Schedule: &schedule,
		Comment:  poolComment,
	}

	if data.Replication.RateLimit > 0 {
		rate := proxmox.StringOrFloat64(data.Replication.RateLimit)
		jobOpts.Rate = &rate
	}

	if err := cluster.NewReplicationJob(ctx, jobOpts); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			_ = cluster.ReplicationJobUpdate(ctx, jobID, &proxmox.ReplicationJobUpdateOption{ //nolint:errcheck
				Schedule: &schedule,
				Comment:  poolComment,
				Rate:     jobOpts.Rate,
			})

			return
		}

		logger.Warn("failed to configure Proxmox storage replication job", zap.String("jobID", jobID), zap.Error(err))
	}
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

func (p *Provisioner) cleanupReplicationJobs(ctx context.Context, logger *zap.Logger, vmid int32) {
	if vmid == 0 {
		return
	}

	cluster := new(proxmox.Cluster).New(p.proxmoxClient)

	jobs, err := cluster.ReplicationJobs(ctx)
	if err != nil {
		return
	}

	prefix := fmt.Sprintf("%d-", vmid)
	for _, j := range jobs {
		if j != nil && strings.HasPrefix(j.ID, prefix) {
			if delErr := cluster.ReplicationJobDelete(ctx, j.ID, true, false); delErr != nil {
				logger.Debug("failed to delete VM replication job during deprovision", zap.String("jobID", j.ID), zap.Error(delErr))
			}
		}
	}
}

func applyDiskVolumeConfigPatches(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine], disks []Disk) {
	// Disk 0 is the system boot disk; disks[1..] are additional data disks
	for i := 1; i < len(disks); i++ {
		volName := strings.TrimSpace(disks[i].VolumeName)
		if volName == "" {
			continue
		}

		sizeGB := disks[i].EffectiveSize()
		serial := sanitizeVolumeSerial(volName)

		matchExpr := fmt.Sprintf("!system_disk && disk.size >= %du * GiB", max(1, sizeGB-1))
		if serial != "" {
			matchExpr = fmt.Sprintf("disk.serial == %q || (!system_disk && disk.size >= %du * GiB)", serial, max(1, sizeGB-1))
		}

		patchYAML := fmt.Sprintf(`apiVersion: v1alpha1
kind: UserVolumeConfig
name: %s
provisioning:
  diskSelector:
    match: '%s'
  minSize: %dGiB
`, volName, matchExpr, sizeGB)

		patchName := fmt.Sprintf("900-proxmox-volume-%s", volName)
		if err := pctx.CreateConfigPatch(ctx, patchName, []byte(patchYAML)); err != nil {
			logger.Debug("skipping UserVolumeConfig patch creation", zap.String("volume", volName), zap.Error(err))
		}
	}
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
