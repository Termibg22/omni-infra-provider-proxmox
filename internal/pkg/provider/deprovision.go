// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

var errVMAbsent = errors.New("VM does not exist in the cluster")

func pendingCloneName(machineUUID string) string {
	return "omni-" + machineUUID
}

func verifyVMIdentity(vm *proxmox.VirtualMachine, machine *resources.Machine, pendingClone bool) error {
	spec := machine.TypedSpec().Value
	if vm == nil || int64(vm.VMID) != int64(spec.Vmid) || vm.VirtualMachineConfig == nil || bool(vm.VirtualMachineConfig.Template) || spec.Uuid == "" {
		return fmt.Errorf("cannot verify VM %d identity for request %q", spec.Vmid, machine.Metadata().ID())
	}

	for field := range strings.SplitSeq(vm.VirtualMachineConfig.SMBios1, ",") {
		if value, ok := strings.CutPrefix(field, "uuid="); ok && strings.EqualFold(value, spec.Uuid) {
			return nil
		}
	}

	taskFields := strings.Split(spec.VmCreateTask, ":")
	if pendingClone && spec.VmStartTask == "" && spec.StartAttempts == 0 && len(taskFields) > 7 && taskFields[0] == "UPID" && taskFields[5] == "qmclone" &&
		vm.VirtualMachineConfig.Name == pendingCloneName(spec.Uuid) {
		return nil
	}

	return fmt.Errorf("VM %d on node %q does not match the recorded identity for request %q; refusing to modify it", spec.Vmid, vm.Node, machine.Metadata().ID())
}

func (p *Provisioner) findVMForTeardown(ctx context.Context, machine *resources.Machine) (*proxmox.VirtualMachine, error) {
	spec := machine.TypedSpec().Value
	if spec.Node != "" {
		vm, err := p.getVM(ctx, spec.Node, spec.Vmid)
		if err == nil {
			return vm, nil
		}

		if !spec.HaRegistered && !isAlreadyGoneErr(err) {
			return nil, err
		}
	}

	node, found, err := p.ha.FindVMNode(ctx, spec.Vmid)
	if err != nil {
		return nil, err
	}

	if found {
		return p.getVM(ctx, node, spec.Vmid)
	}

	if spec.VmStartTask == "" && spec.VmCreateTask != "" {
		if taskErr := p.checkTaskStatus(ctx, spec.VmCreateTask); taskErr != nil && !isTaskFailed(taskErr) {
			return nil, fmt.Errorf("cannot confirm VM %d creation has finished: %w", spec.Vmid, taskErr)
		}
	}

	free, err := p.checkVMIDFree(ctx, int(spec.Vmid))
	if err != nil {
		return nil, fmt.Errorf("cannot confirm absence of VM %d: %w", spec.Vmid, err)
	}

	if !free {
		return nil, fmt.Errorf("cannot confirm absence of VM %d: VMID is still allocated in the cluster", spec.Vmid)
	}

	return nil, errVMAbsent
}

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
