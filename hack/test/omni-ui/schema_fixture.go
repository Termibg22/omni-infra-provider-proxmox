//go:build ignore

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Export a synthetic cluster schema for the stock Omni renderer tests.
// Run from the provider repository root; no network access or credentials.
package main

import (
	"fmt"
	"os"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
)

func main() {
	raw, err := os.ReadFile("cmd/omni-infra-provider-proxmox/data/schema.json")
	if err != nil {
		panic(err)
	}

	schema, err := provider.BuildDynamicSchema(string(raw), provider.ClusterOptions{
		Nodes:          []string{"pve1", "pve2", "pve3"},
		NodeGroups:     map[string][]string{"rack-a": {"pve1", "pve2"}, "rack-b": {"pve3"}},
		DiskStorages:   []string{"local-zfs"},
		ISOStorages:    []string{"local"},
		Templates:      []string{"9000 (talos-template)"},
		NetworkBridges: []string{"vmbr0"},
		CPUTypes:       provider.DefaultCPUTypes,
		PCIMappings:    []string{"gpu-one", "gpu-two", "pve1/0000:01:00.0 (NVIDIA GeForce RTX 4090)", "pve2/0000:01:00.0 (AMD Radeon RX 7900)"},
		USBMappings:    []string{"usb-one", "usb-three", "pve1/1-2.3 (Silicon Labs CP210x)", "pve3/2-1 (APC UPS)"},
		NodeTopologies: map[string]provider.NodeTopology{
			"pve1": {PCIMappings: []string{"gpu-one", "pve1/0000:01:00.0 (NVIDIA GeForce RTX 4090)"}, USBMappings: []string{"usb-one", "pve1/1-2.3 (Silicon Labs CP210x)"}},
			"pve2": {PCIMappings: []string{"gpu-two", "pve2/0000:01:00.0 (AMD Radeon RX 7900)"}},
			"pve3": {USBMappings: []string{"usb-three", "pve3/2-1 (APC UPS)"}},
		},
	})
	if err != nil {
		panic(err)
	}

	fmt.Print(schema)
}
