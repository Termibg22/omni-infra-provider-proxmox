// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"

	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

func (p *Provisioner) registerHAStep(ctx context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	var data Data
	if err := pctx.UnmarshalProviderData(&data); err != nil {
		return err
	}

	data.ApplyWorkloadProfile()

	if data.HA == nil {
		return nil
	}

	if pctx.State.TypedSpec().Value.Vmid == 0 {
		return fmt.Errorf("vmid not assigned before HA registration")
	}

	if _, err := p.locateExistingVM(ctx, pctx.State); err != nil {
		return err
	}

	if err := p.ha.Register(ctx, fmt.Sprintf("vm:%d", pctx.State.TypedSpec().Value.Vmid), *data.HA); err != nil {
		return err
	}

	pctx.State.TypedSpec().Value.HaRegistered = true

	return nil
}

func (p *Provisioner) syncHARulesStep(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	var data Data
	if err := pctx.UnmarshalProviderData(&data); err != nil {
		return err
	}

	data.ApplyWorkloadProfile()

	if data.HA == nil {
		return nil
	}

	cfg := *data.HA

	if _, err := p.locateExistingVM(ctx, pctx.State); err != nil {
		return err
	}

	if data.NodeGroup != "" && len(cfg.NodeAffinityNodes) == 0 {
		nodes, err := p.resolveNodeGroupNodes(ctx, data.NodeGroup)
		if err != nil {
			return err
		}

		cfg.NodeAffinityNodes = nodes
	}

	set, _ := pctx.GetMachineRequestSetID()

	return p.ha.SyncRules(ctx, logger, set, fmt.Sprintf("vm:%d", pctx.State.TypedSpec().Value.Vmid), cfg)
}
