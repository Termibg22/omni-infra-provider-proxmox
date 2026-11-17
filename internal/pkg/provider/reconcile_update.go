// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/api/omni/specs"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	"go.uber.org/zap"
	"go.yaml.in/yaml/v4"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

const providerDataHashAnnotation = "proxmox.omni.siderolabs.io/provider-data-hash"

// MachineSetUpdaterConfig configures the background reconciler that applies
// MachineSet / MachineClass updates to already-provisioned VMs.
type MachineSetUpdaterConfig struct {
	Provisioner   *Provisioner
	Logger        *zap.Logger
	OmniEndpoint  string
	ProviderID    string
	ClientOptions []client.Option
	Interval      time.Duration
}

// StartMachineSetUpdater periodically inspects provisioned MachineRequests in Omni
// and reconciles any changes in ProviderData (disks, NICs, CPU/RAM, node_group, HA,
// security groups, tags, pools) onto the existing Proxmox VMs.
func StartMachineSetUpdater(ctx context.Context, cfg MachineSetUpdaterConfig) {
	if cfg.Interval <= 0 || cfg.Provisioner == nil || cfg.OmniEndpoint == "" {
		return
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := syncProvisionedMachines(ctx, cfg, logger); err != nil {
					logger.Debug("periodic MachineSet update check encountered an error", zap.Error(err))
				}
			}
		}
	}()
}

func syncProvisionedMachines(ctx context.Context, cfg MachineSetUpdaterConfig, logger *zap.Logger) error {
	c, err := client.New(cfg.OmniEndpoint, cfg.ClientOptions...)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck

	st := c.Omni().State()

	requests, err := safe.StateListAll[*infra.MachineRequest](
		ctx,
		st,
		state.WithLabelQuery(resource.LabelEqual(omni.LabelInfraProviderID, cfg.ProviderID)),
	)
	if err != nil {
		return err
	}

	for req := range requests.All() {
		if req.Metadata().Phase() != resource.PhaseRunning {
			continue
		}

		reconcileSingleProvisionedRequest(ctx, st, cfg.Provisioner, logger, req)
	}

	return nil
}

func reconcileSingleProvisionedRequest(
	ctx context.Context,
	st state.State,
	prov *Provisioner,
	logger *zap.Logger,
	req *infra.MachineRequest,
) {
	reqID := req.Metadata().ID()

	status, err := safe.StateGetByID[*infra.MachineRequestStatus](ctx, st, reqID)
	if err != nil || status.TypedSpec().Value.Stage != specs.MachineRequestStatusSpec_PROVISIONED {
		return
	}

	machine, err := safe.StateGetByID[*resources.Machine](ctx, st, reqID)
	if err != nil || machine.TypedSpec().Value.Vmid == 0 {
		return
	}

	desiredHash := hashProviderData(req.TypedSpec().Value.ProviderData)
	currentHash, _ := machine.Metadata().Annotations().Get(providerDataHashAnnotation)

	if currentHash == desiredHash {
		return
	}

	// Adopt legacy machines without changing their hardware on first observation.
	if currentHash == "" {
		_, err = safe.StateUpdateWithConflicts(ctx, st, machine.Metadata(), func(m *resources.Machine) error {
			recordAppliedProviderData(m, req.TypedSpec().Value.ProviderData)

			return nil
		}, state.WithUpdateOwner(machine.Metadata().Owner()))
		if err != nil {
			logger.Warn("failed to record initial provider data", zap.Error(err))
		}

		return
	}

	if reconcileErr := prov.ReconcileExistingMachine(ctx, logger, machine, req); reconcileErr != nil {
		logger.Warn("failed to reconcile MachineSet update on existing VM",
			zap.String("requestID", reqID),
			zap.Int32("vmid", machine.TypedSpec().Value.Vmid),
			zap.Error(reconcileErr),
		)

		return
	}

	_, err = safe.StateUpdateWithConflicts(ctx, st, machine.Metadata(), func(m *resources.Machine) error {
		m.TypedSpec().Value.Node = machine.TypedSpec().Value.Node
		m.TypedSpec().Value.Pool = machine.TypedSpec().Value.Pool
		m.TypedSpec().Value.HaRegistered = machine.TypedSpec().Value.HaRegistered
		recordAppliedProviderData(m, req.TypedSpec().Value.ProviderData)

		return nil
	}, state.WithUpdateOwner(machine.Metadata().Owner()))
	if err != nil {
		logger.Warn("failed to persist reconciled machine state", zap.String("requestID", reqID), zap.Error(err))
	}
}

func hashProviderData(providerData string) string {
	var value map[string]any

	canonical := []byte(strings.TrimSpace(providerData))
	if yaml.Unmarshal(canonical, &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			canonical = encoded
		}
	}

	sum := sha256.Sum256(canonical)

	return hex.EncodeToString(sum[:])
}

// ReconcileExistingMachine applies live MachineSet / MachineClass configuration updates
// onto an already-provisioned Proxmox VM (adding/removing/resizing disks, adding/removing/updating NICs,
// updating CPU/RAM/tags/description/protection/PCI/USB devices, migrating across node_group/node changes,
// and syncing firewall security groups and HA rules).
func (p *Provisioner) ReconcileExistingMachine(
	ctx context.Context,
	logger *zap.Logger,
	machine *resources.Machine,
	machineRequest *infra.MachineRequest,
) error {
	if logger == nil {
		logger = zap.NewNop()
	}

	spec := machine.TypedSpec().Value
	if spec.Vmid == 0 {
		return nil
	}

	var data Data
	if raw := strings.TrimSpace(machineRequest.TypedSpec().Value.ProviderData); raw != "" {
		if err := yaml.Unmarshal([]byte(raw), &data); err != nil {
			return fmt.Errorf("failed to unmarshal updated ProviderData: %w", err)
		}
	}

	data.ApplyWorkloadProfile()

	if previous, ok := appliedProviderData(machine); ok {
		if err := validateLiveUpdate(previous, data); err != nil {
			return err
		}
	}

	if err := data.Validate(); err != nil {
		return err
	}

	vm, currentNode, err := p.locateExistingVM(ctx, spec.Node, spec.Vmid)
	if err != nil {
		return err
	}

	spec.Node = currentNode

	if bootErr := validateExistingBootSlot(vm, data.EffectiveDisks()); bootErr != nil {
		return bootErr
	}

	// 1. Reconcile node / node_group placement (live-migrate if node or node_group changed)
	vm, err = p.reconcileVMNodePlacement(ctx, logger, machine, vm, data)
	if err != nil {
		return err
	}

	machineRequestSet, _ := machineRequest.Metadata().Labels().Get(omni.LabelMachineRequestSet)

	// 2. Reconcile disks (attach new disks, resize grown disks, update disk flags, detach removed secondary disks)
	if err := p.reconcileVMDisks(ctx, logger, vm, data); err != nil {
		return err
	}

	// 3. Reconcile NICs, CPU, Memory, Tags, PCI/USB mappings, and VM metadata
	if err := p.reconcileVMConfigOptions(ctx, logger, vm, data, machineRequestSet); err != nil {
		return err
	}

	// 4. Reconcile Resource Pool
	if poolValue := cmp.Or(data.Pool, machineRequestSet); poolValue != "" && poolValue != spec.Pool {
		p.poolMu.Lock()
		poolErr := p.ensurePool(ctx, logger, poolValue, machineRequestSet, data.AutoCreatePool)
		p.poolMu.Unlock()

		if poolErr != nil {
			return poolErr
		}

		// ensurePool only creates the pool; membership must be moved in Proxmox too.
		if err := p.proxmoxClient.Put(ctx, "/pools", map[string]any{"poolid": poolValue, "vms": fmt.Sprint(spec.Vmid), "allow-move": 1}, nil); err != nil {
			return fmt.Errorf("failed to move VM %d to pool %q: %w", spec.Vmid, poolValue, err)
		}

		spec.Pool = poolValue
	}

	// 5. Reconcile Firewall Options, Security Groups, and Storage Replication Jobs
	if err := p.applyVMFirewallOptions(ctx, logger, vm, data.FirewallOptions); err != nil {
		return err
	}

	if err := reconcileVMSecurityGroups(ctx, logger, vm, data.SecurityGroups); err != nil {
		return err
	}

	if err := p.reconcileReplicationJob(ctx, logger, spec.Vmid, spec.Node, data); err != nil {
		return err
	}

	// 6. Reconcile Proxmox HA registration and node-affinity / resource-affinity rules
	return p.reconcileVMHA(ctx, logger, machine, data, machineRequestSet)
}

func (p *Provisioner) locateExistingVM(ctx context.Context, recordedNode string, vmid int32) (*proxmox.VirtualMachine, string, error) {
	if recordedNode != "" {
		if vm, err := p.getVM(ctx, recordedNode, vmid); err == nil {
			return vm, recordedNode, nil
		}
	}

	actualNode, found, err := p.ha.FindVMNode(ctx, vmid)
	if err != nil {
		return nil, "", err
	}

	if !found {
		return nil, "", fmt.Errorf("existing VM %d not found on any online Proxmox node", vmid)
	}

	vm, err := p.getVM(ctx, actualNode, vmid)
	if err != nil {
		return nil, "", err
	}

	return vm, actualNode, nil
}

func (p *Provisioner) reconcileVMNodePlacement(
	ctx context.Context,
	logger *zap.Logger,
	machine *resources.Machine,
	vm *proxmox.VirtualMachine,
	data Data,
) (*proxmox.VirtualMachine, error) {
	targetNode, err := p.desiredNodeForExistingVM(ctx, logger, vm.Node, data)
	if err != nil {
		return nil, err
	}

	if hardwareErr := p.validatePhysicalDevicesOnNode(ctx, cmp.Or(targetNode, vm.Node), data); hardwareErr != nil {
		return nil, hardwareErr
	}

	if targetNode == "" || targetNode == vm.Node {
		return vm, nil
	}

	logger.Info("migrating existing VM to match updated node/node_group configuration",
		zap.Int("vmid", int(vm.VMID)),
		zap.String("fromNode", vm.Node),
		zap.String("toNode", targetNode),
	)

	migTask, err := vm.Migrate(ctx, &proxmox.VirtualMachineMigrateOptions{
		Target:         targetNode,
		Online:         proxmox.IntOrBool(vm.IsRunning()),
		WithLocalDisks: proxmox.IntOrBool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to migrate VM %d from %q to %q: %w", vm.VMID, vm.Node, targetNode, err)
	}

	if err := p.waitForTaskToFinish(ctx, migTask); err != nil {
		return nil, fmt.Errorf("migration task for VM %d to %q failed: %w", vm.VMID, targetNode, err)
	}

	machine.TypedSpec().Value.Node = targetNode

	return p.getVM(ctx, targetNode, int32(vm.VMID))
}

func (p *Provisioner) desiredNodeForExistingVM(ctx context.Context, logger *zap.Logger, currentNode string, data Data) (string, error) {
	if data.Node != "" && data.Node != currentNode {
		if data.NodeGroup != "" {
			groupNodes, err := p.resolveNodeGroupNodes(ctx, data.NodeGroup)
			if err != nil {
				return "", err
			}

			if !slices.Contains(groupNodes, data.Node) {
				return "", fmt.Errorf("configured node %q does not belong to node_group %q (%v)", data.Node, data.NodeGroup, groupNodes)
			}
		}

		return data.Node, nil
	}

	if data.NodeGroup == "" {
		return currentNode, nil
	}

	groupNodes, err := p.resolveNodeGroupNodes(ctx, data.NodeGroup)
	if err != nil {
		return "", err
	}

	if slices.Contains(groupNodes, currentNode) {
		return currentNode, nil
	}

	nodes, err := p.proxmoxClient.Nodes(ctx)
	if err != nil {
		return "", err
	}

	candidates, err := p.filterNodesByGroupAndTopology(ctx, logger, nodes, data)
	if err != nil {
		return "", err
	}

	for _, c := range candidates {
		if c != nil && c.Status == "online" {
			return c.Node, nil
		}
	}

	return "", fmt.Errorf("no online node available in node_group %q to migrate VM from %q", data.NodeGroup, currentNode)
}

func (p *Provisioner) reconcileVMDisks(ctx context.Context, logger *zap.Logger, vm *proxmox.VirtualMachine, data Data) error {
	return p.reconcileVMDisksWithPolicy(ctx, logger, vm, data, true)
}

func (p *Provisioner) reconcileVMDisksWithPolicy(ctx context.Context, logger *zap.Logger, vm *proxmox.VirtualMachine, data Data, detach bool) error {
	node, err := p.proxmoxClient.Node(ctx, vm.Node)
	if err != nil {
		return err
	}

	effectiveDisks := data.EffectiveDisks()
	if err := validateExistingBootSlot(vm, effectiveDisks); err != nil {
		return err
	}

	resolvedStorages := make([]string, len(effectiveDisks))

	for i, d := range effectiveDisks {
		st, pickErr := p.resolveStorage(ctx, node, d.Storage, d.StorageSelector)
		if pickErr != nil {
			return fmt.Errorf("failed to resolve storage for disk %d: %w", i, pickErr)
		}

		resolvedStorages[i] = st
	}

	newDiskOptions := buildDiskOptions(effectiveDisks, resolvedStorages)

	var existingDisks map[string]string
	if vm.VirtualMachineConfig != nil {
		existingDisks = vm.VirtualMachineConfig.MergeDisks()
	}

	desiredSlots := make(map[string]struct{}, len(newDiskOptions))

	var (
		attachOrUpdateOpts []proxmox.VirtualMachineOption
		resizeTargets      []struct {
			slot string
			size int
		}
	)

	for i, opt := range newDiskOptions {
		slot := opt.Name
		desiredSlots[slot] = struct{}{}
		desiredSizeGB := effectiveDisks[i].EffectiveSize()

		currentSpec, exists := existingDisks[slot]
		if !exists || currentSpec == "" {
			// New disk added to the MachineSet: allocate and attach it
			attachOrUpdateOpts = append(attachOrUpdateOpts, opt)

			continue
		}

		// Existing disk: preserve its volume ID (e.g. "local-lvm:vm-100-disk-0") and update flags if needed
		updatedSpec := buildUpdatedExistingDiskSpec(currentSpec, fmt.Sprint(opt.Value))
		if updatedSpec != stripSizeFromDiskSpec(currentSpec) {
			attachOrUpdateOpts = append(attachOrUpdateOpts, proxmox.VirtualMachineOption{
				Name:  slot,
				Value: updatedSpec,
			})
		}

		// Check if disk size was increased in the MachineSet
		if currentSizeGB := parseDiskSizeGB(currentSpec); currentSizeGB > 0 && desiredSizeGB > currentSizeGB {
			resizeTargets = append(resizeTargets, struct {
				slot string
				size int
			}{slot: slot, size: desiredSizeGB})
		}
	}

	if len(attachOrUpdateOpts) > 0 {
		task, cfgErr := vm.Config(ctx, attachOrUpdateOpts...)
		if cfgErr != nil {
			return fmt.Errorf("failed to update disks on VM %d: %w", vm.VMID, cfgErr)
		}

		if waitErr := p.waitForTaskToFinish(ctx, task); waitErr != nil {
			return fmt.Errorf("disk configuration task failed on VM %d: %w", vm.VMID, waitErr)
		}
	}

	for _, rt := range resizeTargets {
		sizeStr := fmt.Sprintf("%dG", rt.size)
		logger.Info("resizing disk on existing VM", zap.Int("vmid", int(vm.VMID)), zap.String("disk", rt.slot), zap.String("newSize", sizeStr))

		task, resizeErr := vm.ResizeDisk(ctx, rt.slot, sizeStr)
		if resizeErr != nil {
			return fmt.Errorf("failed to resize disk %s to %s on VM %d: %w", rt.slot, sizeStr, vm.VMID, resizeErr)
		}

		if waitErr := p.waitForTaskToFinish(ctx, task); waitErr != nil {
			return fmt.Errorf("disk resize task for %s on VM %d failed: %w", rt.slot, vm.VMID, waitErr)
		}
	}

	bootSlot := "scsi0"
	if len(newDiskOptions) > 0 {
		bootSlot = newDiskOptions[0].Name
	}

	if !detach {
		return nil
	}

	// Detach secondary disks that were removed from the MachineSet (never detach the primary boot disk or ide* CD-ROMs)
	return p.detachRemovedSecondaryDisks(ctx, logger, vm, existingDisks, desiredSlots, bootSlot)
}

func (p *Provisioner) detachRemovedSecondaryDisks(
	ctx context.Context,
	logger *zap.Logger,
	vm *proxmox.VirtualMachine,
	existingDisks map[string]string,
	desiredSlots map[string]struct{},
	bootSlot string,
) error {
	for slot, spec := range existingDisks {
		if strings.HasPrefix(slot, "ide") || slot == bootSlot || strings.Contains(spec, "media=cdrom") {
			continue
		}

		if _, keep := desiredSlots[slot]; keep {
			continue
		}

		logger.Info("detaching removed secondary disk from existing VM", zap.Int("vmid", int(vm.VMID)), zap.String("disk", slot))

		task, err := vm.UnlinkDisk(ctx, slot, false)
		if err != nil {
			return fmt.Errorf("failed to detach removed disk %s on VM %d: %w", slot, vm.VMID, err)
		}

		if err := p.waitForTaskToFinish(ctx, task); err != nil {
			return fmt.Errorf("failed waiting to detach disk %s on VM %d: %w", slot, vm.VMID, err)
		}
	}

	return nil
}

// buildUpdatedExistingDiskSpec preserves the volume ID of an existing disk (e.g. "local-lvm:vm-100-disk-0")
// while applying the desired disk flags (e.g. "ssd=1,discard=on,iothread=1").
func buildUpdatedExistingDiskSpec(existingSpec, desiredCreationSpec string) string {
	volID, _, _ := strings.Cut(existingSpec, ",")

	volID = strings.TrimSpace(volID)
	if volID == "" {
		return ""
	}

	managed := map[string]bool{
		"ssd": true, "discard": true, "iothread": true, "cache": true, "aio": true, "backup": true,
		"replicate": true, "serial": true, "iops_rd": true, "iops_wr": true, "mbps_rd": true, "mbps_wr": true,
	}
	parts := []string{volID}

	_, oldFlags, _ := strings.Cut(existingSpec, ",")
	for flag := range strings.SplitSeq(oldFlags, ",") {
		key, _, _ := strings.Cut(flag, "=")
		if flag != "" && key != "size" && !managed[key] {
			parts = append(parts, flag)
		}
	}

	_, desiredFlags, _ := strings.Cut(desiredCreationSpec, ",")
	if desiredFlags != "" {
		parts = append(parts, strings.Split(desiredFlags, ",")...)
	}

	return strings.Join(parts, ",")
}

func stripSizeFromDiskSpec(spec string) string {
	var kept []string

	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "size=") {
			continue
		}

		kept = append(kept, part)
	}

	return strings.Join(kept, ",")
}

func parseDiskSizeGB(spec string) int {
	for part := range strings.SplitSeq(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k != "size" {
			continue
		}

		v = strings.TrimSpace(v)
		if numStr, isG := strings.CutSuffix(v, "G"); isG {
			if n, err := strconv.Atoi(numStr); err == nil {
				return n
			}
		}

		if numStr, isT := strings.CutSuffix(v, "T"); isT {
			if n, err := strconv.Atoi(numStr); err == nil {
				return n * 1024
			}
		}

		if numStr, isM := strings.CutSuffix(v, "M"); isM {
			if n, err := strconv.Atoi(numStr); err == nil && n >= 1024 {
				return n / 1024
			}
		}
	}

	return 0
}

func (p *Provisioner) reconcileVMConfigOptions(
	ctx context.Context,
	logger *zap.Logger,
	vm *proxmox.VirtualMachine,
	data Data,
	machineRequestSet string,
) error {
	effectiveNICs := data.EffectiveNICs()

	// Preserve existing MAC addresses on already-attached NICs unless the user explicitly set mac_address
	if vm.VirtualMachineConfig != nil && len(vm.VirtualMachineConfig.Nets) > 0 {
		existingMACs := extractVMMACAddresses(vm.VirtualMachineConfig.Nets, max(len(effectiveNICs), len(vm.VirtualMachineConfig.Nets)))
		for i := range effectiveNICs {
			if effectiveNICs[i].MACAddress == "" && existingMACs[i] != "" {
				effectiveNICs[i].MACAddress = strings.ToUpper(existingMACs[i])
			}
		}
	}

	onBoot := 1
	if data.OnBoot != nil && !*data.OnBoot {
		onBoot = 0
	}

	protection := 0
	if data.Protection {
		protection = 1
	}

	vmOptions := []proxmox.VirtualMachineOption{
		{Name: "cpu", Value: buildCPUOptionValue(data)},
		{Name: "cores", Value: cmp.Or(data.Cores, 2)},
		{Name: "sockets", Value: cmp.Or(data.Sockets, 1)},
		{Name: "memory", Value: cmp.Or(data.Memory, uint64(4096))},
		{Name: "onboot", Value: onBoot},
		{Name: "protection", Value: protection},
		{Name: "description", Value: data.Description},
		{Name: "cpulimit", Value: data.CPULimit},
	}

	if data.VGA != "" {
		vmOptions = append(vmOptions, proxmox.VirtualMachineOption{Name: "vga", Value: data.VGA})
	}

	tagsVal := existingVMDesiredTags(vm, data.Tags, machineRequestSet)
	vmOptions = append(vmOptions, proxmox.VirtualMachineOption{Name: "tags", Value: tagsVal})

	vmOptions = append(vmOptions, buildNICOptions(effectiveNICs)...)
	vmOptions = append(vmOptions, buildPCIDeviceOptions(data.PCIDevices)...)
	vmOptions = append(vmOptions, buildUSBDeviceOptions(data.USBDevices)...)
	vmOptions = append(vmOptions, buildEnterpriseVMOptions(data)...)

	// Delete any removed NICs, PCI devices, or USB devices
	if toDelete := buildRemovedDeviceList(vm.VirtualMachineConfig, len(effectiveNICs), len(data.PCIDevices), len(data.USBDevices)); toDelete != "" {
		vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
			Name:  "delete",
			Value: toDelete,
		})
	}

	logger.Info("applying updated configuration to existing VM", zap.Int("vmid", int(vm.VMID)), zap.String("node", vm.Node))

	task, err := vm.Config(ctx, vmOptions...)
	if err != nil {
		return fmt.Errorf("failed to update VM %d config: %w", vm.VMID, err)
	}

	return p.waitForTaskToFinish(ctx, task)
}

func buildPCIDeviceOptions(devices []PCIDevice) []proxmox.VirtualMachineOption {
	options := make([]proxmox.VirtualMachineOption, 0, len(devices))

	for i, pci := range devices {
		pciParts := []string{hardwareOption(pci.Mapping)}
		if pci.PCIExpress {
			pciParts = append(pciParts, "pcie=1")
		}

		if pci.PrimaryGPU {
			pciParts = append(pciParts, "x-vga=1")
		}

		if pci.ROMBar != nil && !*pci.ROMBar {
			pciParts = append(pciParts, "rombar=0")
		}

		if pci.MDev != "" {
			pciParts = append(pciParts, fmt.Sprintf("mdev=%s", pci.MDev))
		}

		options = append(options, proxmox.VirtualMachineOption{
			Name:  fmt.Sprintf("hostpci%d", i),
			Value: strings.Join(pciParts, ","),
		})
	}

	return options
}

func buildRemovedDeviceList(cfg *proxmox.VirtualMachineConfig, desiredNICs, desiredPCIs, desiredUSBs int) string {
	if cfg == nil {
		return ""
	}

	var removed []string

	for k := range cfg.Nets {
		if idx, ok := parseIndexedDeviceSlot(k, "net"); ok && idx >= desiredNICs {
			removed = append(removed, k)
		}
	}

	for k := range cfg.HostPCIs {
		if idx, ok := parseIndexedDeviceSlot(k, "hostpci"); ok && idx >= desiredPCIs {
			removed = append(removed, k)
		}
	}

	for k := range cfg.USBs {
		if idx, ok := parseIndexedDeviceSlot(k, "usb"); ok && idx >= desiredUSBs {
			removed = append(removed, k)
		}
	}

	slices.Sort(removed)

	return strings.Join(removed, ",")
}

func parseIndexedDeviceSlot(key, prefix string) (int, bool) {
	numStr, ok := strings.CutPrefix(key, prefix)
	if !ok || numStr == "" {
		return 0, false
	}

	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, false
	}

	return n, true
}

func reconcileVMSecurityGroups(ctx context.Context, logger *zap.Logger, vm *proxmox.VirtualMachine, desiredGroups []string) error {
	existingRules, err := vm.FirewallRules(ctx)
	if err != nil {
		return fmt.Errorf("failed to list VM firewall rules: %w", err)
	}

	desiredSet := make(map[string]struct{}, len(desiredGroups))
	for _, g := range desiredGroups {
		if g = strings.TrimSpace(g); g != "" {
			desiredSet[g] = struct{}{}
		}
	}

	// Remove security group rules that are no longer in desiredGroups (iterate in reverse position order)
	for _, r := range slices.Backward(existingRules) {
		if r == nil || r.Type != "group" || r.Comment != poolComment {
			continue
		}

		if _, keep := desiredSet[r.Action]; !keep {
			if delErr := r.Delete(ctx); delErr != nil {
				return fmt.Errorf("failed to remove managed security group %q: %w", r.Action, delErr)
			}
		}
	}

	return applySecurityGroups(ctx, logger, vm, desiredGroups)
}

func (p *Provisioner) reconcileVMHA(
	ctx context.Context,
	logger *zap.Logger,
	machine *resources.Machine,
	data Data,
	machineRequestSet string,
) error {
	spec := machine.TypedSpec().Value
	sid := fmt.Sprintf("vm:%d", spec.Vmid)

	if data.HA == nil {
		if spec.HaRegistered {
			if err := p.ha.Teardown(ctx, sid); err != nil {
				return err
			}

			spec.HaRegistered = false
		}

		return nil
	}

	if err := p.ha.Register(ctx, sid, *data.HA); err != nil {
		return err
	}

	spec.HaRegistered = true

	cfg := *data.HA
	if len(cfg.NodeAffinityNodes) == 0 && data.NodeGroup != "" {
		if groupNodes, err := p.resolveNodeGroupNodes(ctx, data.NodeGroup); err == nil && len(groupNodes) > 0 {
			cfg.NodeAffinityNodes = groupNodes
		}
	}

	if err := p.ha.SyncRules(ctx, logger, machineRequestSet, sid, cfg); err != nil {
		return err
	}

	return p.ha.ReconcileNodeAffinityRule(ctx, machineRequestSet, cfg)
}

func validateExistingBootSlot(vm *proxmox.VirtualMachine, effectiveDisks []Disk) error {
	if vm.VirtualMachineConfig == nil {
		return nil
	}

	cfg := vm.VirtualMachineConfig
	desired := effectiveDisks[0].EffectiveBus() + "0"

	disks := cfg.MergeDisks()
	if _, exists := disks[desired]; !exists {
		return fmt.Errorf("changing the boot disk bus/slot on an existing VM is not supported")
	}

	order, ok := strings.CutPrefix(cfg.Boot, "order=")
	if !ok {
		return nil
	}

	for slot := range strings.SplitSeq(order, ";") {
		spec, exists := disks[slot]
		if !exists || strings.Contains(spec, "media=cdrom") {
			continue
		}

		if slot != desired {
			return fmt.Errorf("requested boot slot %q differs from existing boot disk %q", desired, slot)
		}

		break
	}

	return nil
}

func existingVMDesiredTags(vm *proxmox.VirtualMachine, userTags []string, set string) string {
	tags := slices.Clone(userTags)
	cloudInitTag := proxmox.MakeTag(proxmox.TagCloudInit)

	current := vm.Tags
	if vm.VirtualMachineConfig != nil {
		current += ";" + vm.VirtualMachineConfig.Tags
	}

	if hasClusterTag(current, cloudInitTag) && !slices.Contains(tags, cloudInitTag) {
		tags = append(tags, cloudInitTag)
	}

	value, _ := buildTagsOption(tags, set)

	return value
}
