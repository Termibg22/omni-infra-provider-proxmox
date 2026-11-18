// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider //nolint:testpackage // Verify the exact discovery and editor schema contracts.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.yaml.in/yaml/v4"
)

func TestAdditionalDiskDefaultSize(t *testing.T) {
	for _, item := range []string{"{}", "{disk_size: null}", "{disk_size: 0}", "{disk_size: 20}"} {
		t.Run(item, func(t *testing.T) {
			var data Data

			require.NoError(t, yaml.Unmarshal([]byte("disk_size: 48\nadditional_disks: ["+item+"]\n"), &data))
			data.ApplyWorkloadProfile()
			require.NoError(t, data.Validate())
			disks := data.EffectiveDisks()
			require.Len(t, disks, 2)
			require.Equal(t, 48, disks[0].EffectiveSize())
			require.Equal(t, 20, disks[1].EffectiveSize())
		})
	}
}

func TestProvisioningModeOverridesSavedTemplate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		clone bool
	}{
		{"default ISO", "{}", false},
		{"legacy template", "template_vmid: '9000'", true},
		{"explicit clone", "provisioning_mode: clone\ntemplate_vmid: '9000'", true},
		{"ISO keeps template", "provisioning_mode: iso\ntemplate_vmid: '9000'", false},
		{"cleared mode keeps template", "provisioning_mode: ''\ntemplate_vmid: '9000'", false},
		{"grouped legacy template", "os: {template_vmid: '9000'}", true},
		{"grouped clone", "os: {provisioning_mode: clone, template_vmid: '9000'}", true},
		{"grouped ISO", "os: {provisioning_mode: iso, template_vmid: '9000'}", false},
		{"grouped cleared mode", "os: {provisioning_mode: '', template_vmid: '9000'}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data Data

			require.NoError(t, yaml.Unmarshal([]byte(tc.input), &data))
			data.ApplyWorkloadProfile()
			require.NoError(t, data.Validate())
			require.Equal(t, tc.clone, data.IsTemplateClone())

			if strings.Contains(tc.input, "9000") {
				require.Equal(t, "9000", data.TemplateVMID, "changing mode must preserve the saved template")
			}
		})
	}
}

func TestCloneModeRequiresTemplate(t *testing.T) {
	for _, template := range []string{"", " ", "none"} {
		data := Data{ProvisioningMode: "clone", TemplateVMID: template}
		data.ApplyWorkloadProfile()
		require.ErrorContains(t, data.Validate(), "template_vmid is required in clone mode")
	}
}

func TestTemplateLookupRejectsAmbiguousNamesAndOversizedIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/cluster/resources", r.URL.Path)
		respond(t, w, []map[string]any{
			{"type": "qemu", "node": "a", "vmid": 9000, "template": 1, "name": "Talos"},
			{"type": "qemu", "node": "b", "vmid": 9001, "template": 1, "name": "talos"},
			{"type": "lxc", "node": "b", "vmid": 9002, "template": 1, "name": "container"},
		})
	}))
	defer server.Close()

	p := NewProvisioner(proxmox.NewClient(server.URL))
	for template, want := range map[string]string{"TALOS": "multiple VM templates", "container": "not found", "4294976296": "exceeds"} {
		_, err := p.findTemplateVM(t.Context(), Data{TemplateVMID: template})
		require.ErrorContains(t, err, want)
	}
}

func TestInvalidHardwareLimitsDoNotBecomeUnlimited(t *testing.T) {
	for _, input := range []string{"cpu_limit: .nan", "disks: [{bus: sas}]", "disks: [{mbps_read: .inf}]", "disks: [{iops_write: -1}]", "additional_nics: [{rate_limit: -1}]"} {
		var data Data

		require.NoError(t, yaml.Unmarshal([]byte(input), &data))
		data.ApplyWorkloadProfile()
		require.Error(t, data.Validate(), input)
	}
}

func TestClearedCloneModeRequiresReprovisioning(t *testing.T) {
	const old = "template_vmid: '9000'"

	var data Data

	require.NoError(t, yaml.Unmarshal([]byte(old+"\nprovisioning_mode: ''\n"), &data))
	data.ApplyWorkloadProfile()
	_, err := validateLiveUpdate(old, data)
	require.ErrorContains(t, err, "changing installation media")
}

func TestAdditionalDiskAutoKeepsLegacyStorageSelector(t *testing.T) {
	var data Data

	require.NoError(t, yaml.Unmarshal([]byte(`
storage_selector: name == "vm-zfs"
additional_disks:
  - storage: auto
    disk_size: 100
`), &data))
	disks := data.EffectiveDisks()
	require.Len(t, disks, 2)
	require.Empty(t, disks[1].Storage)
	require.Equal(t, `name == "vm-zfs"`, disks[1].StorageSelector)
}

func TestIncompleteStorageDiscoveryDoesNotOfferGlobalOrStaleStorage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{{"type": "storage", "node": "pve2", "storage": "stale", "content": "images"}})
	})
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{{"node": "pve", "status": "online"}})
	})
	mux.HandleFunc("/nodes/pve/status", func(w http.ResponseWriter, _ *http.Request) { respond(t, w, map[string]any{}) })
	mux.HandleFunc("/nodes/pve/storage", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	opts := DiscoverClusterOptions(t.Context(), proxmox.NewClient(server.URL), zap.NewNop())
	require.False(t, opts.DiscoveryComplete)
	require.Empty(t, opts.Nodes)
	require.Empty(t, opts.DiskStorages)
	require.Empty(t, opts.NodeTopologies)
}

func TestEditorDiscoveryUsesCurrentNodesAndUsableVMDiskStorage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{{"type": "node", "node": "pve2"}, {"type": "storage", "node": "pve2", "storage": "stale", "content": "images"}})
	})
	mux.HandleFunc("/storage", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{{"storage": "local-lvm", "content": "images", "nodes": "pve2"}, {"storage": "global-only", "content": "images"}})
	})
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{{"node": "pve", "status": "online"}})
	})
	mux.HandleFunc("/nodes/pve/status", func(w http.ResponseWriter, _ *http.Request) { respond(t, w, map[string]any{}) })
	mux.HandleFunc("/nodes/pve/storage", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{
			{"storage": "vm-zfs", "content": "images,rootdir", "enabled": 1, "active": 1},
			{"storage": "local-lvm", "content": "rootdir", "enabled": 1, "active": 1},
			{"storage": "disabled", "content": "images", "enabled": 0, "active": 1},
			{"storage": "inactive", "content": "images", "enabled": 1, "active": 0},
			{"storage": "local", "content": "iso,backup", "enabled": 1, "active": 1},
		})
	})
	mux.HandleFunc("/nodes/pve/network", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{{"iface": "vmbr0", "type": "bridge"}})
	})
	mux.HandleFunc("/cluster/mapping/pci", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{{"id": "old-gpu", "map": []string{"node=pve2,id=0000:01:00.0"}}})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	opts := DiscoverClusterOptions(t.Context(), proxmox.NewClient(server.URL), zap.NewNop(), map[string][]string{"stale-group": {"pve2"}, "current-group": {"pve", "pve2"}})
	require.True(t, opts.DiscoveryComplete)
	require.Equal(t, []string{"pve"}, opts.Nodes)
	require.Equal(t, []string{"vm-zfs"}, opts.DiskStorages)
	require.Equal(t, []string{"local"}, opts.ISOStorages)
	require.Equal(t, map[string][]string{"current-group": {"pve"}}, opts.NodeGroups)
	require.NotContains(t, opts.NodeTopologies, "pve2")
	require.Empty(t, opts.PCIMappings)
}

// Omni 1.13.3 clamps absent numbers with Math.max/min and writes NaN into the
// form. Bounds must remain in validation subschemas, away from that renderer.
func TestEditorSchemaDoesNotMutateAbsentNumbersOrOverrideLegacyDevices(t *testing.T) {
	base, err := os.ReadFile("../../../cmd/omni-infra-provider-proxmox/data/schema.json")
	require.NoError(t, err)

	var root map[string]any
	require.NoError(t, json.Unmarshal(base, &root))
	assertOmniSafeNumericBounds(t, root, "schema")

	properties, ok := root["properties"].(map[string]any)
	require.True(t, ok)

	for _, key := range []string{"storage_selector", "disks", "nics", "firewall_options", "replication"} {
		require.NotContains(t, properties, key)
	}
}

func assertOmniSafeNumericBounds(t *testing.T, value any, path string) {
	t.Helper()

	switch obj := value.(type) {
	case map[string]any:
		if kind := fmt.Sprint(obj["type"]); strings.Contains(kind, "integer") || strings.Contains(kind, "number") {
			require.NotContains(t, obj, "minimum", path)
			require.NotContains(t, obj, "maximum", path)
		}

		for key, child := range obj {
			assertOmniSafeNumericBounds(t, child, path+"."+key)
		}
	case []any:
		for _, child := range obj {
			assertOmniSafeNumericBounds(t, child, path+"[]")
		}
	}
}

func TestEditorSchemaKeepsRequestedVMControls(t *testing.T) {
	base, err := os.ReadFile("../../../cmd/omni-infra-provider-proxmox/data/schema.json")
	require.NoError(t, err)

	for _, mode := range []string{"basic", "advanced"} {
		built, buildErr := BuildUISchema(string(base), mode)
		require.NoError(t, buildErr)
		built, buildErr = BuildDynamicSchema(built, ClusterOptions{Nodes: []string{"pve"}, DiskStorages: []string{"vm-zfs"}, NetworkBridges: []string{"vmbr0"}})
		require.NoError(t, buildErr)

		var root map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(built), &root))

		var properties map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(root["properties"], &properties))

		for _, key := range []string{
			"cores", "memory", "disk_size", "storage", "network_bridge", "bios", "cpu_flags",
			"talos_extensions", "ha", "pci_devices", "usb_devices", "_section_boot_disk", "_section_primary_nic",
		} {
			require.Contains(t, properties, key)
		}

		var storage struct {
			OneOf []struct {
				Const string `json:"const"`
				Title string `json:"title"`
			} `json:"oneOf"`
		}
		require.NoError(t, json.Unmarshal(properties["storage"], &storage))
		require.Len(t, storage.OneOf, 2)
		require.Empty(t, storage.OneOf[0].Const)
		require.Equal(t, " ", storage.OneOf[0].Title)
		require.Equal(t, "vm-zfs", storage.OneOf[1].Const)
		require.NotContains(t, properties, "workload_profile")
		require.NotContains(t, properties, "bonds")
		require.NotContains(t, built, "legacy format")
		require.NotContains(t, root, "x-omni-uischema")
	}
}

func TestClearedSelectorsUseDefaultsAndSkipEmptyHAEntries(t *testing.T) {
	var data Data
	require.NoError(t, yaml.Unmarshal([]byte(`
node: ""
node_group: ""
pool: ""
storage: ""
network_bridge: ""
disk_cache: none
security_groups: ["", web]
ha:
  group: ""
  rules: ["", spread-workers]
  node_affinity_nodes: ["", pve1]
`), &data))
	data.Normalize()
	require.Empty(t, data.Node)
	require.Empty(t, data.NodeGroup)
	require.Empty(t, data.Pool)
	require.Empty(t, data.EffectiveDisks()[0].Storage)
	require.Equal(t, "none", data.EffectiveDisks()[0].Cache)
	require.Equal(t, "vmbr0", data.EffectiveNICs()[0].Bridge)
	require.Equal(t, []string{"web"}, data.SecurityGroups)
	require.Equal(t, []string{"spread-workers"}, data.HA.Rules)
	require.Equal(t, []string{"pve1"}, data.HA.NodeAffinityNodes)
}

func TestMappingDiscoveryExcludesRemovedNodesAndUnmappedDevices(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/mapping/pci", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{
			{"id": "gpu", "map": []string{"node=pve1,path=0000:01:00.0", "node=removed,path=0000:02:00.0"}},
			{"id": "stale", "map": []string{"node=removed,path=0000:01:00.0"}},
			{"id": "unmapped", "map": []string{}},
		})
	})
	mux.HandleFunc("/cluster/mapping/usb", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{
			{"id": "usb", "map": []string{"node=pve2,id=1234:5678"}},
			{"id": "old-usb", "map": []string{"node=removed,id=1234:5678"}},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	acc := newDiscoveryAccumulator(nil)
	acc.nodesSet = map[string]struct{}{"pve1": {}, "pve2": {}}
	acc.ensureNode("pve1")
	acc.ensureNode("pve2")
	acc.discoverClusterMappings(t.Context(), new(proxmox.Cluster).New(proxmox.NewClient(server.URL)), zap.NewNop())
	opts := acc.buildOptions()
	require.Equal(t, []string{"gpu"}, opts.PCIMappings)
	require.Equal(t, []string{"usb"}, opts.USBMappings)
	require.Equal(t, []string{"pve1"}, opts.NodeGroups["pci-gpu"])
	require.NotContains(t, opts.NodeGroups, "pci-stale")
	require.Empty(t, opts.TopologyForNodes([]string{"pve1"}).USBMappings)
	require.Empty(t, opts.TopologyForNodes([]string{"pve2"}).PCIMappings)
	require.Empty(t, opts.TopologyForNodes([]string{"removed"}).PCIMappings)
	require.Equal(t, []string{"gpu"}, opts.TopologyForNodes([]string{"pve1", "pve2"}).PCIMappings)
}

func TestHardwarePlacementRequiresOneHostAndReportsAPIErrors(t *testing.T) {
	for _, failure := range []string{"", "pci", "usb"} {
		t.Run("failure="+failure, func(t *testing.T) {
			mux := http.NewServeMux()
			for _, kind := range []string{"pci", "usb"} {
				mux.HandleFunc("/cluster/mapping/"+kind, func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, http.MethodGet, r.Method)

					if failure == kind {
						http.Error(w, "permission denied", http.StatusForbidden)

						return
					}

					node := "pve1"
					if kind == "usb" {
						node = "pve2"
					}

					respond(t, w, []map[string]any{{"id": kind, "map": []string{"node=" + node}}})
				})
			}

			server := httptest.NewServer(mux)
			defer server.Close()

			p := NewProvisioner(proxmox.NewClient(server.URL))
			nodes := proxmox.NodeStatuses{{Node: "pve1", Status: "online"}, {Node: "pve2", Status: "online"}}
			selected, err := p.filterNodesByGroupAndTopology(t.Context(), zap.NewNop(), nodes, Data{
				PCIDevices: []PCIDevice{{Mapping: "pci"}},
				USBDevices: []USBDevice{{Mapping: "usb"}},
			})
			require.Empty(t, selected)

			if failure == "" {
				require.ErrorContains(t, err, "no node provides all requested PCI/USB mappings")
			} else {
				require.ErrorContains(t, err, "cannot read Proxmox "+strings.ToUpper(failure)+" resource mappings")
			}
		})
	}
}
