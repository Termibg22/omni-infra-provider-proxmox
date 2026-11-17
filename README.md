# Omni Infrastructure Provider for Proxmox

Provision Talos VMs on Proxmox VE from Omni Machine Classes.

## Run the provider

You need a Proxmox cluster, an Omni **infrastructure provider key**, and connectivity from the provider to both APIs.
Use a Proxmox API token or a username/password pair, with permission to manage the requested VMs, storage, pools, and optional HA resources.

Create `config.yaml`:

```yaml
proxmox:
  url: https://pve.example.com:8006/api2/json
  tokenID: provider@pve!omni
  tokenSecret: REPLACE_ME
  # Optional allocation range:
  minVMID: 5000
  maxVMID: 5999
  # Optional node groups:
  nodeGroups:
    rack-a: [pve1, pve2]
```

For password authentication, replace `tokenID` and `tokenSecret` with `username`, `password`, and `realm` (for example, `pam`).
Configure exactly one authentication method.
Certificate verification is enabled by default; `insecureSkipVerify: true` explicitly disables it for Proxmox.

Set `OMNI_ENDPOINT` and `OMNI_SERVICE_ACCOUNT_KEY` in your environment.
Despite its historical name, `OMNI_SERVICE_ACCOUNT_KEY` must contain an **infrastructure provider key**.

```sh
docker run -d --name omni-proxmox --restart unless-stopped \
  --mount type=bind,source="$(pwd)/config.yaml",target=/config.yaml,readonly \
  -e OMNI_ENDPOINT -e OMNI_SERVICE_ACCOUNT_KEY \
  ghcr.io/termibg22/omni-infra-provider-proxmox:v4.1.7 \
  --config-file /config.yaml
```

`v4.1.7` adds an editor keepalive to mitigate idle connection resets that discard unsaved changes in hosted Omni. It retains physical PCI/USB discovery and works with unmodified Omni.
Do not use `latest` to pin a production deployment.

## Create a Machine Class

In Omni, create an **Auto Provision** Machine Class and select the Proxmox provider.
For ordinary DHCP machines, the provider defaults to 2 cores, 4096 MB RAM, a 20 GB SCSI boot disk, and a VirtIO NIC on `vmbr0`.
Select a storage and bridge that exist on every eligible target node, or constrain placement with a node/group.

The form uses flat fields so previously saved CPU, memory, boot disk and primary NIC values remain visible.
For a normal VM, use Boot Disk and Primary NIC. Additional Disks and Additional NICs append devices.
Grouped v4 YAML remains accepted by the provider, but the flat form cannot display values stored only inside those old groups; edit such manifests with `omnictl`.

```yaml
cores: 4
memory: 8192
disk_size: 40
storage: local-zfs
network_bridge: vmbr0
```

For YAML-only per-device tuning, `disks` and `nics` arrays replace the boot disk and primary NIC controls. These arrays are no longer shown in the form; edit classes that use them with `omnictl`:

```yaml
cores: 4
memory: 8192
disks:
  - storage: local-zfs
    size: 40
nics:
  - bridge: vmbr0
    vlan: 100
```

The first disk is the boot disk.
Later entries are data disks, assigned consecutive slots on their chosen bus (`scsi`, `virtio`, or `sata`).
Legacy fields such as `cores`, `memory`, `disk_size`, `storage_selector`, `network_bridge`, `additional_disks`, and `additional_nics` remain supported.
Field names use snake_case: `cores`, `disk_size`, and `storage_selector`, rather than `cpu`, `diskSize`, or `storageSelector`.

For CLI usage, a Machine Class is an Omni resource:

```yaml
metadata:
  namespace: default
  type: MachineClasses.omni.sidero.dev
  id: proxmox-small
spec:
  auto_provision:
    provider_id: proxmox
    provider_data: |
      cores: 4
      memory: 8192
      disk_size: 40
      storage: local-zfs
      network_bridge: vmbr0
```

Apply it with `omnictl apply -f machine-class.yaml`, then select the class when configuring a cluster's machine sets.

## Form compatibility and updates (v4.1.7)

Both schema modes show the same streamlined VM form. `--schema-mode` remains accepted for compatibility.
The hosted Omni renderer generates its own layout and ignores provider-supplied tabs. It does not apply field defaults when opening the form, but does apply item-property defaults when adding an array item.
Boot Disk and Primary NIC have separate headings. Cores, Memory and Size use short labels; units and defaults are in their help text.
Every dropdown offers an empty first row to clear a selection; `auto` is no longer a visible option.
Workload profiles, network bonds, CEL storage expressions, complete-device overrides, VM Firewall Options, ZFS Storage Replication and Resource Affinity are no longer shown in the form. Existing YAML using these fields remains supported.
Storage is the only storage selector in the form. Choosing it takes precedence over a previously saved CEL rule; leaving it empty preserves that rule when one exists.
The current Omni renderer cannot collapse provider-defined sections.
Blank placement fields mean `auto`; omitted or zero CPU/RAM values use 2 cores and 4096 MiB. Saved nonzero values are displayed directly.
Numeric limits live in validation subschemas to avoid Omni 1.13.3 converting missing values to NaN/null on mount.
Adding an Additional Disk writes 20 into Size immediately. Existing disks with an omitted, null or zero size also use 20 GiB; a size is no longer required solely to accept that default.
Clone fields remain visible in both modes: the stock editor does not accept provider-defined visibility rules. Schema conditions validate a required template in clone mode, but cannot hide controls. Its variant selector replaces data when switching, so it is not used to hide saved settings.

Node choices come from the current `/nodes` inventory. Disk destinations require `enabled=1`, `active=1` and `images` in their per-node content list.
A `rootdir`-only storage supports containers, not VM disks, and is excluded. Stale references in config, HA or mappings do not create selectable nodes.
Dropdown choices are discovered and published once when the provider starts. Restart it to reflect topology changes; actual provisioning still checks current nodes and storage.
The periodic schema publisher has been removed, including when an older deployment still passes `--schema-refresh-interval`.
PCI Device and USB Device list named Resource Mappings plus physical devices discovered on current online nodes. Physical choices show `node/address (device name)`, with a vendor/product ID fallback when the API does not report a name. USB hubs and entries without a usable port are excluded; USB addresses use the bus and topology path, not the transient device number. Hardware is never preselected when adding a device.
Omni does not filter the visible choices when Target Node or Node Group changes. Conditional schema validation checks that the selected placement can provide every requested device on one common node; provisioning checks the current Proxmox API again.
Empty mapping lists can mean no mappings exist or the token cannot see them. Configure them in Proxmox Datacenter > Resource Mappings and grant the appropriate mapping permissions; see the [PCI API](https://github.com/proxmox/pve-manager/blob/master/PVE/API2/Cluster/Mapping/PCI.pm) and [USB API](https://github.com/proxmox/pve-manager/blob/master/PVE/API2/Cluster/Mapping/USB.pm). Restart the provider after changing the inventory or permissions.
Physical inventory requires `Sys.Audit` or `Sys.Modify` on `/` for [PCI](https://github.com/proxmox/pve-manager/blob/master/PVE/API2/Hardware/PCI.pm), and `Sys.Modify` on `/` for [USB](https://github.com/proxmox/pve-manager/blob/master/PVE/API2/Hardware/USB.pm). Inventory failures are logged per node and device type without hiding successfully discovered mappings or other device types.
Proxmox allows direct physical passthrough only with `root@pam` password authentication; API tokens must use a Resource Mapping, as enforced by its [VM permission checks](https://github.com/proxmox/qemu-server/blob/master/src/PVE/API2/Qemu.pm). The provider does not create mappings or change permissions automatically. Physical selections pin placement to their named node and are checked again before VM creation or updates; the schema also rejects combinations spanning different nodes.

The provider emits a metadata heartbeat every 30 seconds on its own `InfraProviderStatus`, which the editor already watches. This keeps otherwise idle browser connections active without changing the schema, rediscovering inventory, updating Machine Classes or touching VMs. The existing health heartbeat uses a separate resource that the editor does not watch. Set `--editor-keepalive-interval=0` to disable the new heartbeat.
This mitigates the observed idle connection closure: the live editor lost changes after simultaneous `ERR_CONNECTION_CLOSED` errors; observing periodic health events kept the same draft intact for over three minutes, and closing that watch restored the failure. The exact network component closing the connection was not identified. The new metadata heartbeat was tested with the original editor; these tests are not a deployment of v4.1.7 to the live cluster.
Actual network outages, browser reloads and session expiry can still discard unsaved changes in hosted Omni. The original Machine Class editor reloads its form when the watched class arrives again.
The full-page diagnostic in `hack/test/omni-ui/machine-class-reconnect.spec.ts` reproduces an unsaved Cores change reverting from 8 to 2 when the class stream reconnects with the identical saved resource/version, without any provider schema update. This is a reproduction of the remaining defect, not a fix. The provider cannot access or persist the browser's unsaved draft.
Save the class before reloading, or edit an exported manifest locally with `omnictl`; the provider does not silently rewrite Machine Classes.
See [EDITOR_FIXES.md](EDITOR_FIXES.md) for the issue-by-issue scope and verification.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--schema-mode` | `basic` | Compatibility flag; both values show the same form |
| `--schema-refresh-interval` | `0` | Accepted for old deployments; ignored. Schema is published only at startup |
| `--editor-keepalive-interval` | `30s` | Update a provider metadata timestamp to keep idle editor connections alive; `0` disables |
| `--machineset-sync-interval` | `0` | Opt in to existing-VM updates, for example `30s` |
| `--id` | `proxmox` | Infrastructure provider ID |
| `--config-file` | required | Proxmox connection settings |

Existing-VM reconciliation reads **MachineRequest provider data**.
Editing a Machine Class is not a guarantee that Omni will change already-created MachineRequests.
When explicitly enabled, the reconciler can apply supported host configuration changes and grow disks; CPU/memory changes can remain pending until a VM reboot.
It never shrinks disks.
Guest networking, installation media/extensions, firmware, and Talos volume changes require reprovisioning and are rejected when an applied configuration snapshot is available.
The first observation of a legacy VM records a baseline without changing its hardware.
Review any requested secondary-disk removal: reconciliation detaches removed data disks.

## Advanced examples

The following snippets are **provider data**, not complete Machine Class manifests.

### Data disks and QoS

```yaml
scsihw: virtio-scsi-single
disks:
  - storage: local-lvm
    size: 40
  - storage: nvme-pool
    size: 500
    volume_name: ceph-data
    iops_read: 10000
    mbps_write: 500
    backup: false
    replicate: false
```

A data disk's `volume_name` creates a Talos `UserVolumeConfig` mounted at `/var/mnt/<name>`, matching that disk's serial.
Names use lowercase letters, digits and hyphens; names producing the same serial are rejected.
Set firmware, HA, disk tuning and Talos extensions explicitly. The QEMU guest-agent extension is always included.

### Static addresses and pools

```yaml
nics:
  - bridge: vmbr0
    ip_pool: 10.10.100.50-10.10.100.99/24
    gateway4: 10.10.100.1
    mtu: 9000
```

Use a dedicated range excluding gateways, DHCP ranges, and manually assigned IPs.
Pool reservations are persisted in Omni before the VM starts and restored after a provider restart.
Overlapping pools share reservations within a provider process; separate provider IDs and external IPAM systems do not coordinate leases.
Subnet notation is also accepted, with a bounded candidate window; explicit ranges make the intended allocation limits clearer.

### HA, replication and hardware mappings

```yaml
ha:
  state: started
  resource_affinity: negative
  node_affinity_nodes: [pve1, pve2]
replication:
  enabled: true
  target_node: pve2
  schedule: '*/15'
pci_devices:
  - mapping: gpu-1
    pcie: true
    rombar: false
usb_devices:
  - mapping: zigbee-controller
    usb3: true
```

HA affinity rules require Proxmox VE 9+.
Storage replication requires suitable ZFS storage and another eligible node.
Provisioning requires one candidate node that provides every selected PCI/USB device or mapping.
Use `bios: ovmf` when requesting `secure_boot: true`.

### Clone a template

```yaml
provisioning_mode: clone
template_vmid: '9000'
full_clone: true
disks:
  - storage: local-lvm
    size: 40
```

The template must already contain the intended Talos version, firmware, and extensions, and must support NoCloud configuration.
The configured boot bus must match the template.
Disk resizing grows the boot disk when needed; retries preserve existing disk volumes.
Selecting `iso` or clearing Provisioning Mode ignores saved template/clone settings without deleting them. For backward compatibility, YAML that omits `provisioning_mode` entirely and specifies `template_vmid` still clones. Changing the effective mode on an existing VM requires reprovisioning.

## Build and verify

Install the Go version required by `go.mod`, then run:

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o ./_out/omni-infra-provider-proxmox ./cmd/omni-infra-provider-proxmox
```

Run a source build with the same environment variables and `--config-file config.yaml`.
Cross-compile using `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` or `GOARCH=arm64`.
The fork's container workflow now runs verification before publishing; main builds use `edge` and stable release tags can update `latest`.

The full integration suite requires Linux 6.8+, `/dev/kvm`, privileged Docker 26+, and a staging image-factory token in `IMAGE_FACTORY_ENTERPRISE_STAGING_TOKEN`.
Run `sudo -E make run-integration-test` on a disposable test host.
See `hack/test/integration.sh` for the production-factory alternative and teardown behavior.
