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
		calls++

		require.Equal(t, "/cluster/ha/resources", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		respond(t, w, nil)
	}))
	defer server.Close()

	p := NewProvisioner(proxmox.NewClient(server.URL))
	m := resources.NewMachine("default", "req")
	m.TypedSpec().Value.Vmid = 100
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
				respond(t, w, map[string]any{"protection": protected})

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
	require.NoError(t, p.destroyVM(ctx, zap.NewNop(), "pve1", 100))
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
	require.NoError(t, validateLiveUpdate("memory: 4096", desired))
	desired.NICs = []NIC{{Addresses: []string{"10.0.0.7/24"}}}
	require.ErrorContains(t, validateLiveUpdate("memory: 4096", desired), "guest networking")
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

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	data := Data{Disks: []Disk{{Storage: "zfs", Size: 20}, {Storage: "zfs", Size: 40}}}

	p := NewProvisioner(px)
	for range 2 {
		require.NoError(t, p.applyClonedVMConfigAndResize(ctx, zap.NewNop(), testContext(machine, ""), vm, data))
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
