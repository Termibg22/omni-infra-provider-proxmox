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

	"go.yaml.in/yaml/v4"
)

// Reject changes which cannot be applied to a running Talos guest instead of
// recording a successful hash after silently ignoring them.
func validateLiveUpdate(previous string, desired Data) error {
	var old Data
	if err := yaml.Unmarshal([]byte(previous), &old); err != nil {
		return fmt.Errorf("invalid applied provider data: %w", err)
	}

	old.ApplyWorkloadProfile()

	if old.HA != nil && desired.HA != nil {
		if old.HA.ResourceAffinity != desired.HA.ResourceAffinity || !slices.Equal(old.HA.Rules, desired.HA.Rules) {
			return fmt.Errorf("changing HA rule memberships or resource affinity requires reprovisioning")
		}

		if len(old.HA.NodeAffinityNodes) > 0 && len(desired.HA.NodeAffinityNodes) == 0 {
			return fmt.Errorf("removing HA node affinity requires reprovisioning")
		}
	}

	before, after := creationOnlyFields(old), creationOnlyFields(desired)
	for _, field := range slices.Sorted(maps.Keys(before)) {
		if !reflect.DeepEqual(before[field], after[field]) {
			return fmt.Errorf("changing %s on an existing VM requires reprovisioning", field)
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
