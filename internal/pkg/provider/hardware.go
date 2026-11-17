// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/luthermonson/go-proxmox"
	"go.uber.org/zap"
)

var (
	hardwareNodePattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`)
	hardwareMappingPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	pciAddressPattern      = regexp.MustCompile(`^(?:[0-9a-fA-F]{4}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)
	usbAddressPattern      = regexp.MustCompile(`^[1-9][0-9]*-[1-9][0-9]*(?:\.[1-9][0-9]*)*$`)
)

type physicalDevice struct {
	Node    string
	Address string
	Name    string
}

func (d physicalDevice) selection() string {
	return d.Node + "/" + d.Address + " (" + strings.Join(strings.Fields(d.Name), " ") + ")"
}

// Omni's enum renderer needs the saved value to equal its visible label.
// Keep the stable node/address before the optional descriptive suffix. Plain
// resource mapping IDs remain unchanged for existing Machine Classes.
func parsePhysicalDevice(selection string) (physicalDevice, bool) {
	id, _, _ := strings.Cut(selection, " (")
	node, address, physical := strings.Cut(id, "/")

	return physicalDevice{Node: node, Address: address}, physical
}

func hardwareAddressValid(kind, address string) bool {
	if kind == "pci" {
		return pciAddressPattern.MatchString(address)
	}

	return usbAddressPattern.MatchString(address)
}

func hardwareOption(selection string) string {
	if device, physical := parsePhysicalDevice(selection); physical {
		return "host=" + device.Address
	}

	return "mapping=" + selection
}

func listPhysicalDevices(ctx context.Context, px *proxmox.Client, node, kind string) ([]physicalDevice, error) {
	if kind == "pci" {
		return listPhysicalPCI(ctx, px, node)
	}

	return listPhysicalUSB(ctx, px, node)
}

func listPhysicalPCI(ctx context.Context, px *proxmox.Client, node string) ([]physicalDevice, error) {
	// Decode only inventory fields we use; optional speed/mdev/check fields
	// must not prevent the entire list from being displayed.
	var records []struct {
		ID         string `json:"id"`
		Vendor     string `json:"vendor"`
		Device     string `json:"device"`
		VendorName string `json:"vendor_name"`
		DeviceName string `json:"device_name"`
	}
	if err := px.Get(ctx, fmt.Sprintf("/nodes/%s/hardware/pci", node), &records); err != nil {
		return nil, err
	}

	devices := make([]physicalDevice, 0, len(records))
	for _, record := range records {
		if !pciAddressPattern.MatchString(record.ID) {
			continue
		}

		name := strings.TrimSpace(record.VendorName + " " + record.DeviceName)
		if name == "" {
			name = "PCI " + record.Vendor + ":" + record.Device
		}

		devices = append(devices, physicalDevice{Node: node, Address: record.ID, Name: name})
	}

	return devices, nil
}

func listPhysicalUSB(ctx context.Context, px *proxmox.Client, node string) ([]physicalDevice, error) {
	var records []struct {
		Manufacturer string `json:"manufacturer"`
		Product      string `json:"product"`
		VendorID     string `json:"vendid"`
		ProductID    string `json:"prodid"`
		USBPath      string `json:"usbpath"`
		Bus          int    `json:"busnum"`
		Class        int    `json:"class"`
	}
	if err := px.Get(ctx, fmt.Sprintf("/nodes/%s/hardware/usb", node), &records); err != nil {
		return nil, err
	}

	devices := make([]physicalDevice, 0, len(records))
	for _, record := range records {
		// Match Proxmox's own USB selector: omit hubs and entries without a
		// device/port. Use bus + topology path, never the transient devnum.
		address := fmt.Sprintf("%d-%s", record.Bus, record.USBPath)
		if record.Class == 9 || record.ProductID == "" || !usbAddressPattern.MatchString(address) {
			continue
		}

		name := strings.TrimSpace(record.Manufacturer + " " + record.Product)
		if name == "" {
			name = "USB " + record.VendorID + ":" + record.ProductID
		}

		devices = append(devices, physicalDevice{Node: node, Address: address, Name: name})
	}

	return devices, nil
}

func (a *discoveryAccumulator) discoverPhysicalDevices(ctx context.Context, px *proxmox.Client, node string, logger *zap.Logger) {
	nt := a.ensureNode(node)
	for _, kind := range []string{"pci", "usb"} {
		devices, err := listPhysicalDevices(ctx, px, node, kind)
		if err != nil {
			logger.Warn("cannot discover physical passthrough devices", zap.String("node", node), zap.String("kind", kind), zap.Error(err))

			continue
		}

		global, local := a.pciSet, nt.pciMappings
		if kind == "usb" {
			global, local = a.usbSet, nt.usbMappings
		}

		for _, device := range devices {
			selection := device.selection()
			global[selection] = struct{}{}
			local[selection] = struct{}{}
		}
	}
}

func (d *Data) hardwareSelections(kind string) []string {
	var selections []string

	if kind == "pci" {
		for _, device := range d.PCIDevices {
			selections = append(selections, device.Mapping)
		}
	} else {
		for _, device := range d.USBDevices {
			selections = append(selections, device.Mapping)
		}
	}

	return selections
}

func (d *Data) usesResourceMappings(kind string) bool {
	return slices.ContainsFunc(d.hardwareSelections(kind), func(selection string) bool {
		_, physical := parsePhysicalDevice(selection)

		return !physical
	})
}

// Query only explicitly requested hosts, once per device kind. The descriptions
// in saved labels are display text; inventory membership is checked by address.
func (p *Provisioner) addPhysicalDeviceNodes(ctx context.Context, candidates proxmox.NodeStatuses, data Data, allowed map[string][]string) error {
	cache := map[string][]physicalDevice{}

	for _, kind := range []string{"pci", "usb"} {
		for _, selection := range data.hardwareSelections(kind) {
			device, physical := parsePhysicalDevice(selection)
			if !physical || !slices.ContainsFunc(candidates, func(node *proxmox.NodeStatus) bool {
				return node != nil && node.Node == device.Node
			}) {
				continue
			}

			key := kind + ":" + device.Node

			devices, cached := cache[key]
			if !cached {
				var err error

				devices, err = listPhysicalDevices(ctx, p.proxmoxClient, device.Node, kind)
				if err != nil {
					return fmt.Errorf("cannot read physical %s devices on node %q: %w", kind, device.Node, err)
				}

				cache[key] = devices
			}

			if slices.ContainsFunc(devices, func(current physicalDevice) bool { return current.Address == device.Address }) {
				allowed[kind+":"+selection] = []string{device.Node}
			}
		}
	}

	return nil
}

func (p *Provisioner) validatePhysicalDevicesOnNode(ctx context.Context, node string, data Data) error {
	if err := data.validateHardware(); err != nil {
		return err
	}

	hasPhysical := false

	for _, kind := range []string{"pci", "usb"} {
		for _, selection := range data.hardwareSelections(kind) {
			_, physical := parsePhysicalDevice(selection)
			hasPhysical = hasPhysical || physical
		}
	}

	if !hasPhysical {
		return nil
	}

	// A physical device and a named mapping must share the same host too.
	matched, err := p.filterNodesWithHardwareMappings(ctx, proxmox.NodeStatuses{{Node: node}}, data)
	if err != nil {
		return err
	}

	if len(matched) == 0 {
		return fmt.Errorf("selected PCI/USB devices are not available on node %q", node)
	}

	return nil
}

func (d *Data) validateHardware() error {
	physicalNode := ""

	for _, kind := range []string{"pci", "usb"} {
		seen := map[string]bool{}

		for i, selection := range d.hardwareSelections(kind) {
			device, physical := parsePhysicalDevice(selection)

			key := selection
			if physical {
				if !hardwareNodePattern.MatchString(device.Node) || !hardwareAddressValid(kind, device.Address) {
					return fmt.Errorf("%s device %d has an invalid node/address", kind, i)
				}

				if (d.Node != "" && d.Node != device.Node) || (physicalNode != "" && physicalNode != device.Node) {
					return fmt.Errorf("all physical PCI/USB devices must belong to the same target node")
				}

				physicalNode = device.Node
				key = device.Node + "/" + device.Address
			} else if !hardwareMappingPattern.MatchString(selection) {
				return fmt.Errorf("%s device %d requires a valid device or resource mapping", kind, i)
			}

			if seen[key] {
				return fmt.Errorf("%s device %q is selected more than once", kind, key)
			}

			seen[key] = true
		}
	}

	return nil
}
