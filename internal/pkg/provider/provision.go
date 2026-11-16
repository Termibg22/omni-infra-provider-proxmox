// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements Proxmox infra provider core.
package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/google/uuid"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/imagefactory"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	siderocel "github.com/siderolabs/talos/pkg/machinery/cel"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/api/specs"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/ha"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

const (
	machineRequestTagPrefix = "machine-request."
	poolComment             = "managed by omni-infra-provider-proxmox"

	// taskLogTailLines is how many of the last task log lines a task failure error carries.
	taskLogTailLines = 10

	// maxStartAttempts is how many failed vm.Start() tasks are tolerated before the VM is destroyed.
	maxStartAttempts = 5

	// pendingVMIDTTL bounds how long an in-flight VMID reservation is held before expiring.
	pendingVMIDTTL = 5 * time.Minute
)

// errTaskFailed is returned for a Proxmox task that finished without success.
var errTaskFailed = errors.New("proxmox task failed")

// ProvisionerOption configures optional Provisioner settings.
type ProvisionerOption func(*Provisioner)

// WithDefaultVMIDRange sets a default [minVMID, maxVMID] allocation range when a MachineClass does not specify one.
func WithDefaultVMIDRange(minVMID, maxVMID int) ProvisionerOption {
	return func(p *Provisioner) {
		p.defaultMinVMID = minVMID
		p.defaultMaxVMID = maxVMID
	}
}

// Provisioner implements Talos Proxmox infra provider.
type Provisioner struct {
	ha                    *ha.Manager
	proxmoxClient         *proxmox.Client
	scheduler             *scheduler
	ipAllocator           *ipPoolAllocator
	customNodeGroups      map[string][]string
	pendingISODownloads   map[string]string
	pendingVMIDs          map[int]time.Time
	defaultMinVMID        int
	defaultMaxVMID        int
	pendingISODownloadsMu sync.Mutex
	poolMu                sync.Mutex
	vmidMu                sync.Mutex
}

// NewProvisioner creates a new provisioner.
func NewProvisioner(proxmoxClient *proxmox.Client, opts ...ProvisionerOption) *Provisioner {
	p := &Provisioner{
		proxmoxClient:       proxmoxClient,
		scheduler:           newScheduler(),
		ipAllocator:         newIPPoolAllocator(),
		ha:                  ha.NewManager(proxmoxClient),
		pendingISODownloads: make(map[string]string),
		pendingVMIDs:        make(map[int]time.Time),
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// ProvisionSteps implements infra.Provisioner.
//
//nolint:gocognit,gocyclo,cyclop,maintidx
func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep("pickNode", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if pctx.State.TypedSpec().Value.Node != "" {
				return nil
			}

			var data Data

			if err := pctx.UnmarshalProviderData(&data); err != nil {
				return err
			}

			data.ApplyWorkloadProfile()

			nodes, err := p.proxmoxClient.Nodes(ctx)
			if err != nil {
				return err
			}

			if len(nodes) == 0 {
				return fmt.Errorf("no nodes available")
			}

			// Filter candidate nodes by `node_group` (if set) and PCI/USB hardware mapping topology
			nodes, err = p.filterNodesByGroupAndTopology(ctx, logger, nodes, data)
			if err != nil {
				return err
			}

			// If user specified a node, validate and use it
			if data.Node != "" {
				for _, node := range nodes {
					if node.Node == data.Node {
						if node.Status != "online" {
							return fmt.Errorf("specified node %q is not online (status: %s)", data.Node, node.Status)
						}

						pctx.State.TypedSpec().Value.Node = data.Node

						logger.Info("using configured node for the Proxmox VM", zap.String("node", data.Node))

						return nil
					}
				}

				return fmt.Errorf("specified node %q not found in cluster (or outside configured node_group %q)", data.Node, data.NodeGroup)
			}

			strategy, err := parseStrategy(data.PlacementStrategy)
			if err != nil {
				return err
			}

			machineRequestSet, inSet := pctx.GetMachineRequestSetID()

			// Fast-path: query all cluster VMs in a single API call (/cluster/resources?type=vm)
			// instead of 2*N per-node calls when counting set VMs.
			var (
				clusterVMsByNode map[string][]*proxmox.ClusterResource
				hasClusterVMs    bool
			)

			if shouldCountSetVMs(data, inSet) {
				cluster := new(proxmox.Cluster).New(p.proxmoxClient)
				if vmResources, resErr := cluster.Resources(ctx, "vm"); resErr == nil {
					hasClusterVMs = true
					clusterVMsByNode = make(map[string][]*proxmox.ClusterResource, len(nodes))

					for _, vmRes := range vmResources {
						if vmRes != nil && vmRes.Node != "" && vmRes.Template == 0 {
							clusterVMsByNode[vmRes.Node] = append(clusterVMsByNode[vmRes.Node], vmRes)
						}
					}
				}
			}

			nodeInfoList := make([]nodeStatus, 0, len(nodes))
			materialized := map[string]struct{}{}
			wantTag := machineRequestTagPrefix + machineRequestSet

			for _, node := range nodes {
				// Skip nodes that are not online to avoid Proxmox API errors
				if node.Status != "online" {
					logger.Debug("skipping offline node", zap.String("node", node.Node), zap.String("status", node.Status))

					continue
				}

				var ns nodeStatus

				ns.Name = node.Node

				// MaxMem/Mem are unsigned; guard the subtraction in case Proxmox ever
				// reports used memory above the node total, and avoid div-by-zero.
				if node.MaxMem > node.Mem {
					ns.FreeMem = node.MaxMem - node.Mem
				}

				if node.MaxMem > 0 {
					ns.MemoryFree = float64(ns.FreeMem) / float64(node.MaxMem)
				}

				if node.CPU >= 0 && node.CPU <= 1 {
					ns.CPUFree = 1.0 - node.CPU
				}

				if shouldCountSetVMs(data, inSet) {
					if hasClusterVMs {
						vmsOnNode := clusterVMsByNode[node.Node]
						ns.TotalVMs = len(vmsOnNode)

						for _, vm := range vmsOnNode {
							if hasClusterTag(vm.Tags, wantTag) {
								ns.SameMachineRequestSetVMs++

								if vm.Name != "" {
									materialized[vm.Name] = struct{}{}
								}
							}
						}
					} else {
						n, err := p.proxmoxClient.Node(ctx, node.Node)
						if err != nil {
							return fmt.Errorf("failed to get node %q, %w", node.Node, err)
						}

						vms, err := n.VirtualMachines(ctx)
						if err != nil {
							return fmt.Errorf("failed to get vms for node %q, %w", node.Node, err)
						}

						ns.TotalVMs = len(vms)

						for _, vm := range vms {
							if vm.HasTag(wantTag) || hasClusterTag(vm.Tags, wantTag) {
								ns.SameMachineRequestSetVMs++
								materialized[vm.Name] = struct{}{}
							}
						}
					}
				}

				nodeInfoList = append(nodeInfoList, ns)
			}

			if len(nodeInfoList) == 0 {
				return fmt.Errorf("no online nodes available for provisioning")
			}

			var pickedNode nodeStatus

			if inSet {
				pickedNode = p.scheduler.pick(nodeInfoList, machineRequestSet, pctx.GetRequestID(), data.Memory*1024*1024, strategy, materialized)
			} else {
				pickedNode = pickNode(nodeInfoList)
			}

			pctx.State.TypedSpec().Value.Node = pickedNode.Name

			logger.Info("auto-selected node for the Proxmox VM", zap.String("node", pickedNode.Name))

			return nil
		}),
		provision.NewStep("uploadISO", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if pctx.State.TypedSpec().Value.VolumeUploadTask != "" {
				err := p.checkTaskStatus(ctx, pctx.State.TypedSpec().Value.VolumeUploadTask)
				if !isTaskFailed(err) {
					return err
				}

				logger.Info("retrying download", zap.Error(err))
			}

			pctx.State.TypedSpec().Value.TalosVersion = pctx.GetTalosVersion()

			var data Data

			err := pctx.UnmarshalProviderData(&data)
			if err != nil {
				return err
			}

			data.ApplyWorkloadProfile()

			// Skip ISO download when cloning from an existing Proxmox VM Template
			if data.IsTemplateClone() {
				return nil
			}

			arch := cmp.Or(data.Architecture, "amd64")

			schematicOpts := []provision.SchematicOption{
				provision.WithExtraExtensions(data.EffectiveExtensions()...),
				provision.WithoutConnectionParams(),
			}
			if len(data.ExtraKernelArgs) > 0 {
				schematicOpts = append(schematicOpts, provision.WithExtraKernelArgs(data.ExtraKernelArgs...))
			}

			media, err := pctx.EnsureInstallationMedia(
				ctx, logger, provision.MediaSpec{
					Kind:          imagefactory.InstallationMediaKindISO,
					Platform:      "nocloud",
					Architecture:  arch,
					SecureBoot:    data.SecureBoot && data.Bios == "ovmf",
					StandaloneURL: true,
				},
				schematicOpts...,
			)
			if err != nil {
				return provision.NewRetryErrorf(time.Second*10, "error resolving the installation media: %w", err)
			}

			pctx.State.TypedSpec().Value.Schematic = media.SchematicID

			isoName := media.StorageKey + ".iso"

			pctx.State.TypedSpec().Value.VolumeId = isoName

			node, err := p.proxmoxClient.Node(ctx, pctx.State.TypedSpec().Value.Node)
			if err != nil {
				return err
			}

			storage, err := p.resolveISOStorage(ctx, node, data.ISOStorage)
			if err != nil {
				return fmt.Errorf("failed to get ISO storage: %w", err)
			}

			p.pendingISODownloadsMu.Lock()
			defer p.pendingISODownloadsMu.Unlock()

			// Deduplicate by storage name when shared across the cluster, or per-node when local
			isoUploadScope := fmt.Sprintf("%s-%s", node.Name, storage.Name)
			if storage.Shared == 1 {
				isoUploadScope = storage.Name
			}

			isoUploadID := fmt.Sprintf("%s-%s", isoUploadScope, isoName)

			_, err = storage.ISO(ctx, isoName)
			// Already downloaded
			if err == nil {
				delete(p.pendingISODownloads, isoUploadID)

				return nil
			}

			if existing, ok := p.pendingISODownloads[isoUploadID]; ok {
				if p.isTaskLive(ctx, existing) {
					logger.Info("ISO image is already being downloaded, reusing the existing task", zap.String("volumeID", isoName), zap.String("task", existing))

					pctx.State.TypedSpec().Value.VolumeUploadTask = existing

					return provision.NewRetryInterval(time.Second)
				}

				// The previously tracked download is no longer running and the ISO never
				// landed, so drop the stale entry and fall through to start a fresh
				// download instead of pinning every machine to a dead task.
				logger.Info("previous ISO download task is no longer running, restarting download", zap.String("volumeID", isoName), zap.String("task", existing))

				delete(p.pendingISODownloads, isoUploadID)
			}

			task, err := storage.DownloadURL(ctx, "iso", isoName, media.URL)
			if err != nil {
				return err
			}

			logger.Info("uploading new ISO image", zap.String("volumeID", isoName), zap.String("storage", storage.Name), zap.String("task", string(task.UPID)))

			pctx.State.TypedSpec().Value.VolumeUploadTask = string(task.UPID)

			p.pendingISODownloads[isoUploadID] = string(task.UPID)

			return provision.NewRetryInterval(time.Second)
		}),
		provision.NewStep("syncVM", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if failure := pctx.State.TypedSpec().Value.Failure; failure != "" {
				return errors.New(failure)
			}

			if pctx.State.TypedSpec().Value.VmCreateTask != "" {
				err := p.checkTaskStatus(ctx, pctx.State.TypedSpec().Value.VmCreateTask)
				if isTaskFailed(err) {
					// a failed clone leaves an unknown partial VM behind, reissuing it would need a new vmid
					return p.giveUp(ctx, logger, pctx, fmt.Errorf("failed to create VM: %w", err))
				}

				return err
			}

			if pctx.State.TypedSpec().Value.Uuid == "" {
				pctx.State.TypedSpec().Value.Uuid = uuid.NewString()
				pctx.SetMachineUUID(pctx.State.TypedSpec().Value.Uuid)
			}

			var data Data

			err := pctx.UnmarshalProviderData(&data)
			if err != nil {
				return err
			}

			data.ApplyWorkloadProfile()

			node, err := p.proxmoxClient.Node(ctx, pctx.State.TypedSpec().Value.Node)
			if err != nil {
				return err
			}

			cluster, err := p.proxmoxClient.Cluster(ctx)
			if err != nil {
				return err
			}

			vmid, err := p.allocateVMID(ctx, cluster, data)
			if err != nil {
				return err
			}

			// Resolve all VM disks (from unified `disks` list or legacy primary + `additional_disks`)
			effectiveDisks := data.EffectiveDisks()
			if len(effectiveDisks) == 0 {
				return fmt.Errorf("at least one disk must be configured")
			}

			resolvedDiskStorages := make([]string, len(effectiveDisks))
			for i, disk := range effectiveDisks {
				storageName, pickErr := p.resolveStorage(ctx, node, disk.Storage, disk.StorageSelector)
				if pickErr != nil {
					return fmt.Errorf("failed to resolve storage for disk %d: %w", i, pickErr)
				}

				resolvedDiskStorages[i] = storageName
			}

			primaryStorage := resolvedDiskStorages[0]

			machineRequestSet, _ := pctx.GetMachineRequestSetID()
			poolValue := cmp.Or(data.Pool, machineRequestSet)

			if poolValue != "" {
				p.poolMu.Lock()
				poolErr := p.ensurePool(ctx, logger, poolValue, machineRequestSet, data.AutoCreatePool)
				p.poolMu.Unlock()

				if poolErr != nil {
					return poolErr
				}

				pctx.State.TypedSpec().Value.Pool = poolValue
			}

			// Branch 1: Clone from Proxmox VM Template when `provisioning_mode: clone` or `template_vmid` is set
			if data.IsTemplateClone() {
				return p.cloneFromTemplate(ctx, logger, pctx, data, vmid, primaryStorage, poolValue)
			}

			// Branch 2: Create VM from Talos Installation ISO
			isoStorage, err := p.resolveISOStorage(ctx, node, data.ISOStorage)
			if err != nil {
				return err
			}

			iso, err := isoStorage.ISO(ctx, pctx.State.TypedSpec().Value.VolumeId)
			if err != nil {
				return err
			}

			scsiHW := cmp.Or(data.SCSIHW, "virtio-scsi-single")

			onBoot := 1
			if data.OnBoot != nil && !*data.OnBoot {
				onBoot = 0
			}

			// Build VM options
			vmOptions := []proxmox.VirtualMachineOption{
				{
					Name:  "smbios1",
					Value: "uuid=" + pctx.State.TypedSpec().Value.Uuid,
				},
				{
					Name:  "name",
					Value: pctx.GetRequestID(),
				},
				{
					Name:  "cdrom",
					Value: iso.VolID,
				},
				{
					Name:  "cpu",
					Value: buildCPUOptionValue(data),
				},
				{
					Name:  "cores",
					Value: cmp.Or(data.Cores, 2),
				},
				{
					Name:  "sockets",
					Value: cmp.Or(data.Sockets, 1),
				},
				{
					Name:  "memory",
					Value: cmp.Or(data.Memory, uint64(4096)),
				},
				{
					Name:  "scsihw",
					Value: scsiHW,
				},
				{
					Name:  "onboot",
					Value: onBoot,
				},
				{
					Name:  "agent",
					Value: "enabled=true",
				},
			}

			if data.SerialConsole {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "serial0",
					Value: "socket",
				})
			}

			if data.Protection {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "protection",
					Value: 1,
				})
			}

			if data.Description != "" {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "description",
					Value: data.Description,
				})
			}

			// Append all resolved disks (scsi0, scsi1, ... or virtio0, sata0, ...)
			vmOptions = append(vmOptions, buildDiskOptions(effectiveDisks, resolvedDiskStorages)...)

			// Append all resolved NICs (net0, net1, ...)
			vmOptions = append(vmOptions, buildNICOptions(data.EffectiveNICs())...)

			if value, ok := buildTagsOption(data.Tags, machineRequestSet); ok {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "tags",
					Value: value,
				})
			}

			if poolValue != "" {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "pool",
					Value: poolValue,
				})
			}

			// Add machine type if specified (q35 for GPU passthrough)
			if data.MachineType != "" {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "machine",
					Value: data.MachineType,
				})
			}

			vmOptions = append(vmOptions, buildFirmwareOptions(data, primaryStorage)...)

			// Add NUMA if enabled
			if data.NUMA {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "numa",
					Value: 1,
				})
			}

			// Add hugepages if specified (Proxmox expects: "any", "2" for 2MB, "1024" for 1GB)
			if data.Hugepages != "" {
				var hugepagesStr string

				switch data.Hugepages {
				case "2MB", "2":
					hugepagesStr = "2"
				case "1GB", "1024":
					hugepagesStr = "1024"
				case "any":
					hugepagesStr = "any"
				default:
					hugepagesStr = data.Hugepages
				}

				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "hugepages",
					Value: hugepagesStr,
				})
			}

			// Disable balloon if explicitly set to false (for GPU/hugepages)
			if data.Balloon != nil && !*data.Balloon {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "balloon",
					Value: 0,
				})
			}

			// Add PCI device passthrough using Resource Mappings
			for i, pci := range data.PCIDevices {
				var pciParts []string

				pciParts = append(pciParts, fmt.Sprintf("mapping=%s", pci.Mapping))
				if pci.PCIExpress {
					pciParts = append(pciParts, "pcie=1")
				}

				if pci.PrimaryGPU {
					pciParts = append(pciParts, "x-vga=1")
				}

				if pci.ROMBar {
					pciParts = append(pciParts, "rombar=1")
				}

				if pci.MDev != "" {
					pciParts = append(pciParts, fmt.Sprintf("mdev=%s", pci.MDev))
				}

				pciString := strings.Join(pciParts, ",")
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  fmt.Sprintf("hostpci%d", i), // hostpci0, hostpci1, etc.
					Value: pciString,
				})
			}

			vmOptions = append(vmOptions, buildUSBDeviceOptions(data.USBDevices)...)
			vmOptions = append(vmOptions, buildEnterpriseVMOptions(data)...)

			task, err := node.NewVirtualMachine(ctx, vmid, vmOptions...)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.VmCreateTask = string(task.UPID)
			pctx.State.TypedSpec().Value.Vmid = int32(vmid)
			pctx.SetMachineInfraID(fmt.Sprintf("%s/%d", pctx.State.TypedSpec().Value.Node, vmid))
			applyDiskVolumeConfigPatches(ctx, logger, pctx, effectiveDisks)

			return provision.NewRetryInterval(time.Second * 10)
		}),
		provision.NewStep("startVM", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if failure := pctx.State.TypedSpec().Value.Failure; failure != "" {
				return errors.New(failure)
			}

			spec := pctx.State.TypedSpec().Value

			if spec.VmStartTask != "" {
				err := p.checkTaskStatus(ctx, spec.VmStartTask)
				if !isTaskFailed(err) {
					return err
				}

				// the VM can be gone already: a previous give up deleted it, but failed waiting for the delete task,
				// so the failure was never persisted. Give up again, destroyVM treats the missing VM as deleted.
				vm, vmErr := p.getVM(ctx, spec.Node, spec.Vmid)
				gone := isAlreadyGoneErr(vmErr)

				if vmErr != nil && !gone {
					return vmErr
				}

				// the task failed, but the VM may be up regardless (e.g. it was already running)
				if !gone && vm.IsRunning() {
					logger.Info("VM start task failed, but the VM is running", zap.Error(err))

					return nil
				}

				if gone || !retryStart(spec) {
					return p.giveUp(ctx, logger, pctx, fmt.Errorf("failed to start VM after %d attempts: %w", spec.StartAttempts+1, err))
				}

				logger.Info("VM start task failed, retrying", zap.Int32("attempts", spec.StartAttempts), zap.Error(err))
			}

			var data Data

			if err := pctx.UnmarshalProviderData(&data); err != nil {
				return err
			}

			data.ApplyWorkloadProfile()

			vm, err := p.getVM(ctx, spec.Node, spec.Vmid)
			if err != nil {
				return err
			}

			// When cloned from a template, apply hardware overrides and resize the boot disk before starting
			if data.IsTemplateClone() && spec.StartAttempts == 0 {
				if err = p.applyClonedVMConfigAndResize(ctx, logger, pctx, vm, data); err != nil {
					return err
				}

				if vm, err = p.getVM(ctx, spec.Node, spec.Vmid); err != nil {
					return err
				}
			}

			applyVMFirewallOptions(ctx, logger, vm, data.FirewallOptions)
			applySecurityGroups(ctx, logger, vm, data.SecurityGroups)
			p.reconcileReplicationJob(ctx, logger, spec.Vmid, spec.Node, data)

			effectiveNICs := data.EffectiveNICs()
			allocatedIPs := make(map[int]string, len(effectiveNICs))

			for i, nic := range effectiveNICs {
				if nic.IPPool != "" {
					poolKey := fmt.Sprintf("nic%d:%s", i, nic.IPPool)
					p.seedIPPoolFromCluster(ctx, poolKey, spec.Vmid)

					ipCIDR, allocErr := p.ipAllocator.Allocate(poolKey, pctx.GetRequestID())
					if allocErr != nil {
						return fmt.Errorf("failed to allocate IP from ip_pool for NIC %d: %w", i, allocErr)
					}

					allocatedIPs[i] = ipCIDR
				}
			}

			for i := range data.Bonds {
				if data.Bonds[i].IPPool != "" {
					poolKey := fmt.Sprintf("bond%d:%s", i, data.Bonds[i].IPPool)
					p.seedIPPoolFromCluster(ctx, poolKey, spec.Vmid)

					ipCIDR, allocErr := p.ipAllocator.Allocate(poolKey, pctx.GetRequestID())
					if allocErr != nil {
						return fmt.Errorf("failed to allocate IP from ip_pool for bond %d: %w", i, allocErr)
					}

					if !slices.Contains(data.Bonds[i].Addresses, ipCIDR) {
						data.Bonds[i].Addresses = append([]string{ipCIDR}, data.Bonds[i].Addresses...)
					}
				}
			}

			var macByIndex map[int]string
			if vm.VirtualMachineConfig != nil {
				macByIndex = extractVMMACAddresses(vm.VirtualMachineConfig.Nets, len(effectiveNICs))
			}

			networkConfig, err := buildCloudInitNetworkConfig(data, effectiveNICs, macByIndex, allocatedIPs)
			if err != nil {
				return fmt.Errorf("failed to build cloud-init network config: %w", err)
			}

			var ciOpts []proxmox.CloudInitOption
			if data.ISOStorage != "" {
				ciOpts = append(ciOpts, proxmox.WithCloudInitStorage(data.ISOStorage))
			}

			err = vm.CloudInit(
				ctx,
				"ide0",
				pctx.ConnectionParams.JoinConfig,
				fmt.Sprintf(
					`instance-id: %s
local-hostname: %s
hostname: %s`,
					pctx.State.TypedSpec().Value.Uuid,
					pctx.GetRequestID(),
					pctx.GetRequestID(),
				),
				"",
				networkConfig,
				ciOpts...,
			)
			if err != nil {
				return fmt.Errorf("failed to inject nocloud config: %w", err)
			}

			task, err := vm.Start(ctx)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.VmStartTask = string(task.UPID)

			return provision.NewRetryInterval(time.Second * 1)
		}),
		provision.NewStep("registerHA", p.ha.RegisterStep),
		provision.NewStep("syncHARules", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			var data Data
			if err := pctx.UnmarshalProviderData(&data); err == nil && data.HA != nil && data.NodeGroup != "" && len(data.HA.NodeAffinityNodes) == 0 {
				if groupNodes, groupErr := p.resolveNodeGroupNodes(ctx, data.NodeGroup); groupErr == nil && len(groupNodes) > 0 {
					set, _ := pctx.GetMachineRequestSetID()
					sid := fmt.Sprintf("vm:%d", pctx.State.TypedSpec().Value.Vmid)
					cfg := *data.HA
					cfg.NodeAffinityNodes = groupNodes

					return p.ha.SyncRules(ctx, logger, set, sid, cfg)
				}
			}

			return p.ha.SyncRulesStep(ctx, logger, pctx)
		}),
	}
}

// Deprovision implements infra.Provisioner.
func (p *Provisioner) Deprovision(ctx context.Context, logger *zap.Logger, machine *resources.Machine, machineRequest *infra.MachineRequest) error {
	// release up front: a request torn down before its VM materializes returns
	// at the Vmid == 0 check below.
	p.scheduler.release(machineRequest.Metadata().ID())
	p.ipAllocator.Release(machineRequest.Metadata().ID())

	if machine.TypedSpec().Value.Vmid == 0 {
		return nil
	}

	p.cleanupReplicationJobs(ctx, logger, machine.TypedSpec().Value.Vmid)

	if err := p.ha.Deprovision(ctx, logger, machine); err != nil {
		return err
	}

	haRegistered := machine.TypedSpec().Value.HaRegistered

	var (
		node  string
		found bool
	)

	if haRegistered {
		var err error

		node, found, err = p.ha.FindVMNode(ctx, machine.TypedSpec().Value.Vmid)
		if err != nil {
			return err
		}
	} else {
		node = machine.TypedSpec().Value.Node
		found = node != ""
	}

	if !found {
		if pool := machine.TypedSpec().Value.Pool; pool != "" {
			p.cleanupPool(ctx, logger, pool)
		}

		return nil
	}

	if err := p.destroyVM(ctx, logger, node, machine.TypedSpec().Value.Vmid); err != nil {
		return err
	}

	if pool := machine.TypedSpec().Value.Pool; pool != "" {
		p.cleanupPool(ctx, logger, pool)
	}

	return nil
}

// allocateVMID selects a free VMID either within the configured [min_vmid, max_vmid] range
// or via Proxmox's /cluster/nextid endpoint, guarding against concurrent VMID collisions.
func (p *Provisioner) allocateVMID(ctx context.Context, cluster *proxmox.Cluster, data Data) (int, error) {
	p.vmidMu.Lock()
	defer p.vmidMu.Unlock()

	now := time.Now()
	for id, ts := range p.pendingVMIDs {
		if now.Sub(ts) > pendingVMIDTTL {
			delete(p.pendingVMIDs, id)
		}
	}

	minID := cmp.Or(data.MinVMID, p.defaultMinVMID)
	maxID := cmp.Or(data.MaxVMID, p.defaultMaxVMID)

	if minID > 0 || maxID > 0 {
		return p.allocateVMIDInRange(ctx, cluster, minID, maxID, now)
	}

	vmid, err := cluster.NextID(ctx)
	if err != nil {
		return 0, err
	}

	for {
		if _, inFlight := p.pendingVMIDs[vmid]; !inFlight {
			p.pendingVMIDs[vmid] = now

			return vmid, nil
		}

		vmid++
	}
}

func (p *Provisioner) allocateVMIDInRange(ctx context.Context, cluster *proxmox.Cluster, minID, maxID int, now time.Time) (int, error) {
	if minID < 100 {
		minID = 100
	}

	if maxID == 0 {
		maxID = 999999999
	}

	if minID > maxID {
		return 0, fmt.Errorf("invalid VMID range: min_vmid (%d) cannot be greater than max_vmid (%d)", minID, maxID)
	}

	used := fetchUsedVMIDs(ctx, cluster)

	for candidate := minID; candidate <= maxID; candidate++ {
		if _, inFlight := p.pendingVMIDs[candidate]; inFlight {
			continue
		}

		if _, exists := used[candidate]; exists {
			continue
		}

		free, err := p.checkVMIDFree(ctx, candidate)
		if err != nil {
			// If /cluster/nextid?vmid= is unsupported on a mock server when /cluster/resources already succeeded,
			// trust the used map if we got cluster resources.
			if len(used) > 0 {
				p.pendingVMIDs[candidate] = now

				return candidate, nil
			}

			return 0, err
		}

		if free {
			p.pendingVMIDs[candidate] = now

			return candidate, nil
		}
	}

	return 0, fmt.Errorf("no free VMID available in configured range [%d, %d]", minID, maxID)
}

func fetchUsedVMIDs(ctx context.Context, cluster *proxmox.Cluster) map[int]struct{} {
	used := map[int]struct{}{}

	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return used
	}

	for _, r := range resources {
		if r != nil && r.VMID > 0 {
			used[int(r.VMID)] = struct{}{}
		}
	}

	return used
}

// checkVMIDFree queries /cluster/nextid?vmid=<id> using a pointer destination (&ret)
// to avoid the non-pointer unmarshal bug in go-proxmox's Cluster.CheckID.
func (p *Provisioner) checkVMIDFree(ctx context.Context, vmid int) (bool, error) {
	var ret string

	err := p.proxmoxClient.Get(ctx, fmt.Sprintf("/cluster/nextid?vmid=%d", vmid), &ret)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// seedIPPoolFromCluster ensures existing VMs with lower VMIDs have their deterministic
// IP pool allocations registered so IP assignments remain collision-free across provider restarts.
func (p *Provisioner) seedIPPoolFromCluster(ctx context.Context, poolKey string, currentVMID int32) {
	cluster := new(proxmox.Cluster).New(p.proxmoxClient)

	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return
	}

	slices.SortFunc(resources, func(a, b *proxmox.ClusterResource) int {
		if a == nil || b == nil {
			return 0
		}

		return cmp.Compare(a.VMID, b.VMID)
	})

	for _, r := range resources {
		if r == nil || r.Template != 0 || r.Name == "" || int32(r.VMID) >= currentVMID {
			continue
		}

		_, _ = p.ipAllocator.Allocate(poolKey, r.Name) //nolint:errcheck
	}
}

func hasClusterTag(tags, want string) bool {
	if tags == "" {
		return false
	}

	for part := range strings.SplitSeq(tags, ";") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}

	return false
}

// User tags come first so the tags string is stable across reconciles.
func buildTagsOption(userTags []string, machineRequestSet string) (string, bool) {
	tags := make([]string, 0, len(userTags)+1)
	tags = append(tags, userTags...)

	if machineRequestSet != "" {
		tags = append(tags, machineRequestTagPrefix+machineRequestSet)
	}

	if len(tags) == 0 {
		return "", false
	}

	return strings.Join(tags, ";"), true
}

func buildDiskOptions(disks []Disk, storages []string) []proxmox.VirtualMachineOption {
	options := make([]proxmox.VirtualMachineOption, 0, len(disks))
	busCounters := map[string]int{}

	for i, disk := range disks {
		bus := disk.EffectiveBus()
		slot := busCounters[bus]
		busCounters[bus] = slot + 1

		options = append(options, proxmox.VirtualMachineOption{
			Name:  fmt.Sprintf("%s%d", bus, slot),
			Value: formatDiskOptionValue(disk, storages[i], bus),
		})
	}

	return options
}

func formatDiskOptionValue(disk Disk, storage, bus string) string {
	opts := []string{fmt.Sprintf("%s:%d", storage, disk.EffectiveSize())}
	if (disk.SSD || disk.DiskSSD) && bus != "virtio" {
		opts = append(opts, "ssd=1")
	}

	if disk.Discard || disk.DiskDiscard {
		opts = append(opts, "discard=on")
	}

	if (disk.IOThread || disk.DiskIOThread) && (bus == "scsi" || bus == "virtio") {
		opts = append(opts, "iothread=1")
	}

	if cache := disk.EffectiveCache(); cache != "" {
		opts = append(opts, fmt.Sprintf("cache=%s", cache))
	}

	if aio := disk.EffectiveAIO(); aio != "" {
		opts = append(opts, fmt.Sprintf("aio=%s", aio))
	}

	if disk.Backup != nil && !*disk.Backup {
		opts = append(opts, "backup=0")
	}

	if disk.Replicate {
		opts = append(opts, "replicate=1")
	}

	opts = appendDiskQoSAndSerial(opts, disk)

	return strings.Join(opts, ",")
}

func appendDiskQoSAndSerial(opts []string, disk Disk) []string {
	if disk.IOPSRead > 0 {
		opts = append(opts, fmt.Sprintf("iops_rd=%d", disk.IOPSRead))
	}

	if disk.IOPSWrite > 0 {
		opts = append(opts, fmt.Sprintf("iops_wr=%d", disk.IOPSWrite))
	}

	if disk.MBPSRead > 0 {
		opts = append(opts, fmt.Sprintf("mbps_rd=%g", disk.MBPSRead))
	}

	if disk.MBPSWrite > 0 {
		opts = append(opts, fmt.Sprintf("mbps_wr=%g", disk.MBPSWrite))
	}

	if serial := sanitizeVolumeSerial(disk.VolumeName); serial != "" {
		opts = append(opts, fmt.Sprintf("serial=%s", serial))
	}

	return opts
}

func buildNICOptions(nics []NIC) []proxmox.VirtualMachineOption {
	options := make([]proxmox.VirtualMachineOption, 0, len(nics))

	for i, nic := range nics {
		model := cmp.Or(nic.Model, "virtio")
		bridge := cmp.Or(nic.Bridge, "vmbr0")

		firewallVal := 0
		if nic.Firewall != nil && *nic.Firewall {
			firewallVal = 1
		}

		modelPart := model
		if nic.MACAddress != "" {
			modelPart = fmt.Sprintf("%s=%s", model, nic.MACAddress)
		}

		parts := []string{
			modelPart,
			fmt.Sprintf("bridge=%s", bridge),
			fmt.Sprintf("firewall=%d", firewallVal),
		}

		if nic.Vlan > 0 {
			parts = append(parts, fmt.Sprintf("tag=%d", nic.Vlan))
		}

		if nic.MTU > 0 {
			parts = append(parts, fmt.Sprintf("mtu=%d", nic.MTU))
		}

		if nic.Queues > 0 {
			parts = append(parts, fmt.Sprintf("queues=%d", nic.Queues))
		}

		if nic.RateLimit > 0 {
			parts = append(parts, fmt.Sprintf("rate=%g", nic.RateLimit))
		}

		if nic.Trunks != "" {
			parts = append(parts, fmt.Sprintf("trunks=%s", nic.Trunks))
		}

		options = append(options, proxmox.VirtualMachineOption{
			Name:  fmt.Sprintf("net%d", i),
			Value: strings.Join(parts, ","),
		})
	}

	return options
}

func buildUSBDeviceOptions(devices []USBDevice) []proxmox.VirtualMachineOption {
	options := make([]proxmox.VirtualMachineOption, 0, len(devices))

	for i, device := range devices {
		parts := []string{fmt.Sprintf("mapping=%s", device.Mapping)}
		if device.USB3 {
			parts = append(parts, "usb3=1")
		}

		options = append(options, proxmox.VirtualMachineOption{
			Name:  fmt.Sprintf("usb%d", i),
			Value: strings.Join(parts, ","),
		})
	}

	return options
}

func buildFirmwareOptions(data Data, selectedStorage string) []proxmox.VirtualMachineOption {
	var options []proxmox.VirtualMachineOption

	if data.Bios != "" {
		options = append(options, proxmox.VirtualMachineOption{
			Name:  "bios",
			Value: data.Bios,
		})

		if data.Bios == "ovmf" {
			preEnrolled := 0
			if data.SecureBoot {
				preEnrolled = 1
			}

			options = append(options, proxmox.VirtualMachineOption{
				Name:  "efidisk0",
				Value: fmt.Sprintf("%s:1,efitype=4m,pre-enrolled-keys=%d", selectedStorage, preEnrolled),
			})
		}
	}

	if data.TPM {
		options = append(options, proxmox.VirtualMachineOption{
			Name:  "tpmstate0",
			Value: fmt.Sprintf("%s:1,version=v2.0", selectedStorage),
		})
	}

	if data.VGA != "" {
		options = append(options, proxmox.VirtualMachineOption{
			Name:  "vga",
			Value: data.VGA,
		})
	}

	return options
}

func poolCreateDecision(exists bool, poolID, machineRequestSet string, autoCreate ...*bool) (bool, error) {
	if exists {
		return false, nil
	}

	if poolID == machineRequestSet {
		return true, nil
	}

	if len(autoCreate) > 0 && autoCreate[0] != nil && *autoCreate[0] {
		return true, nil
	}

	return false, fmt.Errorf("proxmox pool %q does not exist; create it in Proxmox, enable \"auto_create_pool\", or leave \"pool\" empty to use the machine request set id", poolID)
}

// List + search instead of Pool(id): go-proxmox flattens missing-pool 404 into a generic 500.
func (p *Provisioner) findPoolInList(ctx context.Context, poolID string) (comment string, exists bool, err error) {
	pools, err := p.proxmoxClient.Pools(ctx)
	if err != nil {
		return "", false, fmt.Errorf("failed to list proxmox pools: %w", err)
	}

	for _, pool := range pools {
		if pool.PoolID == poolID {
			return pool.Comment, true, nil
		}
	}

	return "", false, nil
}

func (p *Provisioner) ensurePool(ctx context.Context, logger *zap.Logger, poolID, machineRequestSet string, autoCreate *bool) error {
	_, exists, err := p.findPoolInList(ctx, poolID)
	if err != nil {
		return err
	}

	create, err := poolCreateDecision(exists, poolID, machineRequestSet, autoCreate)
	if err != nil {
		return err
	}

	if !create {
		return nil
	}

	logger.Info("creating proxmox pool", zap.String("pool", poolID))

	return p.proxmoxClient.NewPool(ctx, poolID, poolComment)
}

// Errors are swallowed on purpose: pool cleanup must not fail deprovision.
func (p *Provisioner) cleanupPool(ctx context.Context, logger *zap.Logger, poolID string) {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()

	comment, exists, err := p.findPoolInList(ctx, poolID)
	if err != nil {
		logger.Warn("failed to list proxmox pools during cleanup", zap.String("pool", poolID), zap.Error(err))

		return
	}

	if !exists || comment != poolComment {
		return
	}

	// List doesn't return members, so hit the detail endpoint before deciding
	// whether to delete.
	detail, err := p.proxmoxClient.Pool(ctx, poolID)
	if err != nil {
		logger.Warn("failed to load proxmox pool detail during cleanup", zap.String("pool", poolID), zap.Error(err))

		return
	}

	if detail.Comment != poolComment || len(detail.Members) != 0 {
		return
	}

	if err := detail.Delete(ctx); err != nil {
		logger.Warn("failed to delete proxmox pool", zap.String("pool", poolID), zap.Error(err))

		return
	}

	logger.Info("deleted empty proxmox pool", zap.String("pool", poolID))
}

func (p *Provisioner) resolveISOStorage(ctx context.Context, node *proxmox.Node, isoStorage string) (*proxmox.Storage, error) {
	if isoStorage != "" {
		return node.Storage(ctx, isoStorage)
	}

	return node.StorageISO(ctx)
}

// resolveStorage resolves a VM disk storage pool on node using either a direct storage name
// (selected from the dynamic cluster dropdown) or a CEL expression / plain name in selector.
func (p *Provisioner) resolveStorage(ctx context.Context, node *proxmox.Node, directStorage, selector string) (string, error) {
	storages, err := node.Storages(ctx)
	if err != nil {
		return "", err
	}

	if directStorage != "" {
		for _, st := range storages {
			if st.Name == directStorage {
				return st.Name, nil
			}
		}

		return "", fmt.Errorf("selected storage %q is not available on node %q", directStorage, node.Name)
	}

	return matchStorage(node.Name, storages, selector)
}

func matchStorage(nodeName string, storages proxmox.Storages, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		// If neither storage nor storage_selector was provided, pick the first enabled images-capable storage on the node
		for _, storage := range storages {
			if storage.Enabled != 0 && strings.Contains(storage.Content, "images") {
				return storage.Name, nil
			}
		}

		return "", fmt.Errorf("no storage or storage_selector specified and no default 'images' storage found on node %q", nodeName)
	}

	// Fast path: if selector is a plain storage name (not a CEL expression), match it directly
	for _, storage := range storages {
		if storage.Name == selector {
			return storage.Name, nil
		}
	}

	// Compile the CEL environment and expression once outside the storage loop
	env, err := cel.NewEnv(
		cel.Variable("name", cel.StringType),
		cel.Variable("node", cel.StringType),
		cel.Variable("storageType", cel.StringType),
		cel.Variable("availableSpace", cel.UintType),
	)
	if err != nil {
		return "", err
	}

	expr, err := siderocel.ParseBooleanExpression(selector, env)
	if err != nil {
		return "", err
	}

	for _, storage := range storages {
		matched, err := expr.EvalBool(env, map[string]any{
			"name":           storage.Name,
			"node":           nodeName,
			"storageType":    storage.Type,
			"availableSpace": storage.Avail,
		})
		if err != nil {
			return "", err
		}

		if matched {
			return storage.Name, nil
		}
	}

	return "", fmt.Errorf("failed to pick the disk: no matches for the condition %q", selector)
}

func (p *Provisioner) getVM(ctx context.Context, nodeName string, vmid int32) (*proxmox.VirtualMachine, error) {
	node, err := p.proxmoxClient.Node(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	return node.VirtualMachine(ctx, int(vmid))
}

// isTaskLive reports whether the task identified by id is still running or has
// already completed successfully. A failed, stopped or no-longer-known task
// returns false so the caller can re-issue the work instead of waiting on a
// task that will never finish.
func (p *Provisioner) isTaskLive(ctx context.Context, id string) bool {
	t := proxmox.NewTask(proxmox.UPID(id), p.proxmoxClient)

	if err := t.Ping(ctx); err != nil {
		return false
	}

	return t.IsRunning || t.IsSuccessful
}

func (p *Provisioner) checkTaskStatus(ctx context.Context, id string) error {
	t := proxmox.NewTask(proxmox.UPID(id), p.proxmoxClient)

	if err := t.Ping(ctx); err != nil {
		return err
	}

	switch {
	case t.IsRunning:
		return provision.NewRetryInterval(time.Second * 10)
	case t.IsSuccessful:
		return nil
	}

	return taskFailure(ctx, t)
}

func isTaskFailed(err error) bool {
	return errors.Is(err, errTaskFailed)
}

// retryStart records one more failed vm.Start() and reports whether another attempt is allowed.
// On give up the spec is left untouched: giveUp persists the terminal state instead.
func retryStart(spec *specs.MachineSpec) bool {
	if spec.StartAttempts+1 >= maxStartAttempts {
		return false
	}

	spec.StartAttempts++
	spec.VmStartTask = ""

	return true
}

// giveUp destroys the machine VM and records cause as the terminal failure.
//
// A plain error returned from a step rolls back all of its state changes, so the failure
// is persisted through a requeue error first. On the next reconcile the step sees Failure
// and returns it as a plain error, putting the request into the failed stage without
// touching Proxmox again.
func (p *Provisioner) giveUp(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine], cause error) error {
	spec := pctx.State.TypedSpec().Value

	logger.Error("giving up provisioning, destroying VM", zap.Int32("vmid", spec.Vmid), zap.Error(cause))

	if err := p.destroyVM(ctx, logger, spec.Node, spec.Vmid); err != nil {
		return fmt.Errorf("%w; failed to destroy VM: %w", cause, err)
	}

	if pool := spec.Pool; pool != "" {
		p.cleanupPool(ctx, logger, pool)
	}

	spec.Vmid = 0
	spec.Failure = cause.Error()
	spec.VmStartTask = ""

	return controller.NewRequeueError(cause, time.Second)
}

// destroyVM stops (unless already stopped) and deletes the VM, treating an already removed VM or node as success.
func (p *Provisioner) destroyVM(ctx context.Context, logger *zap.Logger, node string, vmid int32) error {
	vm, err := p.getVM(ctx, node, vmid)
	if err != nil {
		// The VM, or the node it was provisioned on, was removed out-of-band
		// (directly in Proxmox). There is nothing left to tear down.
		if isAlreadyGoneErr(err) {
			logger.Info("VM or its node no longer exists, treating as already deleted",
				zap.String("node", node), zap.Int32("vmid", vmid), zap.Error(err))

			return nil
		}

		return err
	}

	if !vm.IsStopped() {
		task, stopErr := vm.Stop(ctx)
		if stopErr != nil {
			return stopErr
		}

		if stopErr = p.waitForTaskToFinish(ctx, task); stopErr != nil {
			return stopErr
		}
	}

	task, err := vm.Delete(ctx, nil)
	if err != nil {
		return err
	}

	return p.waitForTaskToFinish(ctx, task)
}

// taskPollInterval is how often waitForTaskToFinish polls a task, tests shorten it.
var taskPollInterval = time.Second * 5

func (p *Provisioner) waitForTaskToFinish(ctx context.Context, t *proxmox.Task) error {
	ticker := time.NewTicker(taskPollInterval)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := t.Ping(ctx); err != nil {
				return err
			}

			switch {
			case t.IsFailed:
				return taskFailure(ctx, t)
			case t.IsSuccessful:
				return nil
			}
		}
	}
}

// taskFailure builds the error for a task that finished without success. Proxmox puts only a short
// summary into the exit status (e.g., "start failed: QEMU exited with code 1"), the actual reason is in
// the task log, so the error carries the last lines of it too.
func taskFailure(ctx context.Context, t *proxmox.Task) error {
	err := fmt.Errorf("%w: %s %s on node %q: %s", errTaskFailed, t.Type, t.ID, t.Node, cmp.Or(t.ExitStatus, t.Status))

	log, logErr := t.Log(ctx, 0, 1000)
	if logErr != nil {
		return fmt.Errorf("%w (failed to read the task log: %w)", err, logErr)
	}

	lineNumbers := slices.Sorted(maps.Keys(log))

	lines := make([]string, 0, len(lineNumbers))

	for _, n := range lineNumbers {
		line := strings.TrimSpace(log[n])

		// the final "TASK ERROR: ..." line repeats the exit status
		if line == "" || strings.HasPrefix(line, "TASK ") {
			continue
		}

		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return err
	}

	return fmt.Errorf("%w, task log: %s", err, strings.Join(lines[max(0, len(lines)-taskLogTailLines):], " | "))
}

type nodeStatus struct {
	Name                     string
	MemoryFree               float64
	CPUFree                  float64
	FreeMem                  uint64
	SameMachineRequestSetVMs int
	TotalVMs                 int
}

// shouldCountSetVMs disables the client-side set spread under HA, where Proxmox owns placement.
func shouldCountSetVMs(data Data, hasSet bool) bool {
	return hasSet && data.HA == nil
}

// isAlreadyGoneErr reports whether err means the VM, or the node it lived on,
// no longer exists in Proxmox — e.g. it was deleted or the node was removed
// out-of-band. On teardown such a machine has nothing left to clean up, so the
// caller treats it as already deprovisioned instead of failing forever.
//
// go-proxmox flattens every 500 into errors.New(res.Status) with no type or
// status code (proxmox.go handleResponse), so the reason phrase is the only
// signal. The phrasings matched here are, respectively: a missing VM config
// file, a node dropped from the cluster, and a node whose hostname no longer
// resolves while Proxmox proxies the per-node request.
func isAlreadyGoneErr(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	switch {
	case strings.Contains(msg, "does not exist"):
		return true
	case strings.Contains(msg, "no such node"):
		return true
	case strings.Contains(msg, "hostname lookup") && strings.Contains(msg, "failed to get address info"):
		return true
	default:
		return false
	}
}

func pickNode(nodeInfoList []nodeStatus) nodeStatus {
	// Auto-pick node with most free memory (and lowest CPU load on ties) and with the least number of machines from the same machine request set
	slices.SortFunc(nodeInfoList, func(a, b nodeStatus) int {
		if c := cmp.Compare(a.SameMachineRequestSetVMs, b.SameMachineRequestSetVMs); c != 0 {
			return c
		}

		if c := -cmp.Compare(a.MemoryFree, b.MemoryFree); c != 0 {
			return c
		}

		return -cmp.Compare(a.CPUFree, b.CPUFree)
	})

	return nodeInfoList[0]
}
