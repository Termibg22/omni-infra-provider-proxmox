// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider //nolint:testpackage // Exercise the actual Proxmox requests during teardown.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/owned"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/infra/controllers"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/meta"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

func TestDeprovisionDoesNotRequireISOCleanupPermission(t *testing.T) {
	previous := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = previous })

	var (
		deletes, isoDeletes atomic.Int32
		options             atomic.Value
	)

	const task = "UPID:pve:00000001:00000001:00000001:qmdestroy:100:omni@pve!provider:"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /cluster/replication", func(w http.ResponseWriter, _ *http.Request) { respond(t, w, []map[string]any{}) })
	mux.HandleFunc("GET /nodes/pve/status", func(w http.ResponseWriter, _ *http.Request) { respond(t, w, map[string]any{}) })
	mux.HandleFunc("GET /nodes/pve/qemu/100/status/current", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"vmid": 100, "status": "stopped"})
	})
	mux.HandleFunc("GET /nodes/pve/qemu/100/config", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{
			"tags": "go-proxmox+cloud-init", "ide0": "local:iso/user-data-100.iso,media=cdrom",
			"ide2": "local:iso/talos-shared.iso,media=cdrom", "scsi0": "local-zfs:vm-100-disk-0,size=20G",
		})
	})
	mux.HandleFunc("GET /nodes/pve/storage", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, []map[string]any{{"storage": "local", "content": "iso", "active": 1, "enabled": 1}})
	})
	mux.HandleFunc("GET /nodes/pve/storage/local/content/local:iso/user-data-100.iso", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"volid": "local:iso/user-data-100.iso"})
	})
	mux.HandleFunc("DELETE /nodes/pve/storage/local/content/local:iso/user-data-100.iso", func(w http.ResponseWriter, _ *http.Request) {
		isoDeletes.Add(1)
		http.Error(w, "Permission check failed (/storage/local, Datastore.Allocate)", http.StatusForbidden)
	})
	mux.HandleFunc("DELETE /nodes/pve/qemu/100", func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		options.Store(r.URL.Query())
		respond(t, w, task)
	})
	mux.HandleFunc("GET /nodes/pve/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"upid": task, "node": "pve", "status": "stopped", "exitstatus": "OK"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	machine := resources.NewMachine((resources.MachineExtension{}).ResourceDefinition().DefaultNamespace, "worker")
	machine.TypedSpec().Value.Vmid = 100
	machine.TypedSpec().Value.Node = "pve"
	core, logs := observer.New(zap.WarnLevel)
	p := NewProvisioner(proxmox.NewClient(srv.URL))
	require.NoError(t, p.Deprovision(t.Context(), zap.New(core), machine, infra.NewMachineRequest("worker")))
	require.EqualValues(t, 1, deletes.Load(), "ISO cleanup permission must not block deleting the VM and its disks")
	require.EqualValues(t, 1, isoDeletes.Load())
	query, ok := options.Load().(url.Values)
	require.True(t, ok)
	require.Equal(t, "1", query.Get("purge"))
	require.Equal(t, "1", query.Get("destroy-unreferenced-disks"))
	require.Empty(t, query.Get("skiplock"), "normal API tokens must not require root-only options")
	require.Equal(t, 1, logs.Len(), "the remaining private ISO must be reported")
	require.Equal(t, "local:iso/user-data-100.iso", logs.All()[0].ContextMap()["volume"])
	require.Contains(t, logs.All()[0].ContextMap()["error"], "Datastore.Allocate on /storage/local")
}

func TestCloudInitCleanupOnlyDeletesOwnedAttachedISO(t *testing.T) {
	for _, tagged := range []bool{true, false} {
		var deletes atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodDelete, r.Method)
			require.Equal(t, "/nodes/pve/storage/iso-store/content/iso-store:iso/user-data-100.iso", r.URL.Path)
			deletes.Add(1)
			respond(t, w, nil) // Proxmox also permits a synchronous delete with no task.
		}))
		t.Cleanup(srv.Close)

		vm := &proxmox.VirtualMachine{
			VMID: 100, Node: "pve",
			VirtualMachineConfig: &proxmox.VirtualMachineConfig{IDEs: map[string]string{
				"ide0": "iso-store:iso/user-data-100.iso,media=cdrom",
				"ide1": "iso-store:iso/user-data-100.iso,media=cdrom",
				"ide2": "iso-store:iso/talos-shared.iso,media=cdrom",
				"ide3": "iso-store:iso/user-data-101.iso,media=cdrom",
			}},
		}
		if tagged {
			vm.VirtualMachineConfig.Tags = proxmox.MakeTag(proxmox.TagCloudInit)
		}

		p := NewProvisioner(proxmox.NewClient(srv.URL))
		p.cleanupVMCloudInitISO(t.Context(), zap.NewNop(), vm)

		if tagged {
			require.EqualValues(t, 1, deletes.Load())
		} else {
			require.Zero(t, deletes.Load())
		}
	}
}

type stalledISOCleanupTransport struct{}

func (stalledISOCleanupTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()

	return nil, req.Context().Err()
}

func TestCloudInitCleanupHasBoundedTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := proxmox.NewClient("https://pve.example.test/api2/json", proxmox.WithHTTPClient(&http.Client{Transport: stalledISOCleanupTransport{}}))
		vm := &proxmox.VirtualMachine{
			VMID: 100, Node: "pve",
			VirtualMachineConfig: &proxmox.VirtualMachineConfig{
				Tags: proxmox.MakeTag(proxmox.TagCloudInit),
				IDEs: map[string]string{"ide0": "local:iso/user-data-100.iso,media=cdrom"},
			},
		}
		core, logs := observer.New(zap.WarnLevel)
		started := time.Now()

		NewProvisioner(client).cleanupVMCloudInitISO(t.Context(), zap.New(core), vm)
		require.Equal(t, 30*time.Second, time.Since(started))
		require.Equal(t, 1, logs.Len())
		require.Contains(t, logs.All()[0].ContextMap()["error"], "context deadline exceeded")
	})
}

func TestDeprovisionControllerKeepsStateUntilDeletionSucceeds(t *testing.T) {
	previous := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = previous })

	for _, failure := range []struct{ name, errorText string }{
		{"permission", "DELETE /nodes/pve/qemu/100 (requires VM.Allocate on /vms/100)"},
		{"task failure", "failed to wait for VM 100 and disk deletion"},
		{"missing task", "returned no task; cannot verify VM and disk deletion"},
	} {
		t.Run(failure.name, func(t *testing.T) {
			var blocked atomic.Bool
			blocked.Store(true)

			const task = "UPID:pve:00000001:00000001:00000001:qmdestroy:100:omni@pve!provider:"

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodDelete && r.URL.Path == "/nodes/pve/qemu/100":
					if blocked.Load() && failure.name == "permission" {
						http.Error(w, "denied", http.StatusForbidden)

						return
					}

					if blocked.Load() && failure.name == "missing task" {
						respond(t, w, nil)

						return
					}

					respond(t, w, task)
				case r.URL.Path == "/nodes/pve/status", r.URL.Path == "/nodes/pve/qemu/100/config":
					respond(t, w, map[string]any{})
				case r.URL.Path == "/nodes/pve/qemu/100/status/current":
					respond(t, w, map[string]any{"vmid": 100, "status": "stopped"})
				case strings.HasSuffix(r.URL.Path, "/log"):
					respond(t, w, []map[string]any{{"n": 1, "t": "storage unavailable"}})
				case strings.HasPrefix(r.URL.Path, "/nodes/pve/tasks/"):
					exitStatus := "OK"
					if blocked.Load() && failure.name == "task failure" {
						exitStatus = "storage unavailable"
					}

					respond(t, w, map[string]any{"upid": task, "node": "pve", "status": "stopped", "exitstatus": exitStatus})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			p := NewProvisioner(proxmox.NewClient(srv.URL))
			ctrl := controllers.NewProvisionController(meta.ProviderID, p, 1, false, nil, nil)
			st := state.WrapCore(inmem.NewState())
			runtime := provisionTestRuntime{State: owned.New(st, ctrl.Name())}
			req := infra.NewMachineRequest("worker")
			req.Metadata().Labels().Set(omni.LabelInfraProviderID, meta.ProviderID)
			req.Metadata().Finalizers().Add(ctrl.Name())
			require.NoError(t, st.Create(t.Context(), req))
			_, err := st.Teardown(t.Context(), req.Metadata())
			require.NoError(t, err)

			machine := resources.NewMachine((resources.MachineExtension{}).ResourceDefinition().DefaultNamespace, "worker")
			machine.TypedSpec().Value.Vmid = 100
			machine.TypedSpec().Value.Node = "pve"
			require.NoError(t, st.Create(t.Context(), machine, state.WithCreateOwner(ctrl.Name())))

			err = ctrl.Reconcile(t.Context(), zap.NewNop(), runtime, req.Metadata())
			require.ErrorContains(t, err, failure.errorText)
			current, err := safe.StateGet[*infra.MachineRequest](t.Context(), st, req.Metadata())
			require.NoError(t, err)
			require.True(t, current.Metadata().Finalizers().Has(ctrl.Name()))
			_, err = st.Get(t.Context(), machine.Metadata())
			require.NoError(t, err, "failed deletion must retain the VMID for retry")

			blocked.Store(false)
			require.NoError(t, ctrl.Reconcile(t.Context(), zap.NewNop(), runtime, req.Metadata()))
			current, err = safe.StateGet[*infra.MachineRequest](t.Context(), st, req.Metadata())
			require.NoError(t, err)
			require.False(t, current.Metadata().Finalizers().Has(ctrl.Name()))
			_, err = st.Get(t.Context(), machine.Metadata())
			require.True(t, state.IsNotFoundError(err), fmt.Sprint(err))
		})
	}
}
