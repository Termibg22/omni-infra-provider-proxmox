// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider //nolint:testpackage // Exercise the provider's persisted provisioning state.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/owned"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/api/omni/specs"
	"github.com/siderolabs/omni/client/pkg/infra/controllers"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	"github.com/siderolabs/omni/client/pkg/omni/resources/siderolink"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	providerspecs "github.com/siderolabs/omni-infra-provider-proxmox/api/specs"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/meta"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

func TestRecordProviderDataMeetsOmniMetadataRules(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"basic", "cores: 2\nmemory: 4096\n"},
		{"grouped", "compute:\n  cores: 4\n  memory: 8192\ngeneral:\n  protection: false\n"},
		{"empty", ""},
		{"empty mapping", "{}"},
		{"null", "null"},
		{"explicit false", "workload_profile: control-plane\nprotection: false\n"},
		{"multiline", "description: |\n  first line\n  segunda línea\n"},
		{"escaped controls", `description: "tab\tnewline\ncontrol\u0085"`},
		{"over annotation limit", "description: " + strings.Repeat("a", omni.MaxAnnotationValueLength+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := resources.NewMachine((resources.MachineExtension{}).ResourceDefinition().DefaultNamespace, "worker")
			machine.Metadata().Annotations().Set(appliedProviderDataAnnotation, "old: snapshot\n")

			steps := NewProvisioner(nil).ProvisionSteps()
			step := steps[len(steps)-1]
			require.Equal(t, "recordProviderData", step.Name())
			require.NoError(t, step.Run(t.Context(), zap.NewNop(), testContext(machine, tc.raw)))
			require.NoError(t, validateTestMetadata(machine))
			require.NotContains(t, machine.Metadata().Annotations().Raw(), appliedProviderDataAnnotation)

			hash, ok := machine.Metadata().Annotations().Get(providerDataHashAnnotation)
			require.True(t, ok)
			require.Equal(t, hashProviderData(tc.raw), hash)
			raw, ok := appliedProviderData(machine)
			require.True(t, ok)
			require.NotEmpty(t, raw)

			var before, after Data
			require.NoError(t, yaml.Unmarshal([]byte(tc.raw), &before))
			require.NoError(t, yaml.Unmarshal([]byte(raw), &after))
			before.ApplyWorkloadProfile()
			after.ApplyWorkloadProfile()
			require.Equal(t, before, after, "persisted data must retain explicit false and grouped settings")

			wire, err := machine.TypedSpec().Value.MarshalVT()
			require.NoError(t, err)

			var decoded providerspecs.MachineSpec
			require.NoError(t, proto.Unmarshal(wire, &decoded))
			require.Equal(t, raw, decoded.AppliedProviderData)
			wire, err = proto.Marshal(&decoded)
			require.NoError(t, err)

			var roundTrip providerspecs.MachineSpec
			require.NoError(t, roundTrip.UnmarshalVT(wire))
			require.True(t, machine.TypedSpec().Value.EqualVT(roundTrip.CloneVT()))
		})
	}
}

func TestAppliedProviderDataKeepsLiveUpdateGuards(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		machine := resources.NewMachine((resources.MachineExtension{}).ResourceDefinition().DefaultNamespace, "worker")
		machine.TypedSpec().Value.Vmid = 100
		machine.Metadata().Annotations().Set(appliedProviderDataAnnotation, "bios: seabios\n")

		if !legacy {
			recordAppliedProviderData(machine, "bios: seabios\n")
			// The new state must take precedence over any stale legacy annotation.
			machine.Metadata().Annotations().Set(appliedProviderDataAnnotation, "bios: ovmf\n")
		}

		req := infra.NewMachineRequest("worker")
		req.TypedSpec().Value.ProviderData = "bios: ovmf\n"
		// A nil client ensures rejected changes cannot reach Proxmox.
		err := NewProvisioner(nil).ReconcileExistingMachine(t.Context(), zap.NewNop(), machine, req)
		require.ErrorContains(t, err, "changing firmware", "legacy=%v", legacy)
	}
}

// In-memory COSI state does not enforce Omni's metadata rules on its own.
type metadataValidatingState struct {
	state.CoreState
}

func validateTestMetadata(res resource.Resource) error {
	for key, value := range res.Metadata().Annotations().Raw() {
		if err := omni.ValidateAnnotation(key, value); err != nil {
			return err
		}
	}

	return nil
}

func (s metadataValidatingState) Create(ctx context.Context, res resource.Resource, opts ...state.CreateOption) error {
	if err := validateTestMetadata(res); err != nil {
		return err
	}

	return s.CoreState.Create(ctx, res, opts...)
}

func (s metadataValidatingState) Update(ctx context.Context, res resource.Resource, opts ...state.UpdateOption) error {
	if err := validateTestMetadata(res); err != nil {
		return err
	}

	return s.CoreState.Update(ctx, res, opts...)
}

type provisionTestRuntime struct {
	*owned.State
}

func (r provisionTestRuntime) GetUncached(ctx context.Context, ptr resource.Pointer, opts ...state.GetOption) (resource.Resource, error) {
	return r.Get(ctx, ptr, opts...)
}

func (r provisionTestRuntime) ListUncached(ctx context.Context, kind resource.Kind, opts ...state.ListOption) (resource.List, error) {
	return r.List(ctx, kind, opts...)
}

func TestProvisionControllerResumesAfterProviderDataFailure(t *testing.T) {
	st := state.WrapCore(metadataValidatingState{CoreState: inmem.NewState()})
	// A nil Proxmox client ensures a resumed request cannot create or start a VM.
	ctrl := controllers.NewProvisionController(meta.ProviderID, NewProvisioner(nil), 1, false, nil, nil)
	runtime := provisionTestRuntime{State: owned.New(st, ctrl.Name())}

	req := infra.NewMachineRequest("worker")
	req.TypedSpec().Value.ProviderData = "cores: 2\nmemory: 4096\n"
	req.Metadata().Labels().Set(omni.LabelInfraProviderID, meta.ProviderID)
	require.NoError(t, st.Create(t.Context(), req))

	status := infra.NewMachineRequestStatus("worker")
	status.TypedSpec().Value.Stage = specs.MachineRequestStatusSpec_FAILED
	status.TypedSpec().Value.Error = "annotation value must not contain control characters"
	require.NoError(t, st.Create(t.Context(), status, state.WithCreateOwner(ctrl.Name())))

	connection := siderolink.NewConnectionParams(siderolink.ConfigID) //nolint:staticcheck // Exercise the SDK's legacy connection flow.
	connection.TypedSpec().Value.ApiEndpoint = "https://omni.example.test:443"
	connection.TypedSpec().Value.JoinToken = "test-token"
	connection.TypedSpec().Value.EventsPort = 8090
	connection.TypedSpec().Value.LogsPort = 8092
	require.NoError(t, st.Create(t.Context(), connection))

	machine := resources.NewMachine((resources.MachineExtension{}).ResourceDefinition().DefaultNamespace, "worker")
	machine.TypedSpec().Value.Vmid = 100
	machine.TypedSpec().Value.Node = "pve"
	machine.TypedSpec().Value.VmStartTask = "UPID:pve:00000001:00000001:00000001:qmstart:100:root@pam:"

	const stepAnnotation = "infra." + omni.SystemLabelPrefix + "step"

	machine.Metadata().Annotations().Set(stepAnnotation, "syncHARules")
	require.NoError(t, st.Create(t.Context(), machine, state.WithCreateOwner(ctrl.Name())))

	require.NoError(t, ctrl.Reconcile(t.Context(), zap.NewNop(), runtime, req.Metadata()))
	finished, err := safe.StateGet[*infra.MachineRequestStatus](t.Context(), st, status.Metadata())
	require.NoError(t, err)
	require.Equal(t, specs.MachineRequestStatusSpec_PROVISIONED, finished.TypedSpec().Value.Stage)
	require.Empty(t, finished.TypedSpec().Value.Error)

	stored, err := safe.StateGet[*resources.Machine](t.Context(), st, machine.Metadata())
	require.NoError(t, err)
	require.Equal(t, machine.TypedSpec().Value.Vmid, stored.TypedSpec().Value.Vmid)
	require.Equal(t, machine.TypedSpec().Value.Node, stored.TypedSpec().Value.Node)
	require.Equal(t, machine.TypedSpec().Value.VmStartTask, stored.TypedSpec().Value.VmStartTask)
	require.NotEmpty(t, stored.TypedSpec().Value.AppliedProviderData)
	require.NoError(t, validateTestMetadata(stored))
	step, _ := stored.Metadata().Annotations().Get(stepAnnotation)
	require.Equal(t, "recordProviderData", step)
}

func TestMachineUpdaterPersistsProviderData(t *testing.T) {
	previous := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = previous })

	const taskUPID = "UPID:pve:00000001:00000001:00000001:qmconfig:100:root@pam:"

	for _, adopt := range []bool{true, false} {
		st := state.WrapCore(metadataValidatingState{CoreState: inmem.NewState()})
		req := infra.NewMachineRequest("worker")
		req.TypedSpec().Value.ProviderData = "cores: 4\nmemory: 8192\n"
		status := infra.NewMachineRequestStatus("worker")
		status.TypedSpec().Value.Stage = specs.MachineRequestStatusSpec_PROVISIONED
		require.NoError(t, st.Create(t.Context(), status))

		machine := resources.NewMachine((resources.MachineExtension{}).ResourceDefinition().DefaultNamespace, "worker")
		machine.TypedSpec().Value.Vmid = 100
		machine.TypedSpec().Value.Node = "pve"

		if !adopt {
			recordAppliedProviderData(machine, "cores: 2\nmemory: 4096\n")
		}

		require.NoError(t, st.Create(t.Context(), machine, state.WithCreateOwner(meta.ProviderID+".ProvisionController")))

		var updates []map[string]any

		mux := http.NewServeMux()
		mux.HandleFunc("GET /nodes/pve/status", func(w http.ResponseWriter, _ *http.Request) { respond(t, w, map[string]any{}) })
		mux.HandleFunc("GET /nodes/pve/qemu/100/status/current", func(w http.ResponseWriter, _ *http.Request) {
			respond(t, w, map[string]any{"vmid": 100, "status": "running"})
		})
		mux.HandleFunc("GET /nodes/pve/qemu/100/config", func(w http.ResponseWriter, _ *http.Request) {
			respond(t, w, map[string]any{"scsi0": "zfs:vm-100-disk-0,size=20G", "net0": "virtio=BC:24:11:AA:BB:01,bridge=vmbr0"})
		})
		mux.HandleFunc("GET /nodes/pve/storage", func(w http.ResponseWriter, _ *http.Request) {
			respond(t, w, []map[string]any{{"storage": "zfs", "content": "images", "active": 1, "enabled": 1}})
		})
		mux.HandleFunc("POST /nodes/pve/qemu/100/config", func(w http.ResponseWriter, r *http.Request) {
			var update map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&update))
			updates = append(updates, update)

			respond(t, w, taskUPID)
		})
		mux.HandleFunc("GET /nodes/pve/tasks/", func(w http.ResponseWriter, _ *http.Request) {
			respond(t, w, map[string]any{"upid": taskUPID, "node": "pve", "status": "stopped", "exitstatus": "OK"})
		})
		mux.HandleFunc("GET /nodes/pve/qemu/100/firewall/rules", func(w http.ResponseWriter, _ *http.Request) {
			respond(t, w, []map[string]any{})
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		prov := NewProvisioner(proxmox.NewClient(srv.URL))

		reconcileSingleProvisionedRequest(t.Context(), st, prov, zaptest.NewLogger(t), req)
		stored, err := safe.StateGet[*resources.Machine](t.Context(), st, machine.Metadata())
		require.NoError(t, err)
		require.Equal(t, req.TypedSpec().Value.ProviderData, stored.TypedSpec().Value.AppliedProviderData, "adopt=%v", adopt)
		require.NoError(t, validateTestMetadata(stored))
		require.NotContains(t, stored.Metadata().Annotations().Raw(), appliedProviderDataAnnotation)
		hash, _ := stored.Metadata().Annotations().Get(providerDataHashAnnotation)
		require.Equal(t, hashProviderData(req.TypedSpec().Value.ProviderData), hash)

		if adopt {
			require.Empty(t, updates, "adopting existing machines must not change hardware")
		} else {
			require.NotEmpty(t, updates)
			require.EqualValues(t, 4, updates[len(updates)-1]["cores"])
			require.EqualValues(t, 8192, updates[len(updates)-1]["memory"])
		}

		version := stored.Metadata().Version()
		updateCount := len(updates)

		reconcileSingleProvisionedRequest(t.Context(), st, prov, zap.NewNop(), req)
		stored, err = safe.StateGet[*resources.Machine](t.Context(), st, machine.Metadata())
		require.NoError(t, err)
		require.Equal(t, version, stored.Metadata().Version())
		require.Len(t, updates, updateCount, "unchanged configuration must not be applied repeatedly")
	}
}
