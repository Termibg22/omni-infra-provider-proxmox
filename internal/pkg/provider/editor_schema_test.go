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

	for _, key := range []string{"disks", "nics"} {
		field, fieldOK := properties[key].(map[string]any)
		require.True(t, fieldOK)
		require.NotContains(t, field, "default", "must not replace existing legacy devices")
		require.NotContains(t, field, "minItems", "an empty override retains the legacy devices")
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

func TestEditorSchemaKeepsAllLegacyAndAdvancedFields(t *testing.T) {
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
			"cores", "memory", "disk_size", "storage_selector", "network_bridge", "bios", "cpu_flags",
			"talos_extensions", "ha", "replication", "firewall_options", "pci_devices", "usb_devices", "_section_boot_disk", "_section_primary_nic",
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
