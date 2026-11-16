// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/ha"
)

// ReplicationConfig configures a Proxmox ZFS/storage replication job (/cluster/replication) for the VM.
type ReplicationConfig struct {
	Schedule   string  `yaml:"schedule,omitempty"`
	TargetNode string  `yaml:"target_node,omitempty"`
	RateLimit  float64 `yaml:"rate_limit,omitempty"`
	Enabled    bool    `yaml:"enabled,omitempty"`
}

// VMFirewallOptions configures VM-level Proxmox firewall policy (/nodes/{node}/qemu/{vmid}/firewall/options).
type VMFirewallOptions struct {
	Enable     *bool  `yaml:"enable,omitempty"`
	DHCP       *bool  `yaml:"dhcp,omitempty"`
	MACFilter  *bool  `yaml:"macfilter,omitempty"`
	IPFilter   *bool  `yaml:"ipfilter,omitempty"`
	NDP        *bool  `yaml:"ndp,omitempty"`
	Radv       *bool  `yaml:"radv,omitempty"`
	PolicyIn   string `yaml:"policy_in,omitempty"`  // ACCEPT, DROP, REJECT
	PolicyOut  string `yaml:"policy_out,omitempty"` // ACCEPT, DROP, REJECT
	LogLevelIn string `yaml:"log_level_in,omitempty"`
}

// NetBond represents a Cloud-Init NoCloud v2 bonded network interface (e.g. LACP 802.3ad or active-backup).
type NetBond struct {
	DHCP4              *bool    `yaml:"dhcp4,omitempty"`
	DHCP6              *bool    `yaml:"dhcp6,omitempty"`
	Name               string   `yaml:"name"`                           // e.g. "bond0"
	Mode               string   `yaml:"mode,omitempty"`                 // "802.3ad", "active-backup", "balance-xor", "balance-tlb", "balance-alb"
	LACPRate           string   `yaml:"lacp_rate,omitempty"`            // "fast" or "slow"
	TransmitHashPolicy string   `yaml:"transmit_hash_policy,omitempty"` // "layer2", "layer2+3", "layer3+4"
	IPPool             string   `yaml:"ip_pool,omitempty"`
	Gateway4           string   `yaml:"gateway4,omitempty"`
	Gateway6           string   `yaml:"gateway6,omitempty"`
	Interfaces         []string `yaml:"interfaces"` // e.g. ["eth0", "eth1"]
	Addresses          []string `yaml:"addresses,omitempty"`
	Nameservers        []string `yaml:"nameservers,omitempty"`
	SearchDomains      []string `yaml:"search_domains,omitempty"`
	Routes             []Route  `yaml:"routes,omitempty"`
	MIIMonitorInterval uint32   `yaml:"mii_monitor_interval,omitempty"`
	MTU                int      `yaml:"mtu,omitempty"`
}

// GeneralSection groups Proxmox VM general placement and metadata fields from the UI schema.
type GeneralSection struct {
	OnBoot            *bool    `yaml:"onboot,omitempty"`
	AutoCreatePool    *bool    `yaml:"auto_create_pool,omitempty"`
	WorkloadProfile   string   `yaml:"workload_profile,omitempty"`
	NodeGroup         string   `yaml:"node_group,omitempty"`
	Node              string   `yaml:"node,omitempty"`
	PlacementStrategy string   `yaml:"placement_strategy,omitempty"`
	Pool              string   `yaml:"pool,omitempty"`
	Description       string   `yaml:"description,omitempty"`
	Tags              []string `yaml:"tags,omitempty"`
	MinVMID           int      `yaml:"min_vmid,omitempty"`
	MaxVMID           int      `yaml:"max_vmid,omitempty"`
	Protection        bool     `yaml:"protection,omitempty"`
}

// OSSection groups Proxmox OS / ISO / Template provisioning fields from the UI schema.
type OSSection struct {
	FullClone        *bool    `yaml:"full_clone,omitempty"`
	ProvisioningMode string   `yaml:"provisioning_mode,omitempty"`
	ISOStorage       string   `yaml:"iso_storage,omitempty"`
	TemplateVMID     string   `yaml:"template_vmid,omitempty"`
	Architecture     string   `yaml:"architecture,omitempty"`
	TalosExtensions  []string `yaml:"talos_extensions,omitempty"`
	ExtraKernelArgs  []string `yaml:"extra_kernel_args,omitempty"`
}

// SystemSection groups Proxmox VM system, firmware, display, and watchdog fields from the UI schema.
type SystemSection struct {
	RNG            *bool  `yaml:"rng,omitempty"`
	MachineType    string `yaml:"machine_type,omitempty"`
	Bios           string `yaml:"bios,omitempty"`
	SCSIHW         string `yaml:"scsihw,omitempty"`
	VGA            string `yaml:"vga,omitempty"`
	WatchdogModel  string `yaml:"watchdog_model,omitempty"`
	WatchdogAction string `yaml:"watchdog_action,omitempty"`
	StartupOrder   string `yaml:"startup_order,omitempty"`
	SecureBoot     bool   `yaml:"secure_boot,omitempty"`
	TPM            bool   `yaml:"tpm,omitempty"`
	SerialConsole  bool   `yaml:"serial_console,omitempty"`
}

// ComputeSection groups Proxmox CPU and memory fields from the UI schema.
type ComputeSection struct {
	Balloon   *bool   `yaml:"balloon,omitempty"`
	CPUType   string  `yaml:"cpu_type,omitempty"`
	CPUFlags  string  `yaml:"cpu_flags,omitempty"`
	Hugepages string  `yaml:"hugepages,omitempty"`
	Memory    uint64  `yaml:"memory,omitempty"`
	CPULimit  float64 `yaml:"cpu_limit,omitempty"`
	Cores     int     `yaml:"cores,omitempty"`
	Sockets   int     `yaml:"sockets,omitempty"`
	CPUUnits  int     `yaml:"cpu_units,omitempty"`
	NUMA      bool    `yaml:"numa,omitempty"`
}

// NetworkSection groups Proxmox NICs, LACP bonds, and global DNS fields from the UI schema.
type NetworkSection struct {
	NICs             []NIC     `yaml:"nics,omitempty"`
	Bonds            []NetBond `yaml:"bonds,omitempty"`
	DNSServers       []string  `yaml:"dns_servers,omitempty"`
	DNSSearchDomains []string  `yaml:"dns_search_domains,omitempty"`
}

// FirewallSection groups Proxmox security groups and VM firewall options from the UI schema.
type FirewallSection struct {
	Options        *VMFirewallOptions `yaml:"options,omitempty"`
	SecurityGroups []string           `yaml:"security_groups,omitempty"`
}

// PassthroughSection groups PCI and USB hardware passthrough devices from the UI schema.
type PassthroughSection struct {
	PCIDevices []PCIDevice `yaml:"pci_devices,omitempty"`
	USBDevices []USBDevice `yaml:"usb_devices,omitempty"`
}

// Data is the provider custom machine config.
type Data struct {
	explicitFields map[string]bool
	General        *GeneralSection     `yaml:"general,omitempty"`
	OS             *OSSection          `yaml:"os,omitempty"`
	System         *SystemSection      `yaml:"system,omitempty"`
	Compute        *ComputeSection     `yaml:"compute,omitempty"`
	Network        *NetworkSection     `yaml:"network,omitempty"`
	Firewall       *FirewallSection    `yaml:"firewall,omitempty"`
	Passthrough    *PassthroughSection `yaml:"passthrough,omitempty"`
	Balloon        *bool               `yaml:"balloon,omitempty"`
	// HA registers the VM as a Proxmox HA resource; its presence also hands placement to HA (pickNode stops spreading the set).
	HA               *ha.Config         `yaml:"ha,omitempty"`
	Replication      *ReplicationConfig `yaml:"replication,omitempty"`
	FirewallOptions  *VMFirewallOptions `yaml:"firewall_options,omitempty"`
	NetworkFirewall  *bool              `yaml:"network_firewall,omitempty"`
	OnBoot           *bool              `yaml:"onboot,omitempty"`
	AutoCreatePool   *bool              `yaml:"auto_create_pool,omitempty"`
	FullClone        *bool              `yaml:"full_clone,omitempty"`
	RNG              *bool              `yaml:"rng,omitempty"`
	WorkloadProfile  string             `yaml:"workload_profile,omitempty"`
	NodeGroup        string             `yaml:"node_group,omitempty"`
	Node             string             `yaml:"node,omitempty"`
	ProvisioningMode string             `yaml:"provisioning_mode,omitempty"`
	TemplateVMID     string             `yaml:"template_vmid,omitempty"`
	ISOStorage       string             `yaml:"iso_storage,omitempty"`
	Storage          string             `yaml:"storage,omitempty"`
	StorageSelector  string             `yaml:"storage_selector,omitempty"`
	NetworkBridge    string             `yaml:"network_bridge,omitempty"`
	Hugepages        string             `yaml:"hugepages,omitempty"`
	MachineType      string             `yaml:"machine_type,omitempty"`
	Bios             string             `yaml:"bios,omitempty"`
	VGA              string             `yaml:"vga,omitempty"`
	CPUType          string             `yaml:"cpu_type,omitempty"`
	CPUFlags         string             `yaml:"cpu_flags,omitempty"`
	Architecture     string             `yaml:"architecture,omitempty"`
	SCSIHW           string             `yaml:"scsihw,omitempty"`
	DiskAIO          string             `yaml:"disk_aio,omitempty"`
	DiskCache        string             `yaml:"disk_cache,omitempty"`
	Pool             string             `yaml:"pool,omitempty"`
	Description      string             `yaml:"description,omitempty"`
	StartupOrder     string             `yaml:"startup_order,omitempty"`
	WatchdogModel    string             `yaml:"watchdog_model,omitempty"`
	WatchdogAction   string             `yaml:"watchdog_action,omitempty"`
	// PlacementStrategy selects how a node is chosen for an auto-provisioned VM:
	// spread (default), fewer-vms, round-robin or binpack.
	PlacementStrategy string           `yaml:"placement_strategy,omitempty"`
	Disks             []Disk           `yaml:"disks,omitempty"`
	NICs              []NIC            `yaml:"nics,omitempty"`
	Bonds             []NetBond        `yaml:"bonds,omitempty"`
	AdditionalDisks   []AdditionalDisk `yaml:"additional_disks,omitempty"`
	AdditionalNICs    []AdditionalNIC  `yaml:"additional_nics,omitempty"`
	PCIDevices        []PCIDevice      `yaml:"pci_devices,omitempty"`
	USBDevices        []USBDevice      `yaml:"usb_devices,omitempty"`
	Tags              []string         `yaml:"tags,omitempty"`
	DNSServers        []string         `yaml:"dns_servers,omitempty"`
	DNSSearchDomains  []string         `yaml:"dns_search_domains,omitempty"`
	SecurityGroups    []string         `yaml:"security_groups,omitempty"`
	TalosExtensions   []string         `yaml:"talos_extensions,omitempty"`
	ExtraKernelArgs   []string         `yaml:"extra_kernel_args,omitempty"`
	Vlan              uint64           `yaml:"vlan,omitempty"`
	Memory            uint64           `yaml:"memory"`
	CPULimit          float64          `yaml:"cpu_limit,omitempty"`
	Sockets           int              `yaml:"sockets"`
	DiskSize          int              `yaml:"disk_size,omitempty"`
	Cores             int              `yaml:"cores"`
	CPUUnits          int              `yaml:"cpu_units,omitempty"`
	MinVMID           int              `yaml:"min_vmid,omitempty"`
	MaxVMID           int              `yaml:"max_vmid,omitempty"`
	DiskIOThread      bool             `yaml:"disk_iothread,omitempty"`
	NUMA              bool             `yaml:"numa,omitempty"`
	DiskDiscard       bool             `yaml:"disk_discard,omitempty"`
	DiskSSD           bool             `yaml:"disk_ssd,omitempty"`
	SecureBoot        bool             `yaml:"secure_boot,omitempty"`
	TPM               bool             `yaml:"tpm,omitempty"`
	Protection        bool             `yaml:"protection,omitempty"`
	SerialConsole     bool             `yaml:"serial_console,omitempty"`
	normalized        bool
	haDisabled        bool
}

func stripSentinel(v string) string {
	switch strings.TrimSpace(v) {
	case "auto", "none":
		return ""
	default:
		return v
	}
}

// Normalize flattens Proxmox-grouped UI schema sections (`general`, `os`, `system`, `compute`,
// `network`, `firewall`, `passthrough`) onto top-level Data fields and strips UI sentinel values
// (`auto`, `none`) while preserving full backward compatibility with flat YAML configs.
func (d *Data) Normalize() {
	if d.normalized {
		return
	}

	d.normalized = true

	d.normalizeGeneralAndOS()
	d.normalizeSystemAndCompute()
	d.normalizeNetworkFirewallPassthrough()
	d.normalizeSentinels()
}

func (d *Data) normalizeGeneralAndOS() {
	if g := d.General; g != nil {
		d.WorkloadProfile = cmp.Or(g.WorkloadProfile, d.WorkloadProfile)
		d.NodeGroup = cmp.Or(g.NodeGroup, d.NodeGroup)
		d.Node = cmp.Or(g.Node, d.Node)
		d.PlacementStrategy = cmp.Or(g.PlacementStrategy, d.PlacementStrategy)
		d.Pool = cmp.Or(g.Pool, d.Pool)
		d.Description = cmp.Or(g.Description, d.Description)
		d.MinVMID = cmp.Or(g.MinVMID, d.MinVMID)

		d.MaxVMID = cmp.Or(g.MaxVMID, d.MaxVMID)
		if d.explicitFields["general.protection"] {
			d.Protection = g.Protection
		} else {
			d.Protection = g.Protection || d.Protection
		}

		if g.OnBoot != nil && d.OnBoot == nil {
			d.OnBoot = g.OnBoot
		}

		if g.AutoCreatePool != nil && d.AutoCreatePool == nil {
			d.AutoCreatePool = g.AutoCreatePool
		}

		if len(g.Tags) > 0 && len(d.Tags) == 0 {
			d.Tags = g.Tags
		}
	}

	if o := d.OS; o != nil {
		d.ProvisioningMode = cmp.Or(o.ProvisioningMode, d.ProvisioningMode)
		d.ISOStorage = cmp.Or(o.ISOStorage, d.ISOStorage)
		d.TemplateVMID = cmp.Or(o.TemplateVMID, d.TemplateVMID)
		d.Architecture = cmp.Or(o.Architecture, d.Architecture)

		if o.FullClone != nil && d.FullClone == nil {
			d.FullClone = o.FullClone
		}

		if len(o.TalosExtensions) > 0 && len(d.TalosExtensions) == 0 {
			d.TalosExtensions = o.TalosExtensions
		}

		if len(o.ExtraKernelArgs) > 0 && len(d.ExtraKernelArgs) == 0 {
			d.ExtraKernelArgs = o.ExtraKernelArgs
		}
	}
}

func (d *Data) normalizeSystemAndCompute() {
	if s := d.System; s != nil {
		d.MachineType = cmp.Or(s.MachineType, d.MachineType)
		d.Bios = cmp.Or(s.Bios, d.Bios)
		d.SCSIHW = cmp.Or(s.SCSIHW, d.SCSIHW)
		d.VGA = cmp.Or(s.VGA, d.VGA)
		d.WatchdogModel = cmp.Or(s.WatchdogModel, d.WatchdogModel)
		d.WatchdogAction = cmp.Or(s.WatchdogAction, d.WatchdogAction)

		d.StartupOrder = cmp.Or(s.StartupOrder, d.StartupOrder)
		if d.explicitFields["system.secure_boot"] {
			d.SecureBoot = s.SecureBoot
		} else {
			d.SecureBoot = s.SecureBoot || d.SecureBoot
		}

		if d.explicitFields["system.tpm"] {
			d.TPM = s.TPM
		} else {
			d.TPM = s.TPM || d.TPM
		}

		if d.explicitFields["system.serial_console"] {
			d.SerialConsole = s.SerialConsole
		} else {
			d.SerialConsole = s.SerialConsole || d.SerialConsole
		}

		if s.RNG != nil && d.RNG == nil {
			d.RNG = s.RNG
		}
	}

	if c := d.Compute; c != nil {
		d.Cores = cmp.Or(c.Cores, d.Cores)
		d.Sockets = cmp.Or(c.Sockets, d.Sockets)
		d.CPUType = cmp.Or(c.CPUType, d.CPUType)
		d.CPUFlags = cmp.Or(c.CPUFlags, d.CPUFlags)
		d.Hugepages = cmp.Or(c.Hugepages, d.Hugepages)
		d.Memory = cmp.Or(c.Memory, d.Memory)
		d.CPULimit = cmp.Or(c.CPULimit, d.CPULimit)

		d.CPUUnits = cmp.Or(c.CPUUnits, d.CPUUnits)
		if d.explicitFields["compute.numa"] {
			d.NUMA = c.NUMA
		} else {
			d.NUMA = c.NUMA || d.NUMA
		}

		if c.Balloon != nil && d.Balloon == nil {
			d.Balloon = c.Balloon
		}
	}
}

func (d *Data) normalizeNetworkFirewallPassthrough() {
	if n := d.Network; n != nil {
		if len(n.NICs) > 0 && len(d.NICs) == 0 {
			d.NICs = n.NICs
		}

		if len(n.Bonds) > 0 && len(d.Bonds) == 0 {
			d.Bonds = n.Bonds
		}

		if len(n.DNSServers) > 0 && len(d.DNSServers) == 0 {
			d.DNSServers = n.DNSServers
		}

		if len(n.DNSSearchDomains) > 0 && len(d.DNSSearchDomains) == 0 {
			d.DNSSearchDomains = n.DNSSearchDomains
		}
	}

	if fw := d.Firewall; fw != nil {
		if len(fw.SecurityGroups) > 0 && len(d.SecurityGroups) == 0 {
			d.SecurityGroups = fw.SecurityGroups
		}

		if fw.Options != nil && d.FirewallOptions == nil {
			d.FirewallOptions = fw.Options
		}
	}

	if p := d.Passthrough; p != nil {
		if len(p.PCIDevices) > 0 && len(d.PCIDevices) == 0 {
			d.PCIDevices = p.PCIDevices
		}

		if len(p.USBDevices) > 0 && len(d.USBDevices) == 0 {
			d.USBDevices = p.USBDevices
		}
	}
}

func (d *Data) normalizeSentinels() {
	d.NodeGroup = stripSentinel(d.NodeGroup)
	d.Node = stripSentinel(d.Node)
	d.Pool = stripSentinel(d.Pool)
	d.ISOStorage = stripSentinel(d.ISOStorage)
	d.TemplateVMID = stripSentinel(d.TemplateVMID)
	d.Storage = stripSentinel(d.Storage)
	d.WatchdogModel = stripSentinel(d.WatchdogModel)
	d.Hugepages = stripSentinel(d.Hugepages)

	for i := range d.Disks {
		d.Disks[i].Storage = stripSentinel(d.Disks[i].Storage)
	}

	for i := range d.AdditionalDisks {
		d.AdditionalDisks[i].Storage = stripSentinel(d.AdditionalDisks[i].Storage)
	}

	if d.Replication != nil {
		d.Replication.TargetNode = stripSentinel(d.Replication.TargetNode)
	}

	if d.HA != nil {
		if strings.TrimSpace(d.HA.State) == "none" {
			d.haDisabled = true
			d.HA = nil

			return
		}

		d.HA.Group = stripSentinel(d.HA.Group)

		d.HA.ResourceAffinity = stripSentinel(d.HA.ResourceAffinity)
		if d.HA.ResourceAffinity == "anti-affinity" {
			d.HA.ResourceAffinity = "negative"
		}
	}
}

// ApplyWorkloadProfile applies intelligent defaults for the selected `workload_profile`
// without overwriting any field the user explicitly set.
func (d *Data) ApplyWorkloadProfile() {
	d.Normalize()

	switch d.WorkloadProfile {
	case "control-plane":
		d.StartupOrder = cmp.Or(d.StartupOrder, "order=1,up=30")
		d.WatchdogModel = cmp.Or(d.WatchdogModel, "i6300esb")

		d.WatchdogAction = cmp.Or(d.WatchdogAction, "reset")
		if !d.isExplicit("protection") {
			d.Protection = true
		}

		if d.RNG == nil {
			t := true
			d.RNG = &t
		}

		if d.HA == nil && !d.haDisabled {
			d.HA = &ha.Config{
				State:            "started",
				ResourceAffinity: "negative",
			}
		}
	case "storage-node":
		d.SCSIHW = cmp.Or(d.SCSIHW, "virtio-scsi-single")
		d.DiskCache = cmp.Or(d.DiskCache, "none")

		d.DiskAIO = cmp.Or(d.DiskAIO, "io_uring")
		if !d.isExplicit("disk_ssd") {
			d.DiskSSD = true
		}

		if !d.isExplicit("disk_discard") {
			d.DiskDiscard = true
		}

		if !d.isExplicit("disk_iothread") {
			d.DiskIOThread = true
		}
	case "gpu-ai-worker":
		d.MachineType = cmp.Or(d.MachineType, "q35")
		d.CPUType = cmp.Or(d.CPUType, "host")

		d.Bios = cmp.Or(d.Bios, "ovmf")
		if !d.isExplicit("numa") {
			d.NUMA = true
		}

		if d.Balloon == nil {
			f := false
			d.Balloon = &f
		}
	case "hardened-secure":
		d.Bios = cmp.Or(d.Bios, "ovmf")
		if !d.isExplicit("secure_boot") {
			d.SecureBoot = true
		}

		if !d.isExplicit("tpm") {
			d.TPM = true
		}

		if !d.isExplicit("protection") {
			d.Protection = true
		}

		if d.RNG == nil {
			t := true
			d.RNG = &t
		}
	}
}

// EffectiveExtensions returns the deduplicated list of Talos system extensions to install,
// combining `siderolabs/qemu-guest-agent`, workload-profile extensions, and `talos_extensions`.
func (d *Data) EffectiveExtensions() []string {
	exts := []string{"siderolabs/qemu-guest-agent"}

	switch d.WorkloadProfile {
	case "storage-node":
		exts = append(exts, "siderolabs/iscsi-tools", "siderolabs/util-linux-tools")
	case "gpu-ai-worker":
		exts = append(exts, "siderolabs/nonfree-kmod-nvidia-production", "siderolabs/nvidia-container-toolkit-production")
	}

	for _, ext := range d.TalosExtensions {
		ext = strings.TrimSpace(ext)
		if ext != "" && !slices.Contains(exts, ext) {
			exts = append(exts, ext)
		}
	}

	return exts
}

// IsTemplateClone reports whether this MachineClass clones an existing Proxmox VM template.
func (d *Data) IsTemplateClone() bool {
	return d.ProvisioningMode == "clone" || strings.TrimSpace(d.TemplateVMID) != ""
}

// ParsedTemplateVMID extracts the integer VMID from TemplateVMID (which may be "9000" or "9000 (talos-tpl)").
func (d *Data) ParsedTemplateVMID() (int, string) {
	raw := strings.TrimSpace(d.TemplateVMID)
	if raw == "" {
		return 0, ""
	}

	idPart, _, _ := strings.Cut(raw, " ")
	if id, err := strconv.Atoi(strings.TrimSpace(idPart)); err == nil && id > 0 {
		return id, raw
	}

	return 0, raw
}

// Disk represents a flexible VM disk configuration (primary or additional).
type Disk struct {
	Replicate       *bool `yaml:"replicate,omitempty"`
	explicitFields  map[string]bool
	Backup          *bool   `yaml:"backup,omitempty"`
	Storage         string  `yaml:"storage,omitempty"`
	StorageSelector string  `yaml:"storage_selector,omitempty"`
	Bus             string  `yaml:"bus,omitempty"`
	Cache           string  `yaml:"cache,omitempty"`
	DiskCache       string  `yaml:"disk_cache,omitempty"`
	AIO             string  `yaml:"aio,omitempty"`
	DiskAIO         string  `yaml:"disk_aio,omitempty"`
	VolumeName      string  `yaml:"volume_name,omitempty"`
	MBPSRead        float64 `yaml:"mbps_read,omitempty"`
	MBPSWrite       float64 `yaml:"mbps_write,omitempty"`
	Size            int     `yaml:"size,omitempty"`
	DiskSize        int     `yaml:"disk_size,omitempty"`
	IOPSRead        int     `yaml:"iops_read,omitempty"`
	IOPSWrite       int     `yaml:"iops_write,omitempty"`
	SSD             bool    `yaml:"ssd,omitempty"`
	DiskSSD         bool    `yaml:"disk_ssd,omitempty"`
	Discard         bool    `yaml:"discard,omitempty"`
	DiskDiscard     bool    `yaml:"disk_discard,omitempty"`
	IOThread        bool    `yaml:"iothread,omitempty"`
	DiskIOThread    bool    `yaml:"disk_iothread,omitempty"`
}

// EffectiveSize returns Size or falls back to DiskSize (and default 20GB if both are 0).
func (d Disk) EffectiveSize() int {
	if s := cmp.Or(d.Size, d.DiskSize); s > 0 {
		return s
	}

	return 20
}

// EffectiveBus returns the normalized disk bus ("scsi", "virtio", "sata").
func (d Disk) EffectiveBus() string {
	switch d.Bus {
	case "virtio", "sata", "scsi":
		return d.Bus
	default:
		return "scsi"
	}
}

// EffectiveCache returns Cache or DiskCache.
func (d Disk) EffectiveCache() string {
	return cmp.Or(d.Cache, d.DiskCache)
}

// EffectiveAIO returns AIO or DiskAIO.
func (d Disk) EffectiveAIO() string {
	return cmp.Or(d.AIO, d.DiskAIO)
}

// AdditionalDisk represents a legacy additional disk configuration.
type AdditionalDisk struct {
	Storage         string `yaml:"storage,omitempty"`
	StorageSelector string `yaml:"storage_selector,omitempty"`
	DiskCache       string `yaml:"disk_cache,omitempty"`
	DiskAIO         string `yaml:"disk_aio,omitempty"`
	DiskSize        int    `yaml:"disk_size"`
	DiskSSD         bool   `yaml:"disk_ssd,omitempty"`
	DiskDiscard     bool   `yaml:"disk_discard,omitempty"`
	DiskIOThread    bool   `yaml:"disk_iothread,omitempty"`
}

// NIC represents a flexible network interface configuration (primary or additional).
type NIC struct {
	Firewall      *bool    `yaml:"firewall,omitempty"`
	DHCP4         *bool    `yaml:"dhcp4,omitempty"`
	DHCP6         *bool    `yaml:"dhcp6,omitempty"`
	Bridge        string   `yaml:"bridge"`
	Model         string   `yaml:"model,omitempty"`
	MACAddress    string   `yaml:"mac_address,omitempty"`
	Trunks        string   `yaml:"trunks,omitempty"`
	IPPool        string   `yaml:"ip_pool,omitempty"`
	Gateway4      string   `yaml:"gateway4,omitempty"`
	Gateway6      string   `yaml:"gateway6,omitempty"`
	Addresses     []string `yaml:"addresses,omitempty"`
	Nameservers   []string `yaml:"nameservers,omitempty"`
	SearchDomains []string `yaml:"search_domains,omitempty"`
	Routes        []Route  `yaml:"routes,omitempty"`
	Vlan          uint64   `yaml:"vlan,omitempty"`
	RateLimit     float64  `yaml:"rate_limit,omitempty"`
	MTU           int      `yaml:"mtu,omitempty"`
	Queues        int      `yaml:"queues,omitempty"`
}

// PCIDevice represents a PCI device passthrough configuration using Proxmox Resource Mappings.
type PCIDevice struct {
	ROMBar     *bool  `yaml:"rombar,omitempty"`      // Enable ROM BAR (default true, set false to disable)
	Mapping    string `yaml:"mapping"`               // Resource mapping name (e.g., nvidia-gpu-1)
	MDev       string `yaml:"mdev"`                  // Mediated device name (e.g., nvidia-180)
	PCIExpress bool   `yaml:"pcie,omitempty"`        // Use PCIe instead of PCI (recommended for GPUs)
	PrimaryGPU bool   `yaml:"primary_gpu,omitempty"` // Set as primary GPU (x-vga=1)
}

// USBDevice represents a USB device passthrough configuration using Proxmox Resource Mappings.
type USBDevice struct {
	Mapping string `yaml:"mapping"`        // Resource mapping name (e.g., rtl-sdr)
	USB3    bool   `yaml:"usb3,omitempty"` // Use the USB 3 controller
}

// AdditionalNIC represents an additional network interface configuration.
type AdditionalNIC struct {
	Bridge    string   `yaml:"bridge"`               // Network bridge (e.g., vmbr1)
	Model     string   `yaml:"model,omitempty"`      // NIC model (virtio, e1000, vmxnet3)
	IPPool    string   `yaml:"ip_pool,omitempty"`    // Optional static IP pool range
	Gateway4  string   `yaml:"gateway4,omitempty"`   // Optional IPv4 gateway
	Addresses []string `yaml:"addresses,omitempty"`  // Optional static CIDR addresses
	Vlan      uint64   `yaml:"vlan,omitempty"`       // Optional VLAN tag
	RateLimit float64  `yaml:"rate_limit,omitempty"` // Optional rate limit in MB/s
	MTU       int      `yaml:"mtu,omitempty"`        // Optional MTU
	Queues    int      `yaml:"queues,omitempty"`     // Optional multiqueue count
	Firewall  bool     `yaml:"firewall,omitempty"`   // Enable firewall (default: false for storage networks)
}

// EffectiveDisks resolves the complete ordered list of VM disks by merging the unified
// `disks` array with legacy top-level `disk_size`/`storage`/`storage_selector` and `additional_disks`.
func (d *Data) EffectiveDisks() []Disk {
	d.ApplyWorkloadProfile()

	disks := make([]Disk, 0, max(1, len(d.Disks))+len(d.AdditionalDisks))

	if len(d.Disks) > 0 {
		for _, disk := range d.Disks {
			if disk.Storage == "" && disk.StorageSelector == "" {
				disk.Storage = d.Storage
				disk.StorageSelector = d.StorageSelector
			}

			if d.WorkloadProfile == "storage-node" {
				disk.Cache = cmp.Or(disk.Cache, disk.DiskCache, d.DiskCache)

				disk.AIO = cmp.Or(disk.AIO, disk.DiskAIO, d.DiskAIO)
				if !disk.explicitFields["ssd"] && !disk.explicitFields["disk_ssd"] && !d.isExplicit("disk_ssd") {
					disk.SSD = true
				}

				if !disk.explicitFields["discard"] && !disk.explicitFields["disk_discard"] && !d.isExplicit("disk_discard") {
					disk.Discard = true
				}

				if !disk.explicitFields["iothread"] && !disk.explicitFields["disk_iothread"] && !d.isExplicit("disk_iothread") {
					disk.IOThread = true
				}
			}

			disks = append(disks, disk)
		}
	} else {
		disks = append(disks, Disk{
			Storage:         d.Storage,
			StorageSelector: d.StorageSelector,
			Size:            cmp.Or(d.DiskSize, 20),
			Bus:             "scsi",
			SSD:             d.DiskSSD,
			Discard:         d.DiskDiscard,
			IOThread:        d.DiskIOThread,
			Cache:           d.DiskCache,
			AIO:             d.DiskAIO,
		})
	}

	for _, add := range d.AdditionalDisks {
		storage := add.Storage
		selector := add.StorageSelector

		if storage == "" && selector == "" {
			storage = d.Storage
			selector = d.StorageSelector
		}

		disks = append(disks, Disk{
			Storage:         storage,
			StorageSelector: selector,
			Size:            add.DiskSize,
			Bus:             "scsi",
			SSD:             add.DiskSSD,
			Discard:         add.DiskDiscard,
			IOThread:        add.DiskIOThread,
			Cache:           add.DiskCache,
			AIO:             add.DiskAIO,
		})
	}

	return disks
}

// EffectiveNICs resolves the complete ordered list of VM network interfaces by merging
// the unified `nics` array with legacy `network_bridge`/`vlan`/`network_firewall` and `additional_nics`.
func (d *Data) EffectiveNICs() []NIC {
	nics := make([]NIC, 0, max(1, len(d.NICs))+len(d.AdditionalNICs))

	if len(d.NICs) > 0 {
		for i, nic := range d.NICs {
			if nic.Bridge == "" {
				nic.Bridge = cmp.Or(d.NetworkBridge, "vmbr0")
			}

			if nic.Firewall == nil {
				// Primary NIC defaults to firewall=true; secondary NICs default to false unless specified
				def := i == 0
				if i == 0 && d.NetworkFirewall != nil {
					def = *d.NetworkFirewall
				}

				nic.Firewall = &def
			}

			nics = append(nics, nic)
		}
	} else {
		primaryFw := true
		if d.NetworkFirewall != nil {
			primaryFw = *d.NetworkFirewall
		}

		nics = append(nics, NIC{
			Bridge:   cmp.Or(d.NetworkBridge, "vmbr0"),
			Model:    "virtio",
			Vlan:     d.Vlan,
			Firewall: &primaryFw,
		})
	}

	for _, add := range d.AdditionalNICs {
		fw := add.Firewall

		nics = append(nics, NIC{
			Bridge:    cmp.Or(add.Bridge, "vmbr0"),
			Model:     cmp.Or(add.Model, "virtio"),
			Vlan:      add.Vlan,
			Firewall:  &fw,
			MTU:       add.MTU,
			Queues:    add.Queues,
			RateLimit: add.RateLimit,
			IPPool:    add.IPPool,
			Addresses: add.Addresses,
			Gateway4:  add.Gateway4,
		})
	}

	return nics
}
