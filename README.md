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
  ghcr.io/termibg22/omni-infra-provider-proxmox:v4.1.1 \
  --config-file /config.yaml
```

`v4.1.1` includes the reviewed usability and reliability changes described below.
Do not use `latest` to pin a production deployment.

## Create a Machine Class

In Omni, create an **Auto Provision** Machine Class and select the Proxmox provider.
For ordinary DHCP machines, the provider defaults to 2 cores, 4096 MB RAM, a 20 GB SCSI boot disk, and a VirtIO NIC on `vmbr0`.
Select a storage and bridge that exist on every eligible target node, or constrain placement with a node/group.

The provider data accepts grouped UI fields and legacy flat YAML:

```yaml
compute:
  cores: 4
  memory: 8192

disks:
  - storage: local-lvm
    size: 40

network:
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
      disks:
        - storage: local-lvm
          size: 40
```

Apply it with `omnictl apply -f machine-class.yaml`, then select the class when configuring a cluster's machine sets.

## Form detail and updates (v4.1.1)

The default form shows four sections: General, CPU & Memory, Disks, and Network.
Use `--schema-mode advanced` to expose firmware, cloning, HA, replication, firewall, bonding, passthrough, QoS, DNS, and static IP configuration.
Switching form modes preserves advanced fields already present in provider data.
The mode is a provider startup setting, shared by users of that provider.
Omni controls the form renderer; this repository cannot add client-side tabs or conditional dropdowns to it.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--schema-mode` | `basic` | `basic` or `advanced` form |
| `--schema-refresh-interval` | `2m` | Refresh discovered dropdown choices; `0` disables |
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
workload_profile: storage-node
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
Workload profiles supply defaults and extra Talos extensions; explicit YAML values, including `false`, take precedence.
Profiles are `standard`, `control-plane`, `storage-node`, `gpu-ai-worker`, and `hardened-secure`.
The control-plane profile enables Proxmox HA unless explicitly disabled with `ha: {state: none}`.

### Static addresses, pools and bonds

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

```yaml
nics:
  - bridge: vmbr1
  - bridge: vmbr2
bonds:
  - name: bond0
    interfaces: [net0, net1]
    mode: 802.3ad
    mtu: 9000
    addresses: [10.10.200.50/24]
    gateway4: 10.10.200.1
```

Configure addresses/routes on the bond, rather than its member NICs.
LACP requires compatible host/switch networking and Talos support for the supplied NoCloud configuration.

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
PCI/USB mappings must exist on every eligible node; provisioning fails if no candidate provides all requested mappings.
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
