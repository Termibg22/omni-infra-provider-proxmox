// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

import { fireEvent, render, screen, waitFor } from "@testing-library/vue";
import { expect, test, vi } from "vitest";
import { defineComponent, ref, unref } from "vue";
import { ManagementService } from "@/api/omni/management/management.pb";
import { readFileSync } from "node:fs";
import JsonForm from "@/components/Form/JsonForm.vue";
const schema = JSON.parse(
  readFileSync(process.env.PROXMOX_PROVIDER_SCHEMA!, "utf8"),
);
vi.spyOn(ManagementService, "ValidateJSONSchema").mockResolvedValue({
  errors: [],
});
function editor(input: Record<string, unknown>) {
  const model = ref(input);
  const jsonSchema = ref(JSON.stringify(schema));
  render(
    defineComponent({
      components: { JsonForm },
      setup: () => ({ model, jsonSchema }),
      template: '<JsonForm v-model="model" :json-schema="jsonSchema" />',
    }),
  );
  return { model, jsonSchema };
}
test("unmodified Omni 1.13.3 shows saved flat hardware without mutating values or inserting null", async () => {
  const source = {
    workload_profile: "storage-node",
    cores: 2,
    memory: 4096,
    disk_size: 20,
    storage_selector: 'name == "local-zfs"',
    network_bridge: "vmbr0",
    bios: "ovmf",
    disk_ssd: true,
    disk_iothread: true,
    balloon: true,
  };
  const { model } = editor(source);
  await waitFor(() =>
    expect(screen.getAllByRole("spinbutton").length).toBeGreaterThan(5),
  );
  const values = (screen.getAllByRole("spinbutton") as HTMLInputElement[]).map(
    (e) => e.value,
  );
  expect(values.slice(0, 3)).toEqual(["2", "4096", "20"]);
  expect(screen.getByDisplayValue('name == "local-zfs"')).toBeDefined();
  expect(
    screen
      .getAllByRole("combobox")
      .some((e) => e.textContent?.includes("storage-node")),
  ).toBe(true);
  expect(unref(model)).toEqual(source);
  expect(JSON.stringify(unref(model))).not.toContain("null");
});
test("unmodified Omni can mount a new form without manufacturing NaN/null or empty disk overrides", async () => {
  const { model } = editor({});
  await waitFor(() =>
    expect(screen.getAllByRole("spinbutton").length).toBeGreaterThan(5),
  );
  expect(JSON.stringify(unref(model))).not.toContain("null");
  expect(unref(model)).not.toHaveProperty("disks");
  expect(unref(model)).not.toHaveProperty("nics");
});
test("schema refresh keeps user edits in the unmodified form", async () => {
  const { model, jsonSchema } = editor({ cores: 2, memory: 4096 });
  await waitFor(() =>
    expect(screen.getAllByRole("spinbutton").length).toBeGreaterThan(5),
  );
  await fireEvent.update(screen.getAllByRole("spinbutton")[0], "8");
  await waitFor(() => expect(unref(model).cores).toBe(8));
  jsonSchema.value = JSON.stringify({
    ...schema,
    description: "Refreshed topology",
  });
  await waitFor(() =>
    expect(
      (screen.getAllByRole("spinbutton")[0] as HTMLInputElement).value,
    ).toBe("8"),
  );
  expect(unref(model).cores).toBe(8);
});
