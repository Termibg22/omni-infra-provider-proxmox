// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider //nolint:testpackage // Verify discovery, placement and the generated Proxmox configuration together.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	testPCISelection = "pve1/0000:01:00.0 (NVIDIA GeForce RTX 4090)"
	testUSBSelection = "pve1/1-2.3 (Silicon Labs CP210x)"
)

func hardwareInventoryServer(t *testing.T, failure string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method, "inventory and placement must not mutate Proxmox")

		if failure != "" && strings.HasSuffix(r.URL.Path, "/hardware/"+failure) {
			http.Error(w, "permission denied", http.StatusForbidden)

			return
		}

		switch r.URL.Path {
		case "/nodes":
			respond(t, w, []map[string]any{{"node": "pve1", "status": "online"}, {"node": "pve2", "status": "online"}, {"node": "offline", "status": "offline"}})
		case "/nodes/pve1/status", "/nodes/pve2/status":
			respond(t, w, map[string]any{})
		case "/nodes/pve1/storage", "/nodes/pve2/storage":
			respond(t, w, []map[string]any{{"storage": "local-zfs", "enabled": 1, "active": 1, "content": "images"}})
		case "/nodes/pve1/hardware/pci", "/nodes/pve2/hardware/pci":
			respond(t, w, []map[string]any{
				{"id": "0000:01:00.0", "vendor_name": "NVIDIA", "device_name": "GeForce RTX 4090", "mdev": 0},
				{"id": "0000:02:00.0", "vendor": "8086", "device": "10fb"},
				{"id": "invalid"},
			})
		case "/nodes/pve1/hardware/usb", "/nodes/pve2/hardware/usb":
			respond(t, w, []map[string]any{
				{"busnum": 1, "usbpath": "2.3", "class": 0, "vendid": "10c4", "prodid": "ea60", "manufacturer": "Silicon Labs", "product": "CP210x", "speed": 12, "devnum": 35},
				{"busnum": 2, "usbpath": "1", "class": 0, "vendid": "051d", "prodid": "0002"},
				{"busnum": 1, "usbpath": "2", "class": 9, "prodid": "0001", "product": "Hub"},
				{"busnum": 1, "class": 9, "prodid": "0002", "product": "Root hub"},
				{"busnum": 1, "usbpath": "3", "class": 0},
			})
		case "/cluster/mapping/pci":
			respond(t, w, []map[string]any{
				{"id": "mapped-gpu", "map": []string{"node=pve1,path=0000:01:00.0,id=10de:2684"}},
				{"id": "removed-gpu", "map": []string{"node=removed,path=0000:01:00.0,id=10de:2684"}},
			})
		case "/cluster/mapping/usb":
			respond(t, w, []map[string]any{{"id": "mapped-usb", "map": []string{"node=pve1,id=10c4:ea60,path=1-2.3"}}})
		default:
			require.NotContains(t, r.URL.Path, "/offline/", "offline node hardware must not be queried")
			require.NotContains(t, r.URL.Path, "/removed/", "removed node hardware must not be queried")
			respond(t, w, []any{})
		}
	}))
	t.Cleanup(server.Close)

	return server
}

func TestHardwareDiscoveryListsPhysicalDevicesAndMappings(t *testing.T) {
	server := hardwareInventoryServer(t, "")
	opts := DiscoverClusterOptions(t.Context(), proxmox.NewClient(server.URL), zap.NewNop())
	require.True(t, opts.DiscoveryComplete)
	require.Contains(t, opts.PCIMappings, testPCISelection)
	require.Contains(t, opts.PCIMappings, "pve2/0000:01:00.0 (NVIDIA GeForce RTX 4090)")
	require.Contains(t, opts.PCIMappings, "pve1/0000:02:00.0 (PCI 8086:10fb)")
	require.Contains(t, opts.PCIMappings, "mapped-gpu")
	require.NotContains(t, opts.PCIMappings, "removed-gpu")
	require.Contains(t, opts.USBMappings, testUSBSelection)
	require.Contains(t, opts.USBMappings, "pve1/2-1 (USB 051d:0002)")
	require.Contains(t, opts.USBMappings, "mapped-usb")
	require.Len(t, opts.PCIMappings, 5)
	require.Len(t, opts.USBMappings, 5)
	require.NotContains(t, opts.NodeTopologies["pve2"].PCIMappings, testPCISelection)
	require.Empty(t, opts.NodeTopologies["offline"].USBMappings)

	base, err := os.ReadFile("../../../cmd/omni-infra-provider-proxmox/data/schema.json")
	require.NoError(t, err)
	schema, err := BuildDynamicSchema(string(base), opts)
	require.NoError(t, err)
	require.Contains(t, schema, testPCISelection)
	require.Contains(t, schema, testUSBSelection)
	require.NotContains(t, schema, "Resource Affinity (PVE 9+)")
}

func TestHardwareInventoryFailureDoesNotHideMappingsOrOtherDeviceTypes(t *testing.T) {
	for _, kind := range []string{"pci", "usb"} {
		t.Run(kind, func(t *testing.T) {
			server := hardwareInventoryServer(t, kind)
			opts := DiscoverClusterOptions(t.Context(), proxmox.NewClient(server.URL), zap.NewNop())
			require.True(t, opts.DiscoveryComplete)
			require.Contains(t, opts.PCIMappings, "mapped-gpu")
			require.Contains(t, opts.USBMappings, "mapped-usb")

			if kind == "pci" {
				require.NotContains(t, opts.PCIMappings, testPCISelection)
				require.Contains(t, opts.USBMappings, testUSBSelection)
			} else {
				require.NotContains(t, opts.USBMappings, testUSBSelection)
				require.Contains(t, opts.PCIMappings, testPCISelection)
			}
		})
	}
}

func TestPhysicalHardwarePlacementAndConfiguration(t *testing.T) {
	server := hardwareInventoryServer(t, "")
	p := NewProvisioner(proxmox.NewClient(server.URL))
	data := Data{
		PCIDevices: []PCIDevice{{Mapping: testPCISelection, PCIExpress: true}},
		USBDevices: []USBDevice{{Mapping: testUSBSelection, USB3: true}},
	}
	nodes := proxmox.NodeStatuses{{Node: "pve1"}, {Node: "pve2"}}
	selected, err := p.filterNodesWithHardwareMappings(t.Context(), nodes, data)
	require.NoError(t, err)
	require.Equal(t, nodes[:1], selected, "the same PCI address on pve2 is a different device")
	require.Equal(t, "host=0000:01:00.0,pcie=1", buildPCIDeviceOptions(data.PCIDevices)[0].Value)
	require.Equal(t, "host=1-2.3,usb3=1", buildUSBDeviceOptions(data.USBDevices)[0].Value)
	require.NoError(t, p.validatePhysicalDevicesOnNode(t.Context(), "pve1", data))
	require.ErrorContains(t, p.validatePhysicalDevicesOnNode(t.Context(), "pve2", data), "not available on node")

	data.PCIDevices[0].Mapping = "mapped-gpu"
	selected, err = p.filterNodesWithHardwareMappings(t.Context(), nodes, data)
	require.NoError(t, err)
	require.Equal(t, nodes[:1], selected)
	require.Equal(t, "mapping=mapped-gpu,pcie=1", buildPCIDeviceOptions(data.PCIDevices)[0].Value)

	data.USBDevices[0].Mapping = "pve1/1-9 (Disconnected device)"
	require.ErrorContains(t, p.validatePhysicalDevicesOnNode(t.Context(), "pve1", data), "not available on node")

	data.USBDevices[0].Mapping = "pve2/1-2.3 (Silicon Labs CP210x)"
	require.ErrorContains(t, p.validatePhysicalDevicesOnNode(t.Context(), "pve2", data), "not available on node", "a mapped GPU on pve1 cannot be combined with physical USB on pve2")

	// Existing-VM placement must reject the mismatch before sending a migration
	// or configuration request. The test server rejects every non-GET request.
	_, err = p.reconcileVMNodePlacement(t.Context(), zap.NewNop(), nil, &proxmox.VirtualMachine{Node: "pve2"}, data)
	require.ErrorContains(t, err, "not available on node")
}

func TestHardwareSelectionValidation(t *testing.T) {
	for name, data := range map[string]Data{
		"empty":            {PCIDevices: []PCIDevice{{Mapping: ""}}},
		"invalid address":  {PCIDevices: []PCIDevice{{Mapping: "pve1/0000:01:00.0,pcie=1"}}},
		"invalid mapping":  {USBDevices: []USBDevice{{Mapping: "usb,host=1-2"}}},
		"different hosts":  {PCIDevices: []PCIDevice{{Mapping: testPCISelection}}, USBDevices: []USBDevice{{Mapping: "pve2/1-2"}}},
		"wrong target":     {Node: "pve2", USBDevices: []USBDevice{{Mapping: testUSBSelection}}},
		"duplicate device": {PCIDevices: []PCIDevice{{Mapping: testPCISelection}, {Mapping: "pve1/0000:01:00.0 (Other label)"}}},
	} {
		t.Run(name, func(t *testing.T) { require.Error(t, data.validateHardware()) })
	}
}

func TestPhysicalHardwareUnavailableAPIReportsNodeAndKind(t *testing.T) {
	server := hardwareInventoryServer(t, "usb")
	p := NewProvisioner(proxmox.NewClient(server.URL))
	data := Data{USBDevices: []USBDevice{{Mapping: testUSBSelection}}}
	_, err := p.filterNodesWithHardwareMappings(t.Context(), proxmox.NodeStatuses{{Node: "pve1"}}, data)
	require.ErrorContains(t, err, `cannot read physical usb devices on node "pve1"`)
}
