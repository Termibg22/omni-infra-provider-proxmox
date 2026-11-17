// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider //nolint:testpackage // Verify metadata-only writes and worker cancellation.

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func TestEditorKeepalivePublishesWithoutChangingSchemaOrClasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		st := state.WrapCore(inmem.NewState())
		status := infra.NewProviderStatus("proxmox")
		status.TypedSpec().Value.Schema = `{"type":"object","properties":{"cores":{"type":"integer"}}}`
		status.TypedSpec().Value.Version = "v4.1.6"
		status.Metadata().Annotations().Set("existing", "keep")
		require.NoError(t, st.Create(ctx, status))

		other := infra.NewProviderStatus("other")
		require.NoError(t, st.Create(ctx, other))

		class := omni.NewMachineClass("example")
		require.NoError(t, st.Create(ctx, class))

		events := make(chan state.Event, 10)
		require.NoError(t, st.Watch(ctx, status.Metadata(), events))
		require.Equal(t, state.Created, (<-events).Type)

		stop := StartEditorKeepalive(ctx, st, zap.NewNop(), "proxmox", 30*time.Second)
		defer stop()

		synctest.Wait()

		for range 3 {
			time.Sleep(30 * time.Second)
			synctest.Wait()
			require.Len(t, events, 1, "each interval must emit an observable status event")
			require.Equal(t, state.Updated, (<-events).Type)

			current, err := safe.StateGet[*infra.ProviderStatus](ctx, st, status.Metadata())
			require.NoError(t, err)
			require.True(t, proto.Equal(status.TypedSpec().Value, current.TypedSpec().Value))
			heartbeat, ok := current.Metadata().Annotations().Get(editorHeartbeatAnnotation)
			require.True(t, ok)
			require.Equal(t, time.Now().UTC().Format(time.RFC3339Nano), heartbeat)

			previous, ok := current.Metadata().Annotations().Get("existing")
			require.True(t, ok)
			require.Equal(t, "keep", previous)
		}

		for _, untouched := range []resource.Resource{class, other} {
			current, err := st.Get(ctx, untouched.Metadata())
			require.NoError(t, err)
			require.True(t, resource.Equal(untouched, current))
		}

		stop()
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Empty(t, events, "stopping the worker must stop all writes")
	})
}

func TestEditorKeepaliveWaitsForSDKRegistrationAndDoesNotResurrectStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		st := state.WrapCore(inmem.NewState())
		status := infra.NewProviderStatus("proxmox")

		stop := StartEditorKeepalive(ctx, st, zap.NewNop(), "proxmox", 30*time.Second)
		defer stop()

		synctest.Wait()
		time.Sleep(30 * time.Second)
		synctest.Wait()

		_, err := st.Get(ctx, status.Metadata())
		require.True(t, state.IsNotFoundError(err), "keepalive must not publish a status with an empty schema")

		status.TypedSpec().Value.Schema = `{"type":"object"}`
		require.NoError(t, st.Create(ctx, status))
		time.Sleep(30 * time.Second)
		synctest.Wait()

		current, err := safe.StateGet[*infra.ProviderStatus](ctx, st, status.Metadata())
		require.NoError(t, err)

		_, ok := current.Metadata().Annotations().Get(editorHeartbeatAnnotation)
		require.True(t, ok)

		_, err = st.Teardown(ctx, status.Metadata())
		require.NoError(t, err)
		before, err := st.Get(ctx, status.Metadata())
		require.NoError(t, err)
		time.Sleep(30 * time.Second)
		synctest.Wait()

		after, err := st.Get(ctx, status.Metadata())
		require.NoError(t, err)
		require.True(t, resource.Equal(before, after))
	})
}

func TestEditorKeepaliveCanBeDisabled(t *testing.T) {
	// Disabled means no state access, goroutine or timer, even with a nil client.
	stop := StartEditorKeepalive(t.Context(), nil, zap.NewNop(), "proxmox", 0)
	stop()
}

type stalledHeartbeatState struct {
	state.State
	stall bool
}

func (s *stalledHeartbeatState) UpdateWithConflicts(ctx context.Context, ptr resource.Pointer, update state.UpdaterFunc, options ...state.UpdateOption) (resource.Resource, error) {
	if s.stall {
		s.stall = false

		<-ctx.Done()

		return nil, ctx.Err()
	}

	return s.State.UpdateWithConflicts(ctx, ptr, update, options...)
}

func TestEditorKeepaliveRecoversAfterTimedOutUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		st := &stalledHeartbeatState{State: state.WrapCore(inmem.NewState()), stall: true}
		status := infra.NewProviderStatus("proxmox")
		require.NoError(t, st.Create(ctx, status))

		stop := StartEditorKeepalive(ctx, st, zap.NewNop(), "proxmox", 30*time.Second)
		defer stop()

		synctest.Wait()
		time.Sleep(40 * time.Second)
		synctest.Wait()

		current, err := st.Get(ctx, status.Metadata())
		require.NoError(t, err)
		require.True(t, resource.Equal(status, current))

		time.Sleep(20 * time.Second)
		synctest.Wait()

		current, err = st.Get(ctx, status.Metadata())
		require.NoError(t, err)

		_, ok := current.Metadata().Annotations().Get(editorHeartbeatAnnotation)
		require.True(t, ok, "a stalled request must not prevent the next heartbeat")
	})
}
