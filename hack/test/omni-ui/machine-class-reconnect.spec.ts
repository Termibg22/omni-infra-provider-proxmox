// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Diagnostic tests for stock Omni, not a patch to its editor. These exercise
// MachineClass and its real watch/stream/form components, including reconnects.
import { createUpdatedEvent } from "@msw/helpers";
import { createWatchStreamMock } from "@msw/server";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/vue";
import { flushPromises } from "@vue/test-utils";
import { readFileSync } from "node:fs";
import { expect, test, vi } from "vitest";

import { ResourceService } from "@/api/grpc";
import { ManagementService } from "@/api/omni/management/management.pb";
import {
  DefaultNamespace,
  InfraProviderNamespace,
  InfraProviderStatusType,
  MachineClassType,
} from "@/api/resources";
import MachineClass from "@/views/MachineClasses/MachineClass.vue";

vi.mock("vue-router", async (original) => ({
  ...(await original<typeof import("vue-router")>()),
  useRouter: () => ({ push: vi.fn() }),
}));
vi.mock("@/notification", () => ({ showError: vi.fn() }));

vi.spyOn(ManagementService, "ValidateJSONSchema").mockResolvedValue({ errors: [] });

const schema = readFileSync(process.env.PROXMOX_PROVIDER_SCHEMA!, "utf8");

function editor() {
  const machineClass = {
    metadata: {
      id: "reconnect-test",
      type: MachineClassType,
      namespace: DefaultNamespace,
      version: "5",
    },
    spec: {
      auto_provision: {
        provider_id: "proxmox",
        provider_data: "cores: 2\nmemory: 4096\ndisk_size: 20\n",
      },
    },
  };
  const provider = {
    metadata: {
      id: "proxmox",
      type: InfraProviderStatusType,
      namespace: InfraProviderNamespace,
      version: "7",
    },
    spec: { name: "Proxmox", schema },
  };
  const classStream = createWatchStreamMock({
    expectedOptions: { type: MachineClassType, id: "reconnect-test" },
    initialResources: [machineClass],
  });
  createWatchStreamMock({
    expectedOptions: { type: InfraProviderStatusType },
    initialResources: [provider],
  });
  const providerStream = createWatchStreamMock({
    expectedOptions: { type: InfraProviderStatusType, id: "proxmox" },
    initialResources: [provider],
  });
  const rendered = render(MachineClass, {
    props: { machineClassEditId: "reconnect-test" },
    global: {
      stubs: {
        PageContainer: { template: "<main><slot/></main>" },
        PageHeader: true,
        ProviderConfig: true,
        TButtonGroup: true,
        Labels: true,
        MachineMatchItem: true,
      },
    },
  });

  return { ...rendered, classStream, providerStream, machineClass, provider };
}

function coresInput() {
  return document.getElementById("#/properties/cores-input")?.querySelector("input");
}

async function editCores() {
  await waitFor(() => expect(coresInput()?.value).toBe("2"));
  await fireEvent.update(coresInput()!, "8");
  await flushPromises();
  expect(coresInput()!.value).toBe("8");
}

test("provider status and schema updates preserve edits in the complete stock editor", async () => {
  const { providerStream, provider, unmount } = editor();
  await editCores();
  await providerStream.pushEvents(
    createUpdatedEvent({
      ...provider,
      metadata: { ...provider.metadata, version: "8" },
      spec: {
        ...provider.spec,
        name: "Proxmox refreshed",
        schema: JSON.stringify({ ...JSON.parse(schema), description: "Updated inventory" }),
      },
    }, provider),
  );
  await screen.findByText("Proxmox refreshed Provider Config");
  expect(coresInput()!.value).toBe("8");
  unmount();
});

test("repeated provider metadata heartbeats preserve the editor, focus and additional disk draft", async () => {
  const { providerStream, provider, unmount } = editor();
  await editCores();
  const disks = within(screen.getByText("Additional Disks", { exact: true }).parentElement!);
  await fireEvent.click(disks.getByRole("button", { name: "Add", exact: true }));
  await fireEvent.update(await disks.findByRole("spinbutton"), "64");
  await flushPromises();
  const input = coresInput()!;
  input.focus();

  let previous = provider;
  for (let i = 1; i <= 10; i++) {
    const current = {
      ...provider,
      metadata: {
        ...provider.metadata,
        version: String(7 + i),
        annotations: { "proxmox.omni.siderolabs.io/editor-heartbeat": String(i) },
      },
    };
    await providerStream.pushEvents(createUpdatedEvent(current, previous));
    await flushPromises();
    expect(coresInput()).toBe(input);
    expect(document.activeElement).toBe(input);
    expect(input.value).toBe("8");
    expect((disks.getByRole("spinbutton") as HTMLInputElement).value).toBe("64");
    previous = current;
  }
  // A visible final marker confirms that all preceding events on this stream
  // have been consumed, without exposing test hooks in the original editor.
  await providerStream.pushEvents(createUpdatedEvent({
    ...previous,
    spec: { ...provider.spec, name: "Proxmox heartbeat observed" },
  }, previous));
  await screen.findByText("Proxmox heartbeat observed Provider Config");
  expect(coresInput()).toBe(input);
  expect(document.activeElement).toBe(input);
  expect(input.value).toBe("8");
  expect((disks.getByRole("spinbutton") as HTMLInputElement).value).toBe("64");
  unmount();
});

test("stock Omni loses unsaved edits when the class stream reconnects with the same saved version", async () => {
  const { classStream, unmount } = editor();
  const update = vi.spyOn(ResourceService, "Update").mockResolvedValue({});
  const networkLog = vi.spyOn(console, "error").mockImplementation(() => {});

  try {
    await editCores();
    await classStream.closeStream(new Error("simulated network interruption"));
    // The real Stream reconnects and the mock server replays the identical
    // MachineClass (same metadata.version and provider_data), with no provider
    // status/schema event and no API write. Omni replaces the local draft.
    await waitFor(() => expect(coresInput()?.value).toBe("2"));
    expect(networkLog).toHaveBeenCalledWith(
      expect.stringContaining("MachineClasses.omni.sidero.dev"),
    );
    expect(update).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Update Machine Class" })).toBeDefined();
  } finally {
    unmount();
    update.mockRestore();
    networkLog.mockRestore();
  }
});
