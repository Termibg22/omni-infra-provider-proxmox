// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package config describes the connection settings for Proxmox infra provider.
package config

import (
	"fmt"
	"net/url"
)

// Config describes Proxmox provider configuration.
type Config struct {
	Proxmox Proxmox `yaml:"proxmox"`
}

// Proxmox is the config for accessing Proxmox API.
type Proxmox struct {
	// NodeGroups allows defining named logical groups of Proxmox nodes (e.g. "rack-1": ["pve1", "pve2"])
	// in addition to groups automatically discovered from Proxmox HA Groups, HA Rules, and SDN Zones.
	NodeGroups map[string][]string `yaml:"nodeGroups,omitempty"`

	URL string `yaml:"url"`

	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Realm    string `yaml:"realm,omitempty"`

	TokenID     string `yaml:"tokenID,omitempty"`
	TokenSecret string `yaml:"tokenSecret,omitempty"`

	// Optional default VMID allocation range for VMs created by this provider.
	// Can also be overridden per MachineClass in Omni.
	MinVMID int `yaml:"minVMID,omitempty"`
	MaxVMID int `yaml:"maxVMID,omitempty"`

	InsecureSkipVerify bool `yaml:"insecureSkipVerify,omitempty"`
}

// Validate rejects incomplete connection settings before starting any controllers.
func (c Config) Validate() error {
	p := c.Proxmox

	u, err := url.Parse(p.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("proxmox.url must be an absolute HTTP(S) API URL without credentials, query, or fragment")
	}

	if err := p.validateAuthentication(); err != nil {
		return err
	}

	return ValidateVMIDRange(p.MinVMID, p.MaxVMID)
}

func (p Proxmox) validateAuthentication() error {
	passwordAuth := p.Username != "" || p.Password != ""

	tokenAuth := p.TokenID != "" || p.TokenSecret != ""
	if passwordAuth == tokenAuth {
		return fmt.Errorf("configure exactly one Proxmox authentication method: username/password or tokenID/tokenSecret")
	}

	if passwordAuth && (p.Username == "" || p.Password == "") {
		return fmt.Errorf("proxmox.username and proxmox.password must both be set")
	}

	if tokenAuth && (p.TokenID == "" || p.TokenSecret == "") {
		return fmt.Errorf("proxmox.tokenID and proxmox.tokenSecret must both be set")
	}

	return nil
}

// ValidateVMIDRange checks provider defaults and per-machine overrides alike.
func ValidateVMIDRange(minID, maxID int) error {
	for _, bound := range []int{minID, maxID} {
		if bound != 0 && (bound < 100 || bound > 999999999) {
			return fmt.Errorf("VMID bounds must be between 100 and 999999999 (0 means unset)")
		}
	}

	if maxID != 0 && minID > maxID {
		return fmt.Errorf("minimum VMID cannot exceed maximum VMID")
	}

	return nil
}
