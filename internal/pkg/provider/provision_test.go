// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/ha"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

const (
	talosWorkers = "talos-workers"

	nodeAName = "node-a"
	nodeBName = "node-b"
	nodeCName = "node-c"

	worker1 = "worker-1"
	worker2 = "worker-2"
	worker3 = "worker-3"
)

func TestPickNode(t *testing.T) {
	const (
		nodeA = "NodeA"
		nodeB = "NodeB"
		nodeC = "NodeC"
		nodeD = "NodeD"
	)

	tests := []struct {
		name     string
		expected string
		input    []provider.NodeStatus
	}{
		{
			name: "Single node should be returned",
			input: []provider.NodeStatus{
				{Name: "node1", MemoryFree: 1, SameMachineRequestSetVMs: 0},
			},
			expected: "node1",
		},
		{
			name: "Primary criteria: Pick node with fewer same-set VMs",
			input: []provider.NodeStatus{
				{Name: nodeA, MemoryFree: 1.0, SameMachineRequestSetVMs: 10},
				// Node B has less memory, but is free (0 VMs) -> Should win
				{Name: nodeB, MemoryFree: 0.5, SameMachineRequestSetVMs: 0},
			},
			expected: nodeB,
		},
		{
			name: "Secondary criteria: If VMs equal, pick node with MOST free memory",
			input: []provider.NodeStatus{
				{Name: nodeA, MemoryFree: 0.5, SameMachineRequestSetVMs: 5},
				{Name: nodeB, MemoryFree: 1.0, SameMachineRequestSetVMs: 5}, // More memory
				{Name: nodeC, MemoryFree: 0.1, SameMachineRequestSetVMs: 5},
			},
			expected: nodeB,
		},
		{
			name: "Complex scenario",
			input: []provider.NodeStatus{
				{Name: nodeA, MemoryFree: 0.1, SameMachineRequestSetVMs: 2},
				{Name: nodeB, MemoryFree: 0.05, SameMachineRequestSetVMs: 1}, // Best VM count
				{Name: nodeC, MemoryFree: 0.04, SameMachineRequestSetVMs: 1}, // Same VM count, less memory than B
				{Name: nodeD, MemoryFree: 1, SameMachineRequestSetVMs: 5},
			},
			expected: nodeB,
		},
		{
			name: "No free memory",
			input: []provider.NodeStatus{
				{Name: nodeA, MemoryFree: 0, SameMachineRequestSetVMs: 0},
				{Name: nodeB, MemoryFree: 1, SameMachineRequestSetVMs: 0},
			},
			expected: nodeB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			result := provider.PickNode(tt.input)

			// Assert
			require.Equal(t, tt.expected, result.Name)
		})
	}
}

func TestShouldCountSetVMs(t *testing.T) {
	tests := []struct {
		name     string
		data     provider.Data
		hasSet   bool
		expected bool
	}{
		{name: "set, no HA: spread", hasSet: true, expected: true},
		{name: "set, HA: no spread", data: provider.Data{HA: &ha.Config{}}, hasSet: true, expected: false},
		{name: "no set, no HA: no spread", hasSet: false, expected: false},
		{name: "no set, HA: no spread", data: provider.Data{HA: &ha.Config{}}, hasSet: false, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, provider.ShouldCountSetVMs(tt.data, tt.hasSet))
		})
	}
}

func TestPoolCreateDecision(t *testing.T) {
	tests := []struct {
		name              string
		poolID            string
		machineRequestSet string
		exists            bool
		expectedCreate    bool
		expectedErr       bool
	}{
		{
			name:              "Pool exists: no-op",
			poolID:            "my-pool",
			machineRequestSet: talosWorkers,
			exists:            true,
			expectedCreate:    false,
		},
		{
			name:              "Pool absent, matches machine request set: create",
			poolID:            talosWorkers,
			machineRequestSet: talosWorkers,
			expectedCreate:    true,
		},
		{
			name:              "Pool absent, user-specified: refuse",
			poolID:            "gpu-pool",
			machineRequestSet: talosWorkers,
			expectedErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			create, err := provider.PoolCreateDecision(tt.exists, tt.poolID, tt.machineRequestSet)

			if tt.expectedErr {
				require.Error(t, err)
				require.False(t, create)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expectedCreate, create)
		})
	}
}

func TestBuildTagsOption(t *testing.T) {
	tests := []struct {
		name              string
		machineRequestSet string
		expectedValue     string
		userTags          []string
		expectedOk        bool
	}{
		{
			name:       "No user tags, no request set",
			expectedOk: false,
		},
		{
			name:              "Request set only",
			machineRequestSet: talosWorkers,
			expectedValue:     "machine-request.talos-workers",
			expectedOk:        true,
		},
		{
			name:          "User tags only",
			userTags:      []string{"talos-node", "prod"},
			expectedValue: "talos-node;prod",
			expectedOk:    true,
		},
		{
			name:              "User tags first, internal tag last",
			userTags:          []string{"talos-node", "prod"},
			machineRequestSet: talosWorkers,
			expectedValue:     "talos-node;prod;machine-request.talos-workers",
			expectedOk:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, ok := provider.BuildTagsOption(tt.userTags, tt.machineRequestSet)

			require.Equal(t, tt.expectedOk, ok)
			require.Equal(t, tt.expectedValue, value)
		})
	}
}

func TestSchedulerSpreadsInFlightPlacements(t *testing.T) {
	s := provider.NewScheduler()

	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 0.9},
			{Name: nodeBName, MemoryFree: 0.8},
			{Name: nodeCName, MemoryFree: 0.7},
		}
	}

	picked := make([]string, 0, 3)

	for _, requestID := range []string{worker1, worker2, worker3} {
		picked = append(picked, s.Pick(nodes(), talosWorkers, requestID, 0, "spread", nil).Name)
	}

	require.ElementsMatch(t, []string{nodeAName, nodeBName, nodeCName}, picked)
}

func TestSchedulerReleasesMaterializedReservations(t *testing.T) {
	s := provider.NewScheduler()

	twoNodes := func(proxmoxOnA int) []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 1.0, SameMachineRequestSetVMs: proxmoxOnA},
			{Name: nodeBName, MemoryFree: 0.9},
		}
	}

	require.Equal(t, nodeAName, s.Pick(twoNodes(0), talosWorkers, worker1, 0, "spread", nil).Name)
	require.Equal(t, nodeBName, s.Pick(twoNodes(0), talosWorkers, worker2, 0, "spread", nil).Name)

	picked := s.Pick(twoNodes(1), talosWorkers, worker3, 0, "spread", map[string]struct{}{worker1: {}})

	require.Equal(t, nodeAName, picked.Name)
}

func TestSchedulerExpiresStaleReservations(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	s := provider.NewSchedulerWithClock(func() time.Time { return now }, time.Minute)

	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 1.0},
			{Name: nodeBName, MemoryFree: 0.9},
		}
	}

	require.Equal(t, nodeAName, s.Pick(nodes(), talosWorkers, worker1, 0, "spread", nil).Name)

	now = now.Add(2 * time.Minute)

	require.Equal(t, nodeAName, s.Pick(nodes(), talosWorkers, worker2, 0, "spread", nil).Name)
}

func TestSchedulerReleaseFreesReservedNode(t *testing.T) {
	s := provider.NewScheduler()

	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 1.0},
			{Name: nodeBName, MemoryFree: 0.9},
		}
	}

	require.Equal(t, nodeAName, s.Pick(nodes(), talosWorkers, worker1, 0, "spread", nil).Name)
	require.Equal(t, nodeBName, s.Pick(nodes(), talosWorkers, worker2, 0, "spread", nil).Name)

	s.Release(worker2)

	require.Equal(t, nodeBName, s.Pick(nodes(), talosWorkers, worker3, 0, "spread", nil).Name)
}

func TestSchedulerRoundRobinIgnoresMemory(t *testing.T) {
	s := provider.NewScheduler()

	// Memory order (c>b>a) is the inverse of name order, so a name-ordered
	// result proves round-robin ignores free memory.
	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 0.1},
			{Name: nodeBName, MemoryFree: 0.5},
			{Name: nodeCName, MemoryFree: 0.9},
		}
	}

	picked := make([]string, 0, 3)

	for _, requestID := range []string{worker1, worker2, worker3} {
		picked = append(picked, s.Pick(nodes(), talosWorkers, requestID, 0, "round-robin", nil).Name)
	}

	require.Equal(t, []string{nodeAName, nodeBName, nodeCName}, picked)
}

func TestSchedulerFewerVMsBalancesTotalLoad(t *testing.T) {
	s := provider.NewScheduler()

	nodes := []provider.NodeStatus{
		{Name: nodeAName, TotalVMs: 9, SameMachineRequestSetVMs: 0, MemoryFree: 0.5},
		{Name: nodeBName, TotalVMs: 1, SameMachineRequestSetVMs: 3, MemoryFree: 0.5},
	}

	require.Equal(t, nodeBName, s.Pick(nodes, talosWorkers, worker1, 0, "fewer-vms", nil).Name)
}

func TestSchedulerBinpackConsolidatesOntoNodesThatFit(t *testing.T) {
	s := provider.NewScheduler()

	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, FreeMem: 100},
			{Name: nodeBName, FreeMem: 50},
		}
	}

	require.Equal(t, nodeBName, s.Pick(nodes(), talosWorkers, worker1, 40, "binpack", nil).Name)
	require.Equal(t, nodeAName, s.Pick(nodes(), talosWorkers, worker2, 60, "binpack", nil).Name)
}

func TestParseStrategy(t *testing.T) {
	for _, valid := range []string{"", "spread", "fewer-vms", "round-robin", "binpack"} {
		_, err := provider.ParseStrategy(valid)
		require.NoError(t, err)
	}

	_, err := provider.ParseStrategy("nonsense")
	require.Error(t, err)
}

func TestBuildFirmwareOptions(t *testing.T) {
	const vgaStd = "std"

	tests := []struct {
		expected map[string]any
		name     string
		data     provider.Data
	}{
		{
			name:     "No firmware or display options",
			expected: map[string]any{},
		},
		{
			name: "SeaBIOS with display option",
			data: provider.Data{
				Bios: "seabios",
				VGA:  vgaStd,
			},
			expected: map[string]any{
				"bios": "seabios",
				"vga":  vgaStd,
			},
		},
		{
			name: "OVMF creates EFI disk on selected storage",
			data: provider.Data{
				Bios: "ovmf",
				VGA:  vgaStd,
			},
			expected: map[string]any{
				"bios":     "ovmf",
				"efidisk0": "local-lvm:1,efitype=4m,pre-enrolled-keys=0",
				"vga":      vgaStd,
			},
		},
		{
			name: "OVMF lets the signed Talos ISO enroll its keys with TPM 2.0",
			data: provider.Data{
				Bios:       "ovmf",
				SecureBoot: true,
				TPM:        true,
				VGA:        "virtio",
			},
			expected: map[string]any{
				"bios":      "ovmf",
				"efidisk0":  "local-lvm:1,efitype=4m,pre-enrolled-keys=0",
				"tpmstate0": "local-lvm:1,version=v2.0",
				"vga":       "virtio",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, provider.BuildFirmwareOptions(tt.data, "local-lvm"))
		})
	}
}

func TestBuildUSBDeviceOptions(t *testing.T) {
	devices := []provider.USBDevice{
		{Mapping: "rtl-sdr", USB3: true},
		{Mapping: "zigbee-controller"},
	}

	require.Equal(t, map[string]any{
		"usb0": "mapping=rtl-sdr,usb3=1",
		"usb1": "mapping=zigbee-controller",
	}, provider.BuildUSBDeviceOptions(devices))
}

func TestEffectiveDisksAndBuildDiskOptions(t *testing.T) {
	noBackup := false
	data := provider.Data{
		Storage: "local-lvm",
		Disks: []provider.Disk{
			{
				Size:     40,
				SSD:      true,
				Discard:  true,
				IOThread: true,
				Cache:    "none",
				AIO:      "io_uring",
			},
			{
				Storage:   "ceph-rbd",
				Size:      200,
				Bus:       "scsi",
				SSD:       true,
				Discard:   true,
				Backup:    &noBackup,
				Replicate: new(true),
			},
			{
				Storage:  "nvme-pool",
				Size:     500,
				Bus:      "virtio",
				IOThread: true,
			},
		},
	}

	disks := data.EffectiveDisks()
	require.Len(t, disks, 3)
	require.Equal(t, "local-lvm", disks[0].Storage)
	require.Equal(t, "ceph-rbd", disks[1].Storage)
	require.Equal(t, "nvme-pool", disks[2].Storage)

	opts := provider.BuildDiskOptions(disks, []string{"local-lvm", "ceph-rbd", "nvme-pool"})
	require.Equal(t, map[string]any{
		"scsi0":   "local-lvm:40,ssd=1,discard=on,iothread=1,cache=none,aio=io_uring",
		"scsi1":   "ceph-rbd:200,ssd=1,discard=on,backup=0,replicate=1",
		"virtio0": "nvme-pool:500,iothread=1",
	}, opts)
}

func TestEffectiveNICsAndBuildNICOptions(t *testing.T) {
	fwTrue := true
	fwFalse := false
	data := provider.Data{
		NICs: []provider.NIC{
			{
				Bridge:   "vmbr0",
				Model:    "virtio",
				Vlan:     10,
				Firewall: &fwTrue,
				Queues:   4,
			},
			{
				Bridge:     "vmbr1",
				Model:      "virtio",
				Vlan:       20,
				Firewall:   &fwFalse,
				MTU:        9000,
				RateLimit:  1000,
				MACAddress: "BC:24:11:AA:BB:CC",
				Trunks:     "20;30",
			},
		},
	}

	nics := data.EffectiveNICs()
	require.Len(t, nics, 2)

	opts := provider.BuildNICOptions(nics)
	require.Equal(t, map[string]any{
		"net0": "virtio,bridge=vmbr0,firewall=1,tag=10,queues=4",
		"net1": "virtio=BC:24:11:AA:BB:CC,bridge=vmbr1,firewall=0,tag=20,mtu=9000,rate=1000,trunks=20;30",
	}, opts)
}

func TestBuildDynamicSchemaInjectsClusterOptions(t *testing.T) {
	baseSchema := `{
  "type": "object",
  "properties": {
    "general": {
      "type": "object",
      "properties": {
        "node_group": {"type": "string"},
        "node": {"type": "string"},
        "pool": {"type": "string"}
      }
    },
    "os": {
      "type": "object",
      "properties": {
        "iso_storage": {"type": "string"},
        "template_vmid": {"type": "string"}
      }
    },
    "compute": {
      "type": "object",
      "properties": {
        "cpu_type": {"type": "string"}
      }
    },
    "disks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "storage": {"type": "string"}
        }
      }
    },
    "network": {
      "type": "object",
      "properties": {
        "nics": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "bridge": {"type": "string"}
            }
          }
        }
      }
    },
    "passthrough": {
      "type": "object",
      "properties": {
        "pci_devices": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "mapping": {"type": "string"}
            }
          }
        },
        "usb_devices": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "mapping": {"type": "string"}
            }
          }
        }
      }
    },
    "ha": {
      "type": "object",
      "properties": {
        "group": {"type": "string"},
        "node_affinity_nodes": {"type": "array", "items": {"type": "string"}},
        "rules": {"type": "array", "items": {"type": "string"}}
      }
    }
  }
}`

	dynamic, err := provider.BuildDynamicSchema(baseSchema, provider.ClusterOptions{
		Nodes:          []string{"pve1", "pve2"},
		DiskStorages:   []string{"ceph-rbd", "local-lvm"},
		ISOStorages:    []string{"cephfs", "local"},
		NetworkBridges: []string{"vmbr0", "vmbr1", "vnet-storage"},
		Pools:          []string{"k8s-prod", "k8s-staging"},
		PCIMappings:    []string{"nvidia-l40s"},
		USBMappings:    []string{"zigbee-dongle"},
		HARules:        []string{"spread-workers"},
		CPUTypes:       []string{"x86-64-v2-AES", "host", "custom-epyc"},
	})
	require.NoError(t, err)
	require.Contains(t, dynamic, `"pve1"`)
	require.Contains(t, dynamic, `"ceph-rbd"`)
	require.Contains(t, dynamic, `"vnet-storage"`)
	require.Contains(t, dynamic, `"nvidia-l40s"`)
	require.Contains(t, dynamic, `"zigbee-dongle"`)
	require.Contains(t, dynamic, `"spread-workers"`)
	require.Contains(t, dynamic, `"custom-epyc"`)
	// The display title is nonempty for Reka, but the saved clearing value is empty.
	require.Contains(t, dynamic, `"const": ""`)
	require.Contains(t, dynamic, `"title": " "`)
	require.NotContains(t, dynamic, `"auto"`)
}

func TestIPPoolAllocator(t *testing.T) {
	alloc := provider.NewIPPoolAllocator()

	ip1, err := alloc.Allocate("192.168.10.100-192.168.10.101/24", "machine-1")
	require.NoError(t, err)
	require.Contains(t, []string{"192.168.10.100/24", "192.168.10.101/24"}, ip1)

	// Re-requesting for the same machine returns the identical IP
	ip1Again, err := alloc.Allocate("192.168.10.100-192.168.10.101/24", "machine-1")
	require.NoError(t, err)
	require.Equal(t, ip1, ip1Again)

	// Second machine gets the remaining IP in the 2-IP pool
	ip2, err := alloc.Allocate("192.168.10.100-192.168.10.101/24", "machine-2")
	require.NoError(t, err)
	require.NotEqual(t, ip1, ip2)

	// Third machine fails because the pool is exhausted
	_, err = alloc.Allocate("192.168.10.100-192.168.10.101/24", "machine-3")
	require.ErrorContains(t, err, "exhausted")

	// Releasing machine-1 frees its IP for machine-3
	alloc.Release("machine-1")
	ip3, err := alloc.Allocate("192.168.10.100-192.168.10.101/24", "machine-3")
	require.NoError(t, err)
	require.Equal(t, ip1, ip3)
}

func TestBuildCloudInitNetworkConfig(t *testing.T) {
	t.Run("default DHCP returns version 1", func(t *testing.T) {
		data := provider.Data{}
		cfg, err := provider.BuildCloudInitNetworkConfig(data, data.EffectiveNICs(), nil, nil)
		require.NoError(t, err)
		require.Equal(t, "version: 1", cfg)
	})

	t.Run("static IP, pool IP, gateway, DNS and routes produce version 2 netplan config", func(t *testing.T) {
		data := provider.Data{
			DNSServers:       []string{"1.1.1.1", "8.8.8.8"},
			DNSSearchDomains: []string{"k8s.local"},
			NICs: []provider.NIC{
				{
					Bridge:   "vmbr0",
					Gateway4: "192.168.1.1",
					MTU:      1500,
					Routes: []provider.Route{
						{To: "10.96.0.0/12", Via: "192.168.1.254", Metric: 100},
					},
				},
				{
					Bridge:    "vmbr1",
					Addresses: []string{"10.0.20.15/24"},
					MTU:       9000,
				},
			},
		}

		macs := provider.ExtractVMMACAddresses(map[string]string{
			"net0": "virtio=BC:24:11:AA:BB:01,bridge=vmbr0,firewall=1",
			"net1": "virtio=BC:24:11:AA:BB:02,bridge=vmbr1,firewall=0,mtu=9000",
		}, 2)

		allocated := map[int]string{
			0: "192.168.1.105/24",
		}

		cfg, err := provider.BuildCloudInitNetworkConfig(data, data.EffectiveNICs(), macs, allocated)
		require.NoError(t, err)
		require.Contains(t, cfg, "version: 2")
		require.Contains(t, cfg, "macaddress: bc:24:11:aa:bb:01")
		require.Contains(t, cfg, "192.168.1.105/24")
		require.Contains(t, cfg, "gateway4: 192.168.1.1")
		require.Contains(t, cfg, "10.0.20.15/24")
		require.Contains(t, cfg, "mtu: 9000")
		require.Contains(t, cfg, "1.1.1.1")
		require.Contains(t, cfg, "to: 10.96.0.0/12")
	})
}

func TestIPPoolAllocatorSubnetsAndSingleHost(t *testing.T) {
	alloc := provider.NewIPPoolAllocator()

	// Single host /32
	ip32, err := alloc.Allocate("10.10.0.55/32", "vm-a")
	require.NoError(t, err)
	require.Equal(t, "10.10.0.55/32", ip32)

	// Point-to-point /31
	ip31a, err := alloc.Allocate("10.10.0.0/31", "vm-1")
	require.NoError(t, err)
	ip31b, err := alloc.Allocate("10.10.0.0/31", "vm-2")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"10.10.0.0/31", "10.10.0.1/31"}, []string{ip31a, ip31b})

	// Large IPv6 /64 subnet must complete immediately without unbounded iteration
	ip6, err := alloc.Allocate("fd00:1234::/64", "vm-v6")
	require.NoError(t, err)
	require.True(t, len(ip6) > 0)
}

func TestEffectiveDisksAdditionalDiskStorageSelectorNotOverwritten(t *testing.T) {
	data := provider.Data{
		Storage:  "local-lvm",
		DiskSize: 30,
		AdditionalDisks: []provider.AdditionalDisk{
			{
				DiskSize:        100,
				StorageSelector: `name == "ceph-rbd"`,
			},
		},
	}

	disks := data.EffectiveDisks()
	require.Len(t, disks, 2)
	require.Equal(t, "local-lvm", disks[0].Storage)
	require.Empty(t, disks[1].Storage, "additional disk with explicit storage_selector must not inherit primary storage")
	require.Equal(t, `name == "ceph-rbd"`, disks[1].StorageSelector)
}

func TestAllocateVMIDRangeAndConcurrency(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}}))
	})
	mux.HandleFunc("/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"vmid": 5000, "type": "qemu", "node": "pve1"},
			},
		}))
	})
	mux.HandleFunc("/cluster/nextid", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		vmid := r.URL.Query().Get("vmid")
		if vmid == "" {
			vmid = "100"
		}

		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": vmid}))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	px := proxmox.NewClient(srv.URL)
	cluster, err := px.Cluster(context.Background())
	require.NoError(t, err)

	prov := provider.NewProvisioner(px, provider.WithDefaultVMIDRange(5000, 5002))

	// 5000 is already used in /cluster/resources, so first allocation must pick 5001
	id1, err := prov.AllocateVMID(context.Background(), cluster, provider.Data{})
	require.NoError(t, err)
	require.Equal(t, 5001, id1)

	// 5001 is now reserved in pendingVMIDs, so second concurrent allocation must pick 5002
	id2, err := prov.AllocateVMID(context.Background(), cluster, provider.Data{})
	require.NoError(t, err)
	require.Equal(t, 5002, id2)

	// Range [5000, 5002] is now full
	_, err = prov.AllocateVMID(context.Background(), cluster, provider.Data{})
	require.ErrorContains(t, err, "no free VMID available in configured range [5000, 5002]")
}

func TestDiscoverClusterOptionsAgainstMockProxmox(t *testing.T) {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}

	mux.HandleFunc("/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"type": "node", "node": "pve1", "status": "online"},
			{"type": "node", "node": "pve2", "status": "online"},
			{"type": "pool", "pool": "talos-prod"},
		})
	})
	mux.HandleFunc("/storage", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"storage": "ceph-rbd", "content": "images,rootdir"},
			{"storage": "cephfs-iso", "content": "iso,backup"},
		})
	})
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"node": "pve1", "status": "online"},
		})
	})
	mux.HandleFunc("/nodes/pve1/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("/nodes/pve1/storage", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"storage": "local-lvm", "content": "images", "active": 1, "enabled": 1},
			{"storage": "disabled-store", "content": "images", "enabled": 0},
		})
	})
	mux.HandleFunc("/nodes/pve1/network", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"iface": "vmbr0", "type": "bridge"},
			{"iface": "eno1", "type": "eth"},
		})
	})
	mux.HandleFunc("/pools", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"poolid": "talos-prod"},
		})
	})
	mux.HandleFunc("/cluster/sdn/vnets", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"vnet": "vnet-k8s"},
		})
	})
	mux.HandleFunc("/cluster/mapping/pci", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"id": "gpu-rtx4090", "map": []string{"node=pve1,path=0000:01:00.0,id=10de:2684"}},
		})
	})
	mux.HandleFunc("/cluster/mapping/usb", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"id": "ups-usb", "map": []string{"node=pve1,id=051d:0002"}},
		})
	})
	mux.HandleFunc("/cluster/qemu/custom-cpu-models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"cputype": "epyc-zen4"},
		})
	})
	mux.HandleFunc("/cluster/ha/rules", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"rule": "anti-affinity-cp"},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	opts := provider.DiscoverClusterOptions(context.Background(), proxmox.NewClient(srv.URL), zap.NewNop(), map[string][]string{
		"rack-a": {"pve1"},
	})
	require.Equal(t, []string{"pve1"}, opts.Nodes)
	require.Equal(t, []string{"local-lvm"}, opts.DiskStorages)
	require.Empty(t, opts.ISOStorages)
	require.Equal(t, []string{"vmbr0", "vnet-k8s"}, opts.NetworkBridges)
	require.Equal(t, []string{"talos-prod"}, opts.Pools)
	require.Equal(t, []string{"gpu-rtx4090"}, opts.PCIMappings)
	require.Equal(t, []string{"ups-usb"}, opts.USBMappings)
	require.Equal(t, []string{"anti-affinity-cp"}, opts.HARules)
	require.Contains(t, opts.CPUTypes, "custom-epyc-zen4")
	require.Equal(t, []string{"pve1"}, opts.NodeGroups["rack-a"])
}

func TestGroupedProxmoxSchemaAndDataNormalization(t *testing.T) {
	baseSchema := `{
  "type": "object",
  "properties": {
    "general": {"type": "object", "properties": {"node_group": {"type": "string"}, "node": {"type": "string"}, "pool": {"type": "string"}}},
    "os": {"type": "object", "properties": {"iso_storage": {"type": "string"}, "template_vmid": {"type": "string"}}},
    "compute": {"type": "object", "properties": {"cpu_type": {"type": "string"}}},
    "disks": {"type": "array", "items": {"type": "object", "properties": {"storage": {"type": "string"}}}},
    "network": {"type": "object", "properties": {"nics": {"type": "array", "items": {"type": "object", "properties": {"bridge": {"type": "string"}}}}}},
    "ha": {"type": "object", "properties": {
      "group": {"type": "string"},
      "node_affinity_nodes": {"type": "array", "items": {"type": "string"}},
      "rules": {"type": "array", "items": {"type": "string"}}
    }},
    "replication": {"type": "object", "properties": {"target_node": {"type": "string"}}},
    "firewall": {"type": "object", "properties": {"security_groups": {"type": "array", "items": {"type": "string"}}}},
    "passthrough": {"type": "object", "properties": {
      "pci_devices": {"type": "array", "items": {"type": "object", "properties": {"mapping": {"type": "string"}}}},
      "usb_devices": {"type": "array", "items": {"type": "object", "properties": {"mapping": {"type": "string"}}}}
    }}
  }
}`

	opts := provider.ClusterOptions{
		Nodes:          []string{"pve1", "pve2", "pve3"},
		NodeGroups:     map[string][]string{"gpu-rack": {"pve3"}, "std-rack": {"pve1", "pve2"}},
		DiskStorages:   []string{"ceph-rbd", "local-lvm"},
		ISOStorages:    []string{"local"},
		NetworkBridges: []string{"vmbr0", "vmbr-rdma"},
		PCIMappings:    []string{"nvidia-h100"},
		USBMappings:    []string{"dongle-1"},
		SecurityGroups: []string{"sg-k8s-nodes"},
		Templates:      []string{"9000 (talos-1.9)"},
		HAGroups:       []string{"ha-prod"},
	}

	dynamicJSON, err := provider.BuildDynamicSchema(baseSchema, opts)
	require.NoError(t, err)
	require.Contains(t, dynamicJSON, `"gpu-rack"`)
	require.Contains(t, dynamicJSON, `"ceph-rbd"`)
	require.Contains(t, dynamicJSON, `"vmbr-rdma"`)
	require.Contains(t, dynamicJSON, `"sg-k8s-nodes"`)
	require.Contains(t, dynamicJSON, `"9000 (talos-1.9)"`)
	require.Contains(t, dynamicJSON, `"ha-prod"`)

	// Verify Data.Normalize() flattens grouped sections and strips "auto"/"none" sentinels
	data := provider.Data{
		General: &provider.GeneralSection{
			NodeGroup: "auto",
			Node:      "pve2",
			Pool:      "auto",
		},
		OS: &provider.OSSection{
			ProvisioningMode: "iso",
			ISOStorage:       "local",
			TemplateVMID:     "none",
		},
		Compute: &provider.ComputeSection{
			Cores:  8,
			Memory: 16384,
		},
		Disks: []provider.Disk{
			{Storage: "auto", Size: 80},
		},
		Network: &provider.NetworkSection{
			NICs: []provider.NIC{{Bridge: "vmbr0"}},
		},
	}

	data.Normalize()
	require.Empty(t, data.NodeGroup)
	require.Equal(t, "pve2", data.Node)
	require.Empty(t, data.Pool)
	require.Equal(t, "local", data.ISOStorage)
	require.Empty(t, data.TemplateVMID)
	require.Equal(t, 8, data.Cores)
	require.Equal(t, uint64(16384), data.Memory)
	require.Empty(t, data.Disks[0].Storage)
	require.Len(t, data.NICs, 1)
}

func TestFilterNodesByGroupAndTopology(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/ha/groups", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"group": "ha-zone-b", "nodes": "pve2:2,pve3:1"},
			},
		}))
	})
	mux.HandleFunc("/cluster/mapping/pci", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "nvidia-gpu", "map": []string{"node=pve3,path=0000:01:00.0,id=10de:2684"}},
			},
		}))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	prov := provider.NewProvisioner(proxmox.NewClient(srv.URL))

	allNodes := proxmox.NodeStatuses{
		{Node: "pve1", Status: "online"},
		{Node: "pve2", Status: "online"},
		{Node: "pve3", Status: "online"},
	}

	// Filtering by HA group "ha-zone-b" keeps pve2 and pve3, and filtering by PCI mapping "nvidia-gpu" narrows to pve3
	filtered, err := prov.FilterNodesByGroupAndTopology(context.Background(), allNodes, provider.Data{
		NodeGroup:  "ha-zone-b",
		PCIDevices: []provider.PCIDevice{{Mapping: "nvidia-gpu"}},
	})
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	require.Equal(t, "pve3", filtered[0].Node)
}

func TestReconcileExistingMachineUpdatesDisksNICsAndMigratesNodeGroup(t *testing.T) {
	provider.SetTaskPollInterval(t, time.Millisecond)

	var (
		migratedTo   string
		resizedDisk  string
		resizedSize  string
		unlinkedDisk string
		configCalls  []map[string]any
		addedFWRules []map[string]any
	)

	const taskUPID = "UPID:pve2:00000001:00000001:00000001:qmconfig:100:root@pam:"

	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}

	mux.HandleFunc("/nodes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"node": "pve1", "status": "online"},
			{"node": "pve2", "status": "online"},
		})
	})
	mux.HandleFunc("/nodes/pve1/status", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{}) })
	mux.HandleFunc("/nodes/pve2/status", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{}) })

	vmCurrent := map[string]any{"vmid": 100, "status": "running", "name": "worker-1"}
	vmConfig := map[string]any{
		"scsi0": "local-lvm:vm-100-disk-0,size=20G,ssd=1",
		"scsi2": "local-lvm:vm-100-disk-2,size=50G",
		"net0":  "virtio=BC:24:11:AA:BB:01,bridge=vmbr0,firewall=1",
		"net2":  "virtio=BC:24:11:AA:BB:03,bridge=vmbr2,firewall=0",
	}

	mux.HandleFunc("/nodes/pve1/qemu/100/status/current", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, vmCurrent) })
	mux.HandleFunc("/nodes/pve1/qemu/100/config", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, vmConfig) })
	mux.HandleFunc("/nodes/pve1/qemu/100/migrate", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		migratedTo = fmt.Sprint(body["target"])

		writeJSON(w, taskUPID)
	})

	mux.HandleFunc("/nodes/pve2/qemu/100/status/current", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, vmCurrent) })
	mux.HandleFunc("/nodes/pve2/qemu/100/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, vmConfig)

			return
		}

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		configCalls = append(configCalls, body)

		writeJSON(w, taskUPID)
	})
	mux.HandleFunc("/nodes/pve2/qemu/100/resize", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		resizedDisk = body["disk"]
		resizedSize = body["size"]

		writeJSON(w, taskUPID)
	})
	mux.HandleFunc("/nodes/pve2/qemu/100/unlink", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		unlinkedDisk = fmt.Sprint(body["idlist"])

		writeJSON(w, taskUPID)
	})
	mux.HandleFunc("/nodes/pve2/storage", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"storage": "local-lvm", "content": "images", "active": 1, "enabled": 1},
			{"storage": "ceph-rbd", "content": "images", "active": 1, "enabled": 1},
		})
	})
	mux.HandleFunc("/nodes/pve2/qemu/100/firewall/rules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, []map[string]any{})

			return
		}

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		addedFWRules = append(addedFWRules, body)

		writeJSON(w, nil)
	})
	mux.HandleFunc("/nodes/pve2/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"upid": taskUPID, "node": "pve2", "status": "stopped", "exitstatus": "OK"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	prov := provider.NewProvisioner(
		proxmox.NewClient(srv.URL),
		provider.WithCustomNodeGroups(map[string][]string{
			"rack-b": {"pve2"},
		}),
	)

	machine := resources.NewMachine("default", "worker-1")
	machine.TypedSpec().Value.Node = "pve1"
	machine.TypedSpec().Value.Vmid = 100

	req := infra.NewMachineRequest("worker-1")
	req.TypedSpec().Value.ProviderData = `
node_group: rack-b
cores: 8
memory: 16384
security_groups:
  - sg-k8s
disks:
  - storage: local-lvm
    size: 80
    ssd: true
    discard: true
  - storage: ceph-rbd
    size: 200
    ssd: true
nics:
  - bridge: vmbr0
    vlan: 100
    mtu: 9000
  - bridge: vmbr1
    vlan: 200
`

	require.NoError(t, prov.ReconcileExistingMachine(context.Background(), zap.NewNop(), machine, req))

	// 1. VM was live-migrated from pve1 to pve2 to match updated node_group: rack-b
	require.Equal(t, "pve2", migratedTo)
	require.Equal(t, "pve2", machine.TypedSpec().Value.Node)

	// 2. Primary disk scsi0 was resized from 20G to 80G, new scsi1 attached, old scsi2 detached
	require.Equal(t, "scsi0", resizedDisk)
	require.Equal(t, "80G", resizedSize)
	require.Equal(t, "scsi2", unlinkedDisk)

	// 3. NIC0 preserved its MAC address BC:24:11:AA:BB:01 while updating VLAN=100 & MTU=9000, NIC1 added, NIC2 deleted
	require.Len(t, configCalls, 2)
	vmCfgUpdate := configCalls[1]
	require.Contains(t, vmCfgUpdate["net0"], "BC:24:11:AA:BB:01")
	require.Contains(t, vmCfgUpdate["net0"], "tag=100")
	require.Contains(t, vmCfgUpdate["net0"], "mtu=9000")
	require.Contains(t, vmCfgUpdate["net1"], "bridge=vmbr1")
	require.Equal(t, "net2", vmCfgUpdate["delete"])

	// 4. Security group sg-k8s attached
	require.Len(t, addedFWRules, 1)
	require.Equal(t, "sg-k8s", addedFWRules[0]["action"])
}

func TestWorkloadProfilesAndExtensions(t *testing.T) {
	gpuData := provider.Data{
		WorkloadProfile: "gpu-ai-worker",
		TalosExtensions: []string{"siderolabs/tailscale"},
	}
	gpuData.ApplyWorkloadProfile()

	require.Equal(t, "q35", gpuData.MachineType)
	require.Equal(t, "host", gpuData.CPUType)
	require.Equal(t, "ovmf", gpuData.Bios)
	require.True(t, gpuData.NUMA)
	require.NotNil(t, gpuData.Balloon)
	require.False(t, *gpuData.Balloon)
	require.Equal(t, []string{
		"siderolabs/qemu-guest-agent",
		"siderolabs/nonfree-kmod-nvidia-production",
		"siderolabs/nvidia-container-toolkit-production",
		"siderolabs/tailscale",
	}, gpuData.EffectiveExtensions())

	secData := provider.Data{WorkloadProfile: "hardened-secure"}
	secData.ApplyWorkloadProfile()
	require.Equal(t, "ovmf", secData.Bios)
	require.True(t, secData.SecureBoot)
	require.True(t, secData.TPM)
	require.True(t, secData.Protection)

	cpData := provider.Data{WorkloadProfile: "control-plane"}
	cpData.ApplyWorkloadProfile()
	require.Equal(t, "order=1,up=30", cpData.StartupOrder)
	require.Equal(t, "i6300esb", cpData.WatchdogModel)
	require.Equal(t, "reset", cpData.WatchdogAction)
	require.True(t, cpData.Protection)
	require.NotNil(t, cpData.HA)
	require.Equal(t, "negative", cpData.HA.ResourceAffinity)

	stData := provider.Data{
		WorkloadProfile: "storage-node",
		Disks: []provider.Disk{
			{Size: 40, Storage: "local-lvm"},
			{Size: 500, Storage: "nvme-pool", VolumeName: "ceph-osd"},
		},
	}
	stDisks := stData.EffectiveDisks()
	require.Len(t, stDisks, 2)
	require.Equal(t, "none", stDisks[1].EffectiveCache())
	require.Equal(t, "io_uring", stDisks[1].EffectiveAIO())
	require.True(t, stDisks[1].SSD)
	require.True(t, stDisks[1].Discard)
	require.True(t, stDisks[1].IOThread)
}

func TestBuildCloudInitNetworkConfigWithBondLACP(t *testing.T) {
	data := provider.Data{
		NICs: []provider.NIC{
			{Bridge: "vmbr0"},
			{Bridge: "vmbr1"},
		},
		Bonds: []provider.NetBond{
			{
				Name:       "bond0",
				Interfaces: []string{"net0", "net1"},
				Mode:       "802.3ad",
				MTU:        9000,
				Addresses:  []string{"10.10.20.50/24"},
				Gateway4:   "10.10.20.1",
			},
		},
	}

	cfg, err := provider.BuildCloudInitNetworkConfig(data, data.EffectiveNICs(), nil, nil)
	require.NoError(t, err)
	require.Contains(t, cfg, "bonds:")
	require.Contains(t, cfg, "bond0:")
	require.Contains(t, cfg, "mode: 802.3ad")
	require.Contains(t, cfg, "lacp-rate: fast")
	require.Contains(t, cfg, "transmit-hash-policy: layer2+3")
	require.Contains(t, cfg, "10.10.20.50/24")
	require.Contains(t, cfg, "- eth0")
	require.Contains(t, cfg, "- eth1")
}

func TestStorageQoSDiskOptions(t *testing.T) {
	disks := []provider.Disk{
		{
			Size:       100,
			SSD:        true,
			IOPSRead:   5000,
			IOPSWrite:  2500,
			MBPSRead:   500,
			MBPSWrite:  250,
			VolumeName: "local-data",
		},
	}

	opts := provider.BuildDiskOptions(disks, []string{"ceph-rbd"})
	require.Equal(t, map[string]any{
		"scsi0": "ceph-rbd:100,ssd=1,iops_rd=5000,iops_wr=2500,mbps_rd=500,mbps_wr=250,serial=vol-local-data",
	}, opts)
}
