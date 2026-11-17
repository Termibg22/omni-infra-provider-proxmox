// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"
)

const editorHeartbeatAnnotation = "proxmox.omni.siderolabs.io/editor-heartbeat"

// StartEditorKeepalive emits provider status events to keep otherwise idle
// browser watch connections alive. Stock Omni watches ProviderStatus in the
// class editor, but not ProviderHealthStatus. A class watch reconnect replaces
// unsaved form data, while a provider metadata update preserves it.
//
// Only an annotation changes: schema, inventory and MachineClasses are untouched.
// The returned function cancels the worker and waits for it to stop.
func StartEditorKeepalive(ctx context.Context, st state.State, logger *zap.Logger, providerID string, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				updateCtx, cancelUpdate := context.WithTimeout(ctx, 10*time.Second)
				err := publishEditorHeartbeat(updateCtx, st, providerID)

				cancelUpdate()

				if err != nil && ctx.Err() == nil {
					logger.Warn("failed to publish editor keepalive", zap.Error(err))
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func publishEditorHeartbeat(ctx context.Context, st state.State, providerID string) error {
	_, err := safe.StateUpdateWithConflicts(ctx, st, infra.NewProviderStatus(providerID).Metadata(), func(status *infra.ProviderStatus) error {
		status.Metadata().Annotations().Set(editorHeartbeatAnnotation, time.Now().UTC().Format(time.RFC3339Nano))

		return nil
	})
	// The SDK registers the status during startup. Never create an incomplete
	// status while it is starting, or resurrect one being torn down.
	if state.IsNotFoundError(err) || state.IsPhaseConflictError(err) {
		return nil
	}

	return err
}
