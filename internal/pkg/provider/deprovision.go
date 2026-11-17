// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"go.uber.org/zap"
)

// Clean up only the VM's attached, private NoCloud ISO, never the shared Talos
// installation ISO or an image belonging to another VM. Run before VM deletion
// so another provisioning request cannot reuse its VMID during ISO cleanup.
func (p *Provisioner) cleanupVMCloudInitISO(ctx context.Context, logger *zap.Logger, vm *proxmox.VirtualMachine) {
	if vm.VirtualMachineConfig == nil || !vm.HasTag(proxmox.MakeTag(proxmox.TagCloudInit)) {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	want := "iso/" + fmt.Sprintf(proxmox.UserDataISOFormat, vm.VMID)
	seen := map[string]bool{}

	for _, drive := range vm.VirtualMachineConfig.MergeDisks() {
		parts := strings.Split(drive, ",")

		storage, volume, ok := strings.Cut(parts[0], ":")
		if !ok || storage == "" || volume != want || !slices.Contains(parts[1:], "media=cdrom") || seen[parts[0]] {
			continue
		}

		seen[parts[0]] = true
		path := fmt.Sprintf("/nodes/%s/storage/%s/content/%s", vm.Node, storage, parts[0])

		var upid proxmox.UPID

		err := p.proxmoxClient.Delete(ctx, path, &upid)
		if err == nil && upid != "" {
			err = p.waitForTaskToFinish(ctx, proxmox.NewTask(upid, p.proxmoxClient))
		}

		if err == nil || isAlreadyGoneErr(err) {
			continue
		}

		if proxmox.IsNotAuthorized(err) {
			err = fmt.Errorf("requires Datastore.Allocate on /storage/%s to delete this ISO: %w", storage, err)
		}

		logger.Warn("NoCloud ISO cleanup failed; continuing VM and disk deletion",
			zap.Int("vmid", int(vm.VMID)), zap.String("node", vm.Node),
			zap.String("volume", parts[0]), zap.String("endpoint", "DELETE "+path), zap.Error(err),
		)
	}
}
