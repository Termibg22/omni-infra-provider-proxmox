// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider //nolint:testpackage // Exercise persistence and HTTP side effects through internal controllers.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/api/omni/specs"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.yaml.in/yaml/v4"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/ha"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

func respond(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
}

func testContext(machine *resources.Machine, raw string) provision.Context[*resources.Machine] {
	req := infra.NewMachineRequest(machine.Metadata().ID())
	req.TypedSpec().Value.ProviderData = raw

	return provision.NewContext(req, infra.NewMachineRequestStatus(machine.Metadata().ID()), machine, provision.ConnectionParams{}, nil, nil)
}

func TestSchemaPreservesAuthoredOrderAndBasicFields(t *testing.T) {
	base, err := os.ReadFile("../../../cmd/omni-infra-provider-proxmox/data/schema.json")
	require.NoError(t, err)

	for _, mode := range []string{"basic", "advanced"} {
		schema, err := BuildUISchema(string(base), mode)
		require.NoError(t, err)
		schema, err = BuildDynamicSchema(schema, ClusterOptions{DiskStorages: []string{"zfs", "local-lvm"}, Nodes: []string{"pve1"}, CPUTypes: DefaultCPUTypes})
		require.NoError(t, err)
		require.Less(t, strings.Index(schema, `"cores":`), strings.Index(schema, `"memory":`))

		var root map[string]any
		require.NoError(t, json.Unmarshal([]byte(schema), &root))
		props, ok := root["properties"].(map[string]any)
		require.True(t, ok)
		require.Contains(t, props, "ha")
		require.Contains(t, props, "bios")
		require.Contains(t, props, "disk_size")
		require.Contains(t, props, "network_bridge")
		require.NotContains(t, root, "x-omni-uischema")

		// Structure is checked by the schema tests.
		storage := props["storage"].(map[string]any) //nolint:errcheck,forcetypeassert
		require.Equal(t, "", storage["default"])
	}
}

func TestHAUsesSameNormalizedConfigurationAsProvisioning(t *testing.T) {
	calls := 0

	var body map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nodes/pve1/status":
			respond(t, w, map[string]any{})

			return
		case "/nodes/pve1/qemu/100/status/current":
			respond(t, w, map[string]any{"vmid": 100, "status": "running"})

			return
		case "/nodes/pve1/qemu/100/config":
			respond(t, w, map[string]any{"smbios1": "uuid=" + deprovisionTestUUID})

			return
		}

		calls++

		require.Equal(t, "/cluster/ha/resources", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		respond(t, w, nil)
	}))
	defer server.Close()

	p := NewProvisioner(proxmox.NewClient(server.URL))
	m := resources.NewMachine("default", "req")
	m.TypedSpec().Value.Vmid = 100
	m.TypedSpec().Value.Node = "pve1"
	m.TypedSpec().Value.Uuid = deprovisionTestUUID
	require.NoError(t, p.registerHAStep(t.Context(), zap.NewNop(), testContext(m, "ha:\n  state: none\n  group: none\n")))
	require.Zero(t, calls)
	require.False(t, m.TypedSpec().Value.HaRegistered)
	require.NoError(t, p.registerHAStep(t.Context(), zap.NewNop(), testContext(m, "general:\n  workload_profile: control-plane\n")))
	require.Equal(t, 1, calls)
	require.Equal(t, "started", body["state"])
	require.True(t, m.TypedSpec().Value.HaRegistered)

	d := Data{WorkloadProfile: "control-plane", HA: &ha.Config{State: "none"}}
	d.ApplyWorkloadProfile()
	require.Nil(t, d.HA, "explicit disable must win over profile defaults")
	d = Data{WorkloadProfile: "control-plane"}
	d.ApplyWorkloadProfile()
	require.Equal(t, "negative", d.HA.ResourceAffinity)
}

func TestVMIDReservationsNeverSkipOccupancyChecks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vmid") == "101" {
			hijacker, ok := w.(http.Hijacker)
			require.True(t, ok)

			conn, buf, err := hijacker.Hijack()
			require.NoError(t, err)

			defer conn.Close() //nolint:errcheck // Test server connection cleanup.

			_, err = fmt.Fprint(buf, "HTTP/1.1 500 VM 101 already exists\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			require.NoError(t, err)
			require.NoError(t, buf.Flush())

			return
		}

		respond(t, w, "100")
	}))
	defer server.Close()

	px := proxmox.NewClient(server.URL)
	p := NewProvisioner(px)
	c := new(proxmox.Cluster).New(px)
	id, err := p.allocateVMID(t.Context(), c, Data{})
	require.NoError(t, err)
	require.Equal(t, 100, id)
	id, err = p.allocateVMID(t.Context(), c, Data{})
	require.NoError(t, err)
	require.Equal(t, 102, id)
}

func TestVMIDCheckErrorsAreNotIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cluster/resources" {
			respond(t, w, []map[string]any{{"vmid": 100}})

			return
		}

		http.Error(w, "permission denied", http.StatusForbidden)
	}))
	defer server.Close()

	px := proxmox.NewClient(server.URL)
	p := NewProvisioner(px)
	_, err := p.allocateVMID(t.Context(), new(proxmox.Cluster).New(px), Data{MinVMID: 100, MaxVMID: 200})
	require.ErrorContains(t, err, "not authorized")
	require.Empty(t, p.pendingVMIDs)
}

func TestIPAllocationsSurviveRestartAndNICIndexChanges(t *testing.T) {
	st := state.WrapCore(inmem.NewState())
	ns := (resources.MachineExtension{}).ResourceDefinition().DefaultNamespace
	first := resources.NewMachine(ns, "one")
	p := NewProvisioner(nil, WithMachineState(st))

	const config = "nics:\n  - bridge: vmbr0\n    ip_pool: 10.0.0.10-10.0.0.11/24\n"
	require.NoError(t, p.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(first, config)))
	require.NoError(t, st.Create(t.Context(), first))
	one, err := readIPAllocations(first)
	require.NoError(t, err)
	// Restart the provider and allocate the same range on a different NIC.
	p = NewProvisioner(nil, WithMachineState(st))
	second := resources.NewMachine(ns, "two")
	raw := "nics:\n  - bridge: vmbr0\n  - bridge: vmbr1\n    ip_pool: 10.0.0.10-10.0.0.11/24\n"
	require.NoError(t, p.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(second, raw)))
	two, err := readIPAllocations(second)
	require.NoError(t, err)
	require.NotEqual(t, one["nic0"].Address, two["nic1"].Address)
	require.NoError(t, st.Create(t.Context(), second))

	third := resources.NewMachine(ns, "three")
	require.ErrorContains(t, p.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(third, config)), "exhausted")
}

func TestInvalidIPPoolRangesAndOverlaps(t *testing.T) {
	for _, bad := range []string{"10.0.0.1-10.0.1.1/24", "10.0.0.1-2001:db8::1/24", "10.0.0.20-10.0.0.10/24"} {
		_, _, _, err := parseIPPool(bad)
		require.Error(t, err, bad)
	}

	a := newIPPoolAllocator()
	require.NoError(t, a.Reserve("10.0.0.10/24", "req/nic0", "10.0.0.10/24"))
	_, err := a.Allocate("10.0.0.10-10.0.0.10/24", "other/nic1")
	require.ErrorContains(t, err, "exhausted")
	a.Release("req")
	_, err = a.Allocate("10.0.0.10/24", "other/nic1")
	require.NoError(t, err)
}

func TestFailedMachineIPAllocationKeepsOtherPoolsAvailable(t *testing.T) {
	st := state.WrapCore(inmem.NewState())
	namespace := (resources.MachineExtension{}).ResourceDefinition().DefaultNamespace
	provider := NewProvisioner(nil, WithMachineState(st))
	holder := resources.NewMachine(namespace, "holder")
	blocked := resources.NewMachine(namespace, "blocked")
	single := resources.NewMachine(namespace, "single")

	const (
		firstPool  = "nics:\n  - ip_pool: 10.0.0.1/32\n"
		secondPool = "nics:\n  - ip_pool: 10.0.0.2/32\n"
		bothPools  = firstPool + "  - ip_pool: 10.0.0.2/32\n"
	)

	require.NoError(t, provider.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(holder, secondPool)))
	require.NoError(t, st.Create(t.Context(), holder))
	require.NoError(t, st.Create(t.Context(), blocked))
	require.ErrorContains(t, provider.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(blocked, bothPools)), "exhausted")
	saved, err := readIPAllocations(blocked)
	require.NoError(t, err)
	require.Empty(t, saved)

	require.NoError(t, provider.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(single, firstPool)))
	require.NoError(t, st.Create(t.Context(), single))
	require.ErrorContains(t, provider.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(single, secondPool)), "cannot change")
	require.NoError(t, st.Destroy(t.Context(), holder.Metadata()))
	require.ErrorContains(t, provider.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(blocked, bothPools)), "exhausted")
	require.NoError(t, st.Destroy(t.Context(), single.Metadata()))

	require.NoError(t, provider.allocateMachineIPs(t.Context(), zap.NewNop(), testContext(blocked, bothPools)))
	saved, err = readIPAllocations(blocked)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1/32", saved["nic0"].Address)
	require.Equal(t, "10.0.0.2/32", saved["nic1"].Address)
}

func TestAutomaticPlacementRequiresAllRequestedStorages(t *testing.T) {
	const class = "storage: boot\nadditional_disks:\n  - storage: data\niso_storage: iso\n"

	for _, tc := range []struct {
		name          string
		class         string
		firstISO      string
		firstBootUp   int
		firstDataUp   int
		canReadSecond bool
	}{
		{"additional disk inactive", class, "iso", 1, 0, true},
		{"ISO storage accepts backups only", class, "backup", 1, 1, true},
		{"inventory unavailable on the other node", class, "iso", 1, 0, false},
		{"automatic storage needs ISO", "", "backup", 1, 1, true},
		{"automatic storage needs images", "", "iso", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				require.Equal(t, http.MethodGet, request.Method)

				switch request.URL.Path {
				case "/nodes":
					respond(t, w, []map[string]any{
						{"node": "first", "status": "online", "maxmem": 100, "mem": 1},
						{"node": "second", "status": "online", "maxmem": 100, "mem": 90},
						{"node": "removed", "status": "offline"},
					})
				case "/nodes/first/storage":
					respond(t, w, []map[string]any{
						{"storage": "boot", "enabled": 1, "active": tc.firstBootUp, "content": "images"},
						{"storage": "data", "enabled": 1, "active": tc.firstDataUp, "content": "images"},
						{"storage": "iso", "enabled": 1, "active": 1, "content": tc.firstISO},
					})
				case "/nodes/second/storage":
					if !tc.canReadSecond {
						http.Error(w, "permission denied", http.StatusForbidden)

						return
					}

					respond(t, w, []map[string]any{
						{"storage": "boot", "enabled": 1, "active": 1, "content": "images"},
						{"storage": "data", "enabled": 1, "active": 1, "content": "images"},
						{"storage": "iso", "enabled": 1, "active": 1, "content": "backup,iso"},
					})
				default:
					t.Errorf("unexpected request: %s", request.URL.Path)
					http.NotFound(w, request)
				}
			}))
			defer server.Close()

			provider := NewProvisioner(proxmox.NewClient(server.URL))
			machine := resources.NewMachine("default", "worker")
			step := provider.ProvisionSteps()[0]
			require.Equal(t, "pickNode", step.Name())
			err := step.Run(t.Context(), zap.NewNop(), testContext(machine, tc.class))

			if !tc.canReadSecond {
				require.ErrorContains(t, err, "second")
				require.ErrorContains(t, err, "not authorized")
				require.Empty(t, machine.TypedSpec().Value.Node)

				return
			}

			require.NoError(t, err)
			require.Equal(t, "second", machine.TypedSpec().Value.Node)
		})
	}
}

func TestClonePlacementUsesAttachedTemplateVolumes(t *testing.T) {
	const task = "UPID:first:00000001:00000001:00000001:qmclone:9000:root@pam:"

	for _, tc := range []struct {
		name      string
		class     string
		localSlot string
		wantNode  string
		wantStore string
	}{
		{"linked template with local EFI", "full_clone: false", "efidisk0", "first", ""},
		{"full template with local TPM", "", "tpmstate0", "first", "local"},
		{"template with local ISO", "", "ide2", "first", "local"},
		{"linked shared template", "full_clone: false", "", "second", ""},
		{"full shared template selects shared destination", "", "", "second", "shared"},
		{"full clone with local destination", "storage: local", "", "first", "local"},
		{"requested remote node incompatible", "node: second", "scsi0", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cloned map[string]any

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/nodes":
					respond(t, w, []map[string]any{
						{"node": "first", "status": "online", "maxmem": 100, "mem": 90},
						{"node": "second", "status": "online", "maxmem": 100, "mem": 1},
					})
				case "/cluster/resources":
					respond(t, w, []map[string]any{{"type": "qemu", "vmid": 9000, "node": "first", "template": 1}})
				case "/nodes/first/status", "/nodes/first/qemu/9000/status/current":
					respond(t, w, map[string]any{"vmid": 9000})
				case "/nodes/first/qemu/9000/config":
					config := map[string]any{"template": 1, "scsi0": "shared:base-9000-disk-0,size=20G", "ide0": "none,media=cdrom", "unused0": "local:unused"}
					if tc.localSlot != "" {
						config[tc.localSlot] = "file=local:vol-9000"
					}

					respond(t, w, config)
				case "/nodes/first/storage", "/nodes/second/storage":
					respond(t, w, []map[string]any{
						{"storage": "local", "enabled": 1, "active": 1, "content": "images,iso"},
						{"storage": "shared", "enabled": 1, "active": 1, "shared": 1, "content": "images"},
					})
				case "/nodes/first/qemu/9000/clone":
					require.NoError(t, json.NewDecoder(r.Body).Decode(&cloned))
					respond(t, w, task)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			class := "provisioning_mode: clone\ntemplate_vmid: '9000'\n" + tc.class
			machine := resources.NewMachine("default", "worker")
			machine.TypedSpec().Value.Uuid = deprovisionTestUUID
			prov := NewProvisioner(proxmox.NewClient(server.URL))
			pctx := testContext(machine, class)
			err := prov.ProvisionSteps()[0].Run(t.Context(), zap.NewNop(), pctx)

			require.Equal(t, tc.wantNode, machine.TypedSpec().Value.Node)

			if tc.wantNode == "" {
				require.ErrorContains(t, err, tc.localSlot)

				return
			}

			require.NoError(t, err)

			var data Data
			require.NoError(t, pctx.UnmarshalProviderData(&data))

			err = prov.cloneFromTemplate(t.Context(), zap.NewNop(), pctx, data, 101, "")
			require.Error(t, err, "pending clone task")
			require.Equal(t, task, machine.TypedSpec().Value.VmCreateTask)
			require.Equal(t, tc.wantNode, cloned["target"])

			if tc.wantStore == "" {
				require.NotContains(t, cloned, "storage")
			} else {
				require.Equal(t, tc.wantStore, cloned["storage"])
			}
		})
	}
}

func TestISOStorageSelection(t *testing.T) {
	storages := proxmox.Storages{
		&proxmox.Storage{Name: "a-offline", Content: "iso", Enabled: 1},
		&proxmox.Storage{Name: "b-disabled", Content: "iso", Active: 1},
		&proxmox.Storage{Name: "c-images", Content: "images", Enabled: 1, Active: 1},
		&proxmox.Storage{Name: "z-local", Content: "iso", Enabled: 1, Active: 1},
		&proxmox.Storage{Name: "shared", Content: "backup,iso", Enabled: 1, Active: 1, Shared: 1},
	}

	selected, err := isoStorageCandidates("pve1", storages, "")
	require.NoError(t, err)
	require.Len(t, selected, 2)
	require.Equal(t, "shared", selected[0].Name)
	require.Equal(t, 1, selected[0].Shared)

	selected, err = isoStorageCandidates("pve1", storages, "z-local")
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, "z-local", selected[0].Name)

	for _, name := range []string{"a-offline", "b-disabled", "c-images", "missing"} {
		_, err = isoStorageCandidates("pve1", storages, name)
		require.ErrorContains(t, err, name)
		require.ErrorContains(t, err, "pve1")
	}

	_, err = isoStorageCandidates("pve1", storages[:3], "")
	require.ErrorContains(t, err, "no active ISO storage")
}

func TestBootISOLookupRespectsRecordedStorage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pinned    string
		requested string
		wantError string
		lookups   []string
	}{
		{"upgrade with automatic storage", "", "", "", []string{"a-new", "z-original"}},
		{"upgrade with configured storage", "", "a-new", "a-new", []string{"a-new"}},
		{"recorded storage lost the ISO", "a-new", "", "a-new", []string{"a-new"}},
		{"recorded storage went offline", "offline", "", "offline", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lookups []string

			mux := http.NewServeMux()
			mux.HandleFunc("GET /nodes/pve/status", func(w http.ResponseWriter, _ *http.Request) { respond(t, w, map[string]any{}) })
			mux.HandleFunc("GET /nodes/pve/storage", func(w http.ResponseWriter, _ *http.Request) {
				respond(t, w, []map[string]any{
					{"storage": "z-original", "content": "iso", "active": 1, "enabled": 1},
					{"storage": "a-new", "content": "iso", "active": 1, "enabled": 1},
					{"storage": "offline", "content": "iso", "active": 0, "enabled": 1},
				})
			})
			mux.HandleFunc("GET /nodes/pve/storage/{store}/content/{volume...}", func(w http.ResponseWriter, r *http.Request) {
				store := r.PathValue("store")
				lookups = append(lookups, store)
				require.Equal(t, store+":iso/talos.iso", r.PathValue("volume"))

				if store == "z-original" {
					respond(t, w, map[string]any{})

					return
				}

				http.NotFound(w, r)
			})

			server := httptest.NewServer(mux)
			defer server.Close()

			node, err := proxmox.NewClient(server.URL).Node(t.Context(), "pve")
			require.NoError(t, err)

			machine := resources.NewMachine("default", "worker")
			machine.TypedSpec().Value.VolumeId = "talos.iso"

			if tc.pinned != "" {
				machine.Metadata().Annotations().Set(bootISOStorageAnnotation, tc.pinned)
			}

			iso, err := resolveBootISO(t.Context(), node, machine, tc.requested)
			require.Equal(t, tc.lookups, lookups)

			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Equal(t, tc.pinned, machine.Metadata().Annotations().Raw()[bootISOStorageAnnotation])

				return
			}

			require.NoError(t, err)
			require.Equal(t, "z-original:iso/talos.iso", iso.VolID)
			require.Equal(t, "z-original", machine.Metadata().Annotations().Raw()[bootISOStorageAnnotation])
		})
	}
}

func TestMTUTriggersGuestNetworkConfig(t *testing.T) {
	raw, err := buildCloudInitNetworkConfig(Data{}, []NIC{{MTU: 9000}}, nil, nil)
	require.NoError(t, err)
	require.Contains(t, raw, "mtu: 9000")
	require.Contains(t, raw, "version: 2")
}

func TestExplicitFalseDeviceOptions(t *testing.T) {
	f := false
	opts := buildPCIDeviceOptions([]PCIDevice{{Mapping: "gpu", ROMBar: &f}})
	require.Contains(t, opts[0].Value, "rombar=0")
	opts = buildDiskOptions([]Disk{{Replicate: &f}}, []string{"zfs"})
	require.Contains(t, opts[0].Value, "replicate=0")
}

func TestDiskUpdatePreservesForeignFlags(t *testing.T) {
	got := buildUpdatedExistingDiskSpec("zfs:vm-100-disk-0,size=20G,ro=1,werror=stop,ssd=1", "zfs:40,discard=on")
	require.Equal(t, "zfs:vm-100-disk-0,ro=1,werror=stop,discard=on", got)
}

func TestLegacyMachineBaselineDoesNotTouchProxmox(t *testing.T) {
	st := state.WrapCore(inmem.NewState())
	req := infra.NewMachineRequest("legacy")
	status := infra.NewMachineRequestStatus("legacy")
	status.TypedSpec().Value.Stage = specs.MachineRequestStatusSpec_PROVISIONED
	machine := resources.NewMachine((resources.MachineExtension{}).ResourceDefinition().DefaultNamespace, "legacy")
	machine.TypedSpec().Value.Vmid = 100

	require.NoError(t, st.Create(t.Context(), status))
	require.NoError(t, st.Create(t.Context(), machine))
	// A nil Proxmox client deliberately makes any attempted VM mutation fail.
	reconcileSingleProvisionedRequest(t.Context(), st, NewProvisioner(nil), zap.NewNop(), req)
	stored, err := st.Get(t.Context(), machine.Metadata())
	require.NoError(t, err)

	hash, _ := stored.Metadata().Annotations().Get(providerDataHashAnnotation)
	require.Equal(t, hashProviderData(""), hash)
}

func TestProtectedMachineCanBeDeprovisioned(t *testing.T) {
	previous := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = previous })

	protected := true
	deleted := false

	const task = "UPID:pve1:00000001:00000001:00000001:qmconfig:100:root@pam:"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/nodes/pve1/status":
			respond(t, w, map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			respond(t, w, map[string]any{"status": "stopped", "vmid": 100})
		case strings.HasSuffix(r.URL.Path, "/config"):
			if r.Method == http.MethodGet {
				respond(t, w, map[string]any{"protection": protected, "smbios1": "uuid=" + deprovisionTestUUID})

				return
			}

			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, float64(0), body["protection"])

			protected = false

			respond(t, w, task)
		case strings.Contains(r.URL.Path, "/tasks/"):
			respond(t, w, map[string]any{"upid": task, "node": "pve1", "status": "stopped", "exitstatus": "OK"})
		case r.Method == http.MethodDelete:
			require.False(t, protected)

			deleted = true

			respond(t, w, task)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	p := NewProvisioner(proxmox.NewClient(server.URL))
	machine := resources.NewMachine("default", "worker")
	machine.TypedSpec().Value.Vmid = 100
	machine.TypedSpec().Value.Uuid = deprovisionTestUUID
	machine.TypedSpec().Value.Node = "pve1"
	require.NoError(t, p.destroyVM(ctx, zap.NewNop(), machine))
	require.True(t, deleted)
}

func TestStorageSelectorsRejectNonDiskStorage(t *testing.T) {
	storages := proxmox.Storages{
		&proxmox.Storage{Name: "disabled", Content: "images", Enabled: 0},
		&proxmox.Storage{Name: "inactive", Content: "images", Active: 0, Enabled: 1},
		&proxmox.Storage{Name: "local-lvm", Content: "rootdir", Active: 1, Enabled: 1},
		&proxmox.Storage{Name: "iso", Content: "iso", Active: 1, Enabled: 1},
		&proxmox.Storage{Name: "disks", Content: "images", Active: 1, Enabled: 1},
	}
	for _, selector := range []string{"", `true`, `name == "disks"`, "disks"} {
		got, err := matchStorage("pve1", storages, selector)
		require.NoError(t, err)
		require.Equal(t, "disks", got)
	}

	for _, selector := range []string{`name == "disabled"`, `name == "inactive"`, `name == "local-lvm"`, "local-lvm"} {
		_, err := matchStorage("pve1", storages, selector)
		require.Error(t, err)
	}
}

func TestInvalidBondAndVolumeConfigurationsFailBeforeProvisioning(t *testing.T) {
	for _, d := range []Data{
		{Bonds: []NetBond{{Name: "bond0", Interfaces: []string{"net3"}}}},
		{NICs: []NIC{{Addresses: []string{"10.0.0.2/24"}}}, Bonds: []NetBond{{Name: "bond0", Interfaces: []string{"net0"}}}},
		{Disks: []Disk{{}, {VolumeName: "../../invalid"}}},
		{SecureBoot: true, Bios: "seabios"},
	} {
		d.ApplyWorkloadProfile()
		require.Error(t, d.Validate(), fmt.Sprint(d))
	}
}

func TestNullPersistedIPAllocationsReturnError(t *testing.T) {
	machine := resources.NewMachine("default", "worker")
	machine.Metadata().Annotations().Set(ipAllocationsAnnotation, "null")
	_, err := readIPAllocations(machine)
	require.ErrorContains(t, err, "expected an object")
}

func TestFirewallFalseValuesReachTheAPI(t *testing.T) {
	var body map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, "/nodes/pve1/qemu/100/firewall/options", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		respond(t, w, nil)
	}))
	defer server.Close()

	px := proxmox.NewClient(server.URL)
	vm := new(proxmox.VirtualMachine)
	vm.New(px, "pve1", 100)

	f := false
	require.NoError(t, NewProvisioner(px).applyVMFirewallOptions(t.Context(), zap.NewNop(), vm, &VMFirewallOptions{Enable: &f, DHCP: &f, NDP: &f, Radv: &f, IPFilter: &f}))

	for _, key := range []string{"enable", "dhcp", "ndp", "radv", "ipfilter"} {
		require.Equal(t, float64(0), body[key], key)
	}
}

func TestLiveChangesRejectGuestConfigurationInsteadOfIgnoringIt(t *testing.T) {
	desired := Data{Memory: 8192}
	desired.ApplyWorkloadProfile()
	_, err := validateLiveUpdate("memory: 4096", desired)
	require.NoError(t, err)

	desired.NICs = []NIC{{Addresses: []string{"10.0.0.7/24"}}}
	_, err = validateLiveUpdate("memory: 4096", desired)
	require.ErrorContains(t, err, "guest networking")
}

func TestLiveDiskUpdatesRejectUnsupportedChangesBeforeProxmox(t *testing.T) {
	const applied = `storage: zfs
disks:
  - {size: 20}
  - {bus: sata, size: 80, storage_selector: 'name == "bulk"'}
  - {size: 40, storage: fast}
`

	for _, test := range []struct{ name, desired, wantError string }{
		{"inherited boot storage", strings.Replace(applied, "storage: zfs", "storage: ceph", 1), "scsi0 storage"},
		{"data disk selector", strings.Replace(applied, "bulk", "archive", 1), "sata0 storage"},
		{"data disk shrink", strings.Replace(applied, "size: 40", "size: 39", 1), "shrink disk scsi1"},
		{"explicit to inherited", strings.Replace(applied, ", storage: fast", "", 1), "scsi1 storage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			machine := resources.NewMachine("default", "worker")
			machine.TypedSpec().Value.Vmid = 100
			recordAppliedProviderData(machine, applied)

			req := infra.NewMachineRequest("worker")
			req.TypedSpec().Value.ProviderData = test.desired + "node: pve2\n"
			require.ErrorContains(t, NewProvisioner(nil).ReconcileExistingMachine(t.Context(), zap.NewNop(), machine, req), test.wantError)
			require.Equal(t, applied, machine.TypedSpec().Value.AppliedProviderData)
		})
	}

	var desired Data
	require.NoError(t, yaml.Unmarshal([]byte(strings.Replace(applied, "size: 40", "size: 90", 1)+"  - {size: 50, storage: archive}\n"), &desired))
	_, err := validateLiveUpdate(applied, desired)
	require.NoError(t, err)
}

func TestLiveUpdatesRespectDeviceOwnership(t *testing.T) {
	previous := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = previous })

	const task = "UPID:pve1:00000001:00000001:00000001:qmconfig:100:root@pam:"

	for _, test := range []struct {
		name        string
		previous    string
		desired     string
		conflict    string
		wantDeleted string
		wantUnlink  []string
	}{
		{name: "only RAM changed", previous: "memory: 4096"},
		{name: "no applied snapshot"},
		{name: "cannot claim a data disk", previous: "{}", desired: "additional_disks: [{disk_size: 60}]", conflict: "scsi1"},
		{name: "cannot claim an extra NIC", previous: "{}", desired: "additional_nics: [{bridge: vmbr1}]", conflict: "net1"},
		{name: "cannot claim a PCI device", previous: "{}", desired: "pci_devices: [{mapping: gpu}]", conflict: "hostpci0"},
		{name: "cannot claim a USB device", previous: "{}", desired: "usb_devices: [{mapping: radio}]", conflict: "usb0"},
		{
			name: "class removed devices",
			previous: `additional_disks: [{disk_size: 40}]
additional_nics: [{bridge: vmbr1}]
pci_devices: [{mapping: gpu}]
usb_devices: [{mapping: radio}]`,
			wantUnlink: []string{"scsi1"}, wantDeleted: "hostpci0,net1,usb0",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var unlinked []string

			var updated map[string]any

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/nodes/pve1/status":
					respond(t, w, map[string]any{})
				case strings.HasSuffix(r.URL.Path, "/status/current"):
					respond(t, w, map[string]any{"status": "running", "vmid": 100})
				case strings.HasSuffix(r.URL.Path, "/storage"):
					respond(t, w, []map[string]any{{"storage": "zfs", "content": "images", "enabled": 1, "active": 1}})
				case strings.HasSuffix(r.URL.Path, "/config") && r.Method == http.MethodGet:
					respond(t, w, map[string]any{
						"smbios1": "uuid=11111111-1111-1111-1111-111111111111",
						"scsi0":   "zfs:vm-100-disk-0,size=20G", "scsi1": "zfs:vm-100-disk-1,size=40G", "sata3": "zfs:vm-100-disk-9,size=200G",
						"net0": "virtio=AA:BB:CC:DD:EE:01", "net1": "virtio=AA:BB:CC:DD:EE:02", "net4": "virtio=AA:BB:CC:DD:EE:05",
						"hostpci0": "mapping=gpu", "hostpci3": "mapping=manual", "usb0": "mapping=radio", "usb1": "mapping=unmanaged",
					})
				case strings.HasSuffix(r.URL.Path, "/config") && r.Method == http.MethodPost:
					require.NoError(t, json.NewDecoder(r.Body).Decode(&updated))
					respond(t, w, task)
				case strings.HasSuffix(r.URL.Path, "/unlink"):
					var args map[string]string
					require.NoError(t, json.NewDecoder(r.Body).Decode(&args))
					require.NotContains(t, args, "force")

					unlinked = append(unlinked, args["idlist"])

					respond(t, w, task)
				case strings.Contains(r.URL.Path, "/tasks/"):
					respond(t, w, map[string]any{"upid": task, "node": "pve1", "status": "stopped", "exitstatus": "OK"})
				case strings.HasSuffix(r.URL.Path, "/firewall/rules"):
					respond(t, w, []any{})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			machine := resources.NewMachine("default", "worker")
			machine.TypedSpec().Value.Node = "pve1"
			machine.TypedSpec().Value.Vmid = 100
			machine.TypedSpec().Value.Uuid = "11111111-1111-1111-1111-111111111111"

			if test.previous != "" {
				recordAppliedProviderData(machine, test.previous)
			}

			request := infra.NewMachineRequest("worker")
			request.TypedSpec().Value.ProviderData = "memory: 8192"

			if test.conflict != "" {
				request.TypedSpec().Value.ProviderData += "\nnode: pve2\n" + test.desired
			}

			err := NewProvisioner(proxmox.NewClient(server.URL)).ReconcileExistingMachine(t.Context(), zap.NewNop(), machine, request)
			if test.conflict != "" {
				require.ErrorContains(t, err, "slot "+test.conflict+" is already occupied")
				require.Empty(t, updated)
				require.Empty(t, unlinked)
				require.Equal(t, test.previous, machine.TypedSpec().Value.AppliedProviderData)

				return
			}

			require.NoError(t, err)
			require.Equal(t, float64(8192), updated["memory"])
			require.ElementsMatch(t, test.wantUnlink, unlinked)

			if test.wantDeleted == "" {
				require.NotContains(t, updated, "delete")
			} else {
				require.Equal(t, test.wantDeleted, updated["delete"])
			}
		})
	}
}

func TestWorkloadDefaultsRespectExplicitFalse(t *testing.T) {
	var data Data
	require.NoError(t, yaml.Unmarshal([]byte(`
general:
  workload_profile: hardened-secure
  protection: false
system:
  secure_boot: false
  tpm: false
`), &data))
	data.ApplyWorkloadProfile()
	require.False(t, data.Protection)
	require.False(t, data.SecureBoot)
	require.False(t, data.TPM)
	require.NoError(t, yaml.Unmarshal([]byte(`
workload_profile: storage-node
disks:
  - size: 20
    ssd: false
    discard: false
    iothread: false
`), &data))
	disks := data.EffectiveDisks()
	require.False(t, disks[0].SSD)
	require.False(t, disks[0].Discard)
	require.False(t, disks[0].IOThread)
}

func TestSecurityGroupsPreserveRulesOwnedByAnAdministrator(t *testing.T) {
	var removed []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			respond(t, w, []map[string]any{
				{"type": "group", "action": "manual-policy", "pos": 0},
				{"type": "group", "action": "old-managed-policy", "pos": 1, "comment": poolComment},
			})
		case http.MethodDelete:
			removed = append(removed, r.URL.Path)

			respond(t, w, nil)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()

	px := proxmox.NewClient(server.URL)
	vm := new(proxmox.VirtualMachine)
	vm.New(px, "pve1", 100)
	require.NoError(t, reconcileVMSecurityGroups(t.Context(), zap.NewNop(), vm, nil))
	require.Equal(t, []string{"/nodes/pve1/qemu/100/firewall/rules/1"}, removed)
}

func TestClonedDiskConfigurationIsIdempotent(t *testing.T) {
	previous := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = previous })

	const task = "UPID:pve1:00000001:00000001:00000001:qmconfig:100:root@pam:"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/nodes/pve1/status":
			respond(t, w, map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/storage"):
			respond(t, w, []map[string]any{{"storage": "zfs", "content": "images", "active": 1, "enabled": 1}})
		case strings.HasSuffix(r.URL.Path, "/config"):
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.NotContains(t, body, "scsi0", "existing boot volume must survive retries")
			require.NotContains(t, body, "scsi1", "existing data volume must survive retries")
			require.Equal(t, "clone", body["name"])
			require.Equal(t, "uuid="+deprovisionTestUUID, body["smbios1"])
			respond(t, w, task)
		case strings.Contains(r.URL.Path, "/tasks/"):
			respond(t, w, map[string]any{"upid": task, "node": "pve1", "status": "stopped", "exitstatus": "OK"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()

	px := proxmox.NewClient(server.URL)
	vm := new(proxmox.VirtualMachine)
	vm.New(px, "pve1", 100)
	vm.VirtualMachineConfig = &proxmox.VirtualMachineConfig{SCSIs: map[string]string{"scsi0": "zfs:vm-100-disk-0,size=20G", "scsi1": "zfs:vm-100-disk-1,size=40G"}}
	machine := resources.NewMachine("default", "clone")
	machine.TypedSpec().Value.Uuid = deprovisionTestUUID

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	data := Data{Disks: []Disk{{Storage: "zfs", Size: 20}, {Storage: "zfs", Size: 40}}}

	p := NewProvisioner(px)
	for range 2 {
		require.NoError(t, p.applyClonedVMConfigAndResize(ctx, zap.NewNop(), testContext(machine, ""), vm, data))
	}
}

func TestTemplateCloneStoragePolicy(t *testing.T) {
	const task = "UPID:pve1:00000001:00000001:00000001:qmclone:9000:root@pam:"

	for _, test := range []struct {
		name     string
		storage  string
		resolved string
		full     bool
		reject   bool
	}{
		{name: "linked inherits template storage", resolved: "other"},
		{name: "linked with matching selection", storage: "zfs", resolved: "zfs"},
		{name: "linked cannot move disks", storage: "other", resolved: "other", reject: true},
		{name: "full uses destination storage", storage: "other", resolved: "other", full: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var clone map[string]any

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/cluster/resources":
					respond(t, w, []map[string]any{{"type": "qemu", "vmid": 9000, "node": "pve1", "template": 1}})
				case "/nodes/pve1/status", "/nodes/pve1/qemu/9000/status/current":
					respond(t, w, map[string]any{"vmid": 9000})
				case "/nodes/pve1/qemu/9000/config":
					respond(t, w, map[string]any{"template": 1, "scsi0": "zfs:base-9000-disk-0,size=20G"})
				case "/nodes/pve1/storage":
					respond(t, w, []map[string]any{
						{"storage": "zfs", "enabled": 1, "active": 1, "content": "images"},
						{"storage": "other", "enabled": 1, "active": 1, "content": "images"},
					})
				case "/nodes/pve1/qemu/9000/clone":
					require.NoError(t, json.NewDecoder(r.Body).Decode(&clone))
					respond(t, w, task)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			prov := NewProvisioner(proxmox.NewClient(server.URL))
			machine := resources.NewMachine("default", "worker")
			machine.TypedSpec().Value.Node = "pve1"
			data := Data{ProvisioningMode: "clone", TemplateVMID: "9000", FullClone: &test.full, Storage: test.storage}
			machine.TypedSpec().Value.Uuid = deprovisionTestUUID
			err := prov.cloneFromTemplate(t.Context(), zap.NewNop(), testContext(machine, ""), data, 100, "")

			if test.reject {
				require.ErrorContains(t, err, "full clone")
				require.Nil(t, clone)
				require.Empty(t, machine.TypedSpec().Value.VmCreateTask)

				return
			}

			require.Equal(t, task, machine.TypedSpec().Value.VmCreateTask)
			require.Error(t, err, "the clone task must be polled asynchronously")
			require.Equal(t, float64(100), clone["newid"])
			require.Equal(t, "omni-"+deprovisionTestUUID, clone["name"])

			if test.full {
				require.Equal(t, test.resolved, clone["storage"])
			} else {
				require.NotContains(t, clone, "storage", "Proxmox rejects a storage argument on a linked clone")
			}
		})
	}
}

func TestReplicationErrorsAndOwnershipAreEnforced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			respond(t, w, []map[string]any{{"id": "100-0", "target": "pve2", "comment": "manual"}})

			return
		}

		t.Errorf("must not modify an administrator's replication job: %s", r.Method)
	}))
	defer server.Close()

	p := NewProvisioner(proxmox.NewClient(server.URL))
	err := p.reconcileReplicationJob(t.Context(), zap.NewNop(), 100, "pve1", Data{Replication: &ReplicationConfig{Enabled: true, TargetNode: "pve2"}})
	require.ErrorContains(t, err, "not managed")
	require.NoError(t, p.cleanupReplicationJobs(t.Context(), zap.NewNop(), 100))
}

func TestVolumePatchesAreUniqueAndSelectOnlyTheirOwnDisk(t *testing.T) {
	disk := Disk{VolumeName: "data", Size: 500}
	one, raw := diskVolumeConfigPatch("request-one", disk)
	two, _ := diskVolumeConfigPatch("request-two", disk)
	require.NotEqual(t, one, two)
	require.Contains(t, string(raw), `!system_disk && disk.serial == "vol-data"`)
	require.NotContains(t, string(raw), "||")
	require.Contains(t, string(raw), "minSize: 499GiB")
	require.Contains(t, string(raw), "grow: true")
}

func TestVMTagUpdatePreservesCloudInitCleanupMarker(t *testing.T) {
	marker := proxmox.MakeTag(proxmox.TagCloudInit)
	vm := &proxmox.VirtualMachine{VirtualMachineConfig: &proxmox.VirtualMachineConfig{Tags: "old-user-tag;" + marker}}
	value := existingVMDesiredTags(vm, nil, "workers")
	require.Contains(t, value, marker)
	require.Contains(t, value, "machine-request.workers")
	require.NotContains(t, value, "old-user-tag")
}

func TestEmptyTaskFailsWithoutPanicking(t *testing.T) {
	require.ErrorContains(t, NewProvisioner(nil).waitForTaskToFinish(t.Context(), nil), "empty task")
}
