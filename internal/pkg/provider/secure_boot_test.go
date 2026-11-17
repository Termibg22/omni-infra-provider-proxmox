// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider //nolint:testpackage // Exercise the real provisioning steps and SDK media resolution together.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/image-factory/pkg/schematic"
	"github.com/siderolabs/omni/client/pkg/imagefactory"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

// TestUEFIProvisioningMatchesTalosMedia ensures that fixing EFI key enrollment
// does not silently replace the requested Secure Boot ISO with an unsigned one.
func TestUEFIProvisioningMatchesTalosMedia(t *testing.T) {
	isoNames := map[bool]string{}

	for _, secureBoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("secure_boot=%t", secureBoot), func(t *testing.T) {
			const taskID = "UPID:pve1:00000001:00000001:00000001:download:100:root@pam:"

			var downloaded map[string]string

			var created map[string]any

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/nodes/pve1/storage/local/download-url":
					require.NoError(t, json.NewDecoder(r.Body).Decode(&downloaded))
					respond(t, w, taskID)
				case r.Method == http.MethodPost && r.URL.Path == "/nodes/pve1/qemu":
					require.NoError(t, json.NewDecoder(r.Body).Decode(&created))
					respond(t, w, taskID)
				case r.Method != http.MethodGet:
					t.Errorf("unexpected mutation: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				case r.URL.Path == "/nodes/pve1/status" || r.URL.Path == "/nodes/pve1/storage/local/status":
					respond(t, w, map[string]any{})
				case r.URL.Path == "/cluster/status":
					respond(t, w, []any{})
				case r.URL.Path == "/cluster/nextid":
					respond(t, w, "100")
				case r.URL.Path == "/nodes/pve1/storage":
					respond(t, w, []map[string]any{{"storage": "local-zfs", "content": "images", "active": 1, "enabled": 1}})
				case strings.HasPrefix(r.URL.Path, "/nodes/pve1/tasks/"):
					respond(t, w, map[string]any{"status": "stopped", "exitstatus": "OK"})
				case strings.HasPrefix(r.URL.Path, "/nodes/pve1/storage/local/content/local:iso/"):
					if downloaded == nil {
						http.NotFound(w, r)

						return
					}

					require.Equal(t, "/nodes/pve1/storage/local/content/local:iso/"+downloaded["filename"], r.URL.Path)
					respond(t, w, map[string]any{})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			st := state.WrapCore(inmem.NewState())
			require.NoError(t, st.Create(t.Context(), omni.NewFeaturesConfig(omni.FeaturesConfigID)))

			machine := resources.NewMachine("default", "uefi-worker")
			machine.TypedSpec().Value.Node = "pve1"
			req := infra.NewMachineRequest(machine.Metadata().ID())
			req.TypedSpec().Value.TalosVersion = "1.13.0"
			req.TypedSpec().Value.ProviderData = fmt.Sprintf(`bios: ovmf
machine: q35
secure_boot: %t
iso_storage: local
storage: local-zfs
additional_disks:
  - disk_size: 100
`, secureBoot)

			resolved := false
			pctx := provision.NewContext(req, infra.NewMachineRequestStatus(machine.Metadata().ID()), machine, provision.ConnectionParams{}, nil,
				func(ctx context.Context, version string, schematic schematic.Schematic, spec provision.MediaSpec) (imagefactory.InstallationMedia, error) {
					resolved = true

					require.Equal(t, secureBoot, spec.SecureBoot)
					require.Equal(t, "nocloud", spec.Platform)
					require.Equal(t, imagefactory.InstallationMediaKindISO, spec.Kind)
					require.True(t, spec.StandaloneURL)
					require.Contains(t, schematic.Customization.SystemExtensions.OfficialExtensions, "siderolabs/qemu-guest-agent")

					id, err := schematic.ID()
					require.NoError(t, err)

					return imagefactory.ResolveInstallationMedia(ctx, st, version, spec.MediaSpec, id, spec.StandaloneURL)
				})

			p := NewProvisioner(proxmox.NewClient(server.URL))
			for _, step := range p.ProvisionSteps() {
				if step.Name() != "uploadISO" && step.Name() != "syncVM" {
					continue
				}

				var retry *controller.RequeueError
				require.ErrorAs(t, step.Run(t.Context(), zap.NewNop(), pctx), &retry)
				require.NoError(t, step.Run(t.Context(), zap.NewNop(), pctx), "the completed task must allow provisioning to continue")
			}

			require.True(t, resolved)

			wantFile := "nocloud-amd64.iso"
			if secureBoot {
				wantFile = "nocloud-amd64-secureboot.iso"
			}

			require.True(t, strings.HasSuffix(downloaded["url"], "/v1.13.0/"+wantFile), downloaded["url"])
			require.Equal(t, machine.TypedSpec().Value.VolumeId, downloaded["filename"])
			require.Equal(t, "local:iso/"+downloaded["filename"], created["cdrom"])
			require.Equal(t, "ovmf", created["bios"])
			require.Equal(t, "local-zfs:1,efitype=4m,pre-enrolled-keys=0", created["efidisk0"], "Talos must enroll its own key into a fresh EFI disk")
			require.Contains(t, created["scsi0"], "local-zfs:20")
			require.Contains(t, created["scsi1"], "local-zfs:100")

			isoNames[secureBoot] = downloaded["filename"]
		})
	}

	require.NotEqual(t, isoNames[false], isoNames[true], "signed and unsigned ISOs must never share a cached volume")
}
