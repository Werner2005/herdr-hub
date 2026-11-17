import test from "node:test";
import assert from "node:assert/strict";
import { privateConfigurations, initialConfigurationMapping } from "./configuration-selection.js";

test("creation offers only ready private configurations owned by the selected owner", () => {
 const items = [
  { name: "own", owner: "Alice", ready: true, global: false },
  { name: "other", owner: "Bob", ready: true, global: false },
  { name: "broken", owner: "Alice", ready: false, global: false },
  { name: "global", owner: "Alice", ready: true, global: true },
 ];
 assert.deepEqual(privateConfigurations(items, "Alice").map(item => item.name), ["own"]);
 assert.deepEqual(privateConfigurations(items, "Bob").map(item => item.name), ["other"]);
 assert.deepEqual(privateConfigurations(items, "alice"), []);
});

test("creation copies suggested file and environment mappings without mutating the catalog", () => {
 const item = { name: "tool", kind: "secret", keys: ["token"], mapping: { files: [{ key: "token", path: "/home/dev/.config/tool/token" }], env: [{ key: "token", name: "TOOL_TOKEN" }] } };
 const mapping = initialConfigurationMapping(item);
 assert.deepEqual(mapping, { configuration: "tool", files: item.mapping.files, env: item.mapping.env });
 mapping.files[0].path = "/mnt/secrets/token";
 mapping.env[0].name = "OTHER_TOKEN";
 assert.equal(item.mapping.files[0].path, "/home/dev/.config/tool/token");
 assert.equal(item.mapping.env[0].name, "TOOL_TOKEN");
});

test("entries without suggestions start with a safe, editable file mapping", () => {
 assert.deepEqual(initialConfigurationMapping({ name: "settings", kind: "configmap", keys: ["config.json"] }), { configuration: "settings", files: [{ key: "config.json", path: "/mnt/config/settings/config.json" }], env: [] });
 assert.deepEqual(initialConfigurationMapping({ name: "tool", kind: "secret", keys: ["token"], mapping: { files: [], env: [] } }), { configuration: "tool", files: [{ key: "token", path: "/mnt/secrets/tool/token" }], env: [] });
});

test("environment-only suggestions remain environment-only", () => {
 const item = { name: "tool", kind: "secret", keys: ["token"], mapping: { env: [{ key: "token", name: "TOOL_TOKEN" }] } };
 assert.deepEqual(initialConfigurationMapping(item), { configuration: "tool", files: [], env: [{ key: "token", name: "TOOL_TOKEN" }] });
});
