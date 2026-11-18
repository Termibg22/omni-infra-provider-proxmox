// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/config"
)

var volumeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

func validLimit(value float64) bool {
	return value >= 0 && !math.IsInf(value, 0)
}

// Validate checks invariants shared by the UI, flat YAML and live updates.
// Call after ApplyWorkloadProfile, before issuing any mutating API request.
func (d *Data) Validate() error {
	if d.Cores < 0 || d.Sockets < 0 || !validLimit(d.CPULimit) || d.CPUUnits < 0 {
		return fmt.Errorf("CPU values must be finite and nonnegative")
	}

	if err := config.ValidateVMIDRange(d.MinVMID, d.MaxVMID); err != nil {
		return err
	}

	if _, err := parseStrategy(d.PlacementStrategy); err != nil {
		return err
	}

	if d.ProvisioningMode != "" && d.ProvisioningMode != "iso" && d.ProvisioningMode != "clone" {
		return fmt.Errorf("provisioning_mode must be iso or clone")
	}

	if d.ProvisioningMode == "clone" && strings.TrimSpace(d.TemplateVMID) == "" {
		return fmt.Errorf("template_vmid is required in clone mode")
	}

	if d.SecureBoot && d.Bios != "ovmf" {
		return fmt.Errorf("secure_boot requires bios: ovmf")
	}

	if d.HA != nil && d.HA.ResourceAffinity != "" && d.HA.ResourceAffinity != "positive" && d.HA.ResourceAffinity != "negative" {
		return fmt.Errorf("ha.resource_affinity must be positive, negative or none")
	}

	if err := d.validateDisks(); err != nil {
		return err
	}

	if err := d.validateHardware(); err != nil {
		return err
	}

	return d.validateNetwork()
}

func (d *Data) validateDisks() error {
	serials := map[string]bool{}
	busCounts := map[string]int{}

	for i, disk := range d.EffectiveDisks() {
		if disk.Size < 0 || disk.DiskSize < 0 {
			return fmt.Errorf("disk %d size must be positive", i)
		}

		bus := disk.EffectiveBus()
		if disk.Bus != "" && disk.Bus != bus {
			return fmt.Errorf("disk %d: unknown bus %q (expected scsi, sata or virtio)", i, disk.Bus)
		}

		if disk.IOPSRead < 0 || disk.IOPSWrite < 0 || !validLimit(disk.MBPSRead) || !validLimit(disk.MBPSWrite) {
			return fmt.Errorf("disk %d: I/O limits must be finite and nonnegative", i)
		}

		busCounts[bus]++

		limit := map[string]int{"scsi": 31, "virtio": 16, "sata": 6}[bus]
		if busCounts[bus] > limit {
			return fmt.Errorf("too many %s disks (maximum %d)", bus, limit)
		}

		if disk.VolumeName != "" {
			if i == 0 || !volumeNamePattern.MatchString(disk.VolumeName) {
				return fmt.Errorf("disk %d: volume_name must name a data disk using lowercase letters, digits and hyphens", i)
			}

			serial := sanitizeVolumeSerial(disk.VolumeName)
			if serials[serial] {
				return fmt.Errorf("volume_name %q produces a duplicate disk serial", disk.VolumeName)
			}

			serials[serial] = true
		}
	}

	return nil
}

func (d *Data) validateNetwork() error {
	nics := d.EffectiveNICs()
	if len(nics) > 32 {
		return fmt.Errorf("at most 32 NICs can be configured")
	}

	for i, nic := range nics {
		if nic.MTU < 0 || nic.Queues < 0 || !validLimit(nic.RateLimit) {
			return fmt.Errorf("NIC %d: MTU, queues and rate limit must be finite and nonnegative", i)
		}

		if nic.Vlan > 4094 {
			return fmt.Errorf("NIC %d: VLAN must be between 0 and 4094", i)
		}

		if nic.IPPool != "" {
			if _, _, _, err := parseIPPool(nic.IPPool); err != nil {
				return fmt.Errorf("NIC %d: %w", i, err)
			}
		}
	}

	if err := d.validateBonds(nics); err != nil {
		return err
	}

	_, err := buildCloudInitNetworkConfig(*d, nics, nil, nil)

	return err
}

func (d *Data) validateBonds(nics []NIC) error {
	members := map[string]bool{}

	names := map[string]bool{}
	for _, bond := range d.Bonds {
		if names[bond.Name] {
			return fmt.Errorf("duplicate bond name %q", bond.Name)
		}

		names[bond.Name] = true
		if bond.IPPool != "" {
			if _, _, _, err := parseIPPool(bond.IPPool); err != nil {
				return err
			}
		}

		for _, iface := range bond.Interfaces {
			iface = normalizeBondInterfaceName(iface)

			idx, ok := parseIndexedDeviceSlot(iface, "eth")
			if !ok || idx < 0 || idx >= len(nics) || members[iface] {
				return fmt.Errorf("invalid or reused bond member %q", iface)
			}

			members[iface] = true

			nic := nics[idx]
			if nic.IPPool != "" || len(nic.Addresses) > 0 || nic.Gateway4 != "" || nic.Gateway6 != "" || len(nic.Routes) > 0 {
				return fmt.Errorf("bond member %s must configure addresses and routes on the bond instead", iface)
			}
		}
	}

	return nil
}
