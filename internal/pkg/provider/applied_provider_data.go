// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"strings"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

// Older providers stored raw YAML here. Keep reading it for live-update safety,
// but never write it: Omni rejects control characters and caps annotation sizes.
const appliedProviderDataAnnotation = "proxmox.omni.siderolabs.io/applied-provider-data"

func recordAppliedProviderData(machine *resources.Machine, raw string) {
	if strings.TrimSpace(raw) == "" {
		// Distinguish an applied empty configuration from a missing baseline.
		raw = "null\n"
	}

	machine.TypedSpec().Value.AppliedProviderData = raw
	machine.Metadata().Annotations().Set(providerDataHashAnnotation, hashProviderData(raw))
	machine.Metadata().Annotations().Delete(appliedProviderDataAnnotation)
}

func appliedProviderData(machine *resources.Machine) (string, bool) {
	if raw := machine.TypedSpec().Value.AppliedProviderData; raw != "" {
		return raw, true
	}

	return machine.Metadata().Annotations().Get(appliedProviderDataAnnotation)
}
