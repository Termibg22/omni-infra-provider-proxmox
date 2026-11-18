// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"cmp"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/luthermonson/go-proxmox"
	"go.yaml.in/yaml/v4"
)

// Reject changes which cannot be applied to a running Talos guest instead of
// recording a successful hash after silently ignoring them.
func validateLiveUpdate(previous string, desired Data) (deviceSlotCounts, error) {
	var old Data
	if err := yaml.Unmarshal([]byte(previous), &old); err != nil {
		return nil, fmt.Errorf("invalid applied provider data: %w", err)
	}

	old.ApplyWorkloadProfile()

	if old.HA != nil && desired.HA != nil {
		if old.HA.ResourceAffinity != desired.HA.ResourceAffinity || !slices.Equal(old.HA.Rules, desired.HA.Rules) {
			return nil, fmt.Errorf("changing HA rule memberships or resource affinity requires reprovisioning")
		}

		if len(old.HA.NodeAffinityNodes) > 0 && len(desired.HA.NodeAffinityNodes) == 0 {
			return nil, fmt.Errorf("removing HA node affinity requires reprovisioning")
		}
	}

	before, after := creationOnlyFields(old), creationOnlyFields(desired)
	for _, field := range slices.Sorted(maps.Keys(before)) {
		if !reflect.DeepEqual(before[field], after[field]) {
			return nil, fmt.Errorf("changing %s on an existing VM requires reprovisioning", field)
		}
	}

	counts := deviceSlotCounts{"net": len(old.EffectiveNICs()), "hostpci": len(old.PCIDevices), "usb": len(old.USBDevices)}
	previousDisks := map[string]Disk{}

	for _, disk := range old.EffectiveDisks() {
		previousDisks[counts.nextDiskSlot(disk)] = disk
	}

	desiredCounts := deviceSlotCounts{}
	for _, disk := range desired.EffectiveDisks() {
		slot := desiredCounts.nextDiskSlot(disk)

		previousDisk, exists := previousDisks[slot]
		if !exists {
			continue
		}

		if disk.Storage != previousDisk.Storage || (disk.Storage == "" && strings.TrimSpace(disk.StorageSelector) != strings.TrimSpace(previousDisk.StorageSelector)) {
			return nil, fmt.Errorf("changing disk %s storage or its selector on an existing VM requires reprovisioning", slot)
		}

		if size, previousSize := disk.EffectiveSize(), previousDisk.EffectiveSize(); size < previousSize {
			return nil, fmt.Errorf("cannot shrink disk %s on an existing VM from %d to %d GiB", slot, previousSize, size)
		}
	}

	return counts, nil
}

type deviceSlotCounts map[string]int

func (counts deviceSlotCounts) nextDiskSlot(disk Disk) string {
	bus := disk.EffectiveBus()
	index := counts[bus]
	counts[bus]++

	return fmt.Sprintf("%s%d", bus, index)
}

func (counts deviceSlotCounts) owns(slot string) bool {
	prefix := strings.TrimRight(slot, "0123456789")
	index, ok := parseIndexedDeviceSlot(slot, prefix)

	return ok && index >= 0 && index < counts[prefix]
}

func validateExistingVMDevices(vm *proxmox.VirtualMachine, previous deviceSlotCounts, desired Data) error {
	if err := validateExistingBootSlot(vm, desired.EffectiveDisks()); err != nil {
		return err
	}

	if previous == nil {
		return nil
	}

	config := vm.VirtualMachineConfig
	if config == nil {
		return fmt.Errorf("cannot verify device slots without the current VM configuration")
	}

	next := deviceSlotCounts{"net": len(desired.EffectiveNICs()), "hostpci": len(desired.PCIDevices), "usb": len(desired.USBDevices)}
	for _, disk := range desired.EffectiveDisks() {
		next[disk.EffectiveBus()]++
	}

	for _, devices := range []map[string]string{config.MergeDisks(), config.Nets, config.HostPCIs, config.USBs} {
		for _, slot := range slices.Sorted(maps.Keys(devices)) {
			if next.owns(slot) && !previous.owns(slot) {
				return fmt.Errorf("slot %s is already occupied and was not in the last applied configuration; verify its ownership in Proxmox before retrying", slot)
			}
		}
	}

	return nil
}

func creationOnlyFields(d Data) map[string]any {
	// Only the network card hardware is live-reconciled; L3 configuration is
	// delivered through the NoCloud ISO during initial provisioning.
	nics := d.EffectiveNICs()
	guestNICs := map[int]NIC{}

	for i := range nics {
		nics[i].Bridge = ""
		nics[i].Vlan = 0
		nics[i].Model = ""
		nics[i].Queues = 0
		nics[i].RateLimit = 0
		nics[i].Trunks = ""

		nics[i].Firewall = nil
		if !reflect.DeepEqual(nics[i], NIC{}) {
			guestNICs[i] = nics[i]
		}
	}

	volumes := []string{}

	for _, disk := range d.EffectiveDisks() {
		if disk.VolumeName != "" {
			volumes = append(volumes, disk.VolumeName)
		}
	}

	return map[string]any{
		"firmware":           []any{cmp.Or(d.Bios, "seabios"), d.SecureBoot, d.TPM, cmp.Or(d.MachineType, "pc"), d.SCSIHW, d.SerialConsole, d.Hugepages, d.Balloon, d.NUMA},
		"installation media": []any{d.ProvisioningMode, d.IsTemplateClone(), d.TemplateVMID, d.Architecture, d.EffectiveExtensions(), d.ExtraKernelArgs},
		"guest networking":   []any{guestNICs, d.Bonds, d.DNSServers, d.DNSSearchDomains},
		"Talos volumes":      volumes,
	}
}
