// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

const ipAllocationsAnnotation = "proxmox.omni.siderolabs.io/ip-allocations"

type ipAllocation struct {
	Pool    string `json:"pool"`
	Address string `json:"address"`
}

// WithMachineState supplies durable Omni state for IP pool reservations.
func WithMachineState(st state.State) ProvisionerOption {
	return func(p *Provisioner) { p.machineState = st }
}

func readIPAllocations(machine *resources.Machine) (map[string]ipAllocation, error) {
	allocations := map[string]ipAllocation{}
	if raw, ok := machine.Metadata().Annotations().Get(ipAllocationsAnnotation); ok {
		if err := json.Unmarshal([]byte(raw), &allocations); err != nil {
			return nil, fmt.Errorf("invalid persisted IP allocations for %q: %w", machine.Metadata().ID(), err)
		}

		if allocations == nil {
			return nil, fmt.Errorf("invalid persisted IP allocations for %q: expected an object, got null", machine.Metadata().ID())
		}
	}

	return allocations, nil
}

// Allocate in a separate provision step so reservations are committed to Omni
// before Cloud-Init is uploaded or the VM boots. Restore actual reservations,
// rather than guessing addresses from VM names or VMID ordering after a restart.
func (p *Provisioner) allocateMachineIPs(ctx context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	var data Data
	if err := pctx.UnmarshalProviderData(&data); err != nil {
		return err
	}

	data.ApplyWorkloadProfile()

	pools := map[string]string{}

	for i, nic := range data.EffectiveNICs() {
		if nic.IPPool != "" {
			pools[fmt.Sprintf("nic%d", i)] = nic.IPPool
		}
	}

	for i, bond := range data.Bonds {
		if bond.IPPool != "" {
			pools[fmt.Sprintf("bond%d", i)] = bond.IPPool
		}
	}

	if len(pools) == 0 {
		return nil
	}

	if p.machineState == nil {
		return fmt.Errorf("ip_pool requires durable Omni machine state")
	}

	p.ipAllocationMu.Lock()
	defer p.ipAllocationMu.Unlock()

	if err := p.restoreIPAllocations(ctx, pctx.GetRequestID()); err != nil {
		return err
	}

	allocations, err := readIPAllocations(pctx.State)
	if err != nil {
		return err
	}

	if err = p.ipAllocator.allocateInterfaces(pctx.GetRequestID(), pools, allocations); err != nil {
		return err
	}

	raw, err := json.Marshal(allocations)
	if err != nil {
		return err
	}

	pctx.State.Metadata().Annotations().Set(ipAllocationsAnnotation, string(raw))

	return nil
}

func (allocator *ipPoolAllocator) allocateInterfaces(requestID string, pools map[string]string, allocations map[string]ipAllocation) error {
	allocator.mu.Lock()
	defer allocator.mu.Unlock()

	staged := ipPoolAllocator{allocations: maps.Clone(allocator.allocations)}

	for _, pool := range pools {
		key := strings.TrimSpace(pool)
		if existing := allocator.allocations[key]; existing != nil {
			staged.allocations[key] = maps.Clone(existing)
		}
	}

	for _, iface := range slices.Sorted(maps.Keys(pools)) {
		pool := pools[iface]

		owner := requestID + "/" + iface
		if existing, ok := allocations[iface]; ok {
			if existing.Pool != pool {
				return fmt.Errorf("cannot change ip_pool of provisioned interface %s", iface)
			}

			if reserveErr := staged.Reserve(pool, owner, existing.Address); reserveErr != nil {
				return reserveErr
			}
		}

		addr, allocErr := staged.Allocate(pool, owner)
		if allocErr != nil {
			return fmt.Errorf("failed to allocate IP for %s: %w", iface, allocErr)
		}

		allocations[iface] = ipAllocation{Pool: pool, Address: addr}
	}

	allocator.allocations = staged.allocations

	return nil
}

func (p *Provisioner) restoreIPAllocations(ctx context.Context, currentRequest string) error {
	machines, err := safe.StateListAll[*resources.Machine](ctx, p.machineState)
	if err != nil {
		return fmt.Errorf("failed to restore IP allocations: %w", err)
	}

	known := map[string]bool{currentRequest: true}
	for machine := range machines.All() {
		known[machine.Metadata().ID()] = true
	}

	p.ipAllocator.pruneOwners(known)

	for machine := range machines.All() {
		saved, readErr := readIPAllocations(machine)
		if readErr != nil {
			return readErr
		}

		for iface, allocation := range saved {
			if reserveErr := p.ipAllocator.Reserve(allocation.Pool, machine.Metadata().ID()+"/"+iface, allocation.Address); reserveErr != nil {
				return reserveErr
			}
		}
	}

	return nil
}
