// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/config"
)

func TestValidateConnectionSettings(t *testing.T) {
	valid := config.Proxmox{URL: "https://pve.example:8006/api2/json", TokenID: "provider@pve!omni", TokenSecret: "example"}
	require.NoError(t, (config.Config{Proxmox: valid}).Validate())

	for _, change := range []func(*config.Proxmox){
		func(p *config.Proxmox) { p.URL = "pve.example:8006" },
		func(p *config.Proxmox) { p.URL = "https://user:pass@pve.example" },
		func(p *config.Proxmox) { p.TokenSecret = "" },
		func(p *config.Proxmox) { p.Username = "root"; p.Password = "example" },
		func(p *config.Proxmox) { p.MinVMID = 99 },
		func(p *config.Proxmox) { p.MinVMID = 500; p.MaxVMID = 400 },
	} {
		p := valid
		change(&p)
		require.Error(t, (config.Config{Proxmox: p}).Validate())
	}
}
