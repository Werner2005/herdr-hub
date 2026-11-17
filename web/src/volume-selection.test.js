import test from "node:test";
import assert from "node:assert/strict";
import { attachableVolumes } from "./volume-selection.js";

test("attachment choices follow the workspace owner, not the viewing admin", () => {
 const volumes = [
  { name: "private", owner: "alice", accessible_owners: ["alice"] },
  { name: "shared", owner: "alice", accessible_owners: ["alice", "admin", "bob"] },
  { name: "admin-disk", owner: "admin", accessible_owners: ["admin"] },
 ];
 assert.deepEqual(attachableVolumes(volumes, { owner: "admin" }).map(v => v.name), ["shared", "admin-disk"]);
 assert.deepEqual(attachableVolumes(volumes, { owner: "bob" }).map(v => v.name), ["shared"]);
 assert.deepEqual(attachableVolumes(volumes, { owner: "alice", volumes: ["shared"] }).map(v => v.name), ["private"]);
 assert.deepEqual(attachableVolumes(volumes, { owner: "Alice" }), []);
});

test("owner access works without access metadata and unknown access fails closed", () => {
 const volumes = [{ name: "own", owner: "alice" }, { name: "other", owner: "bob" }];
 assert.deepEqual(attachableVolumes(volumes, { owner: "alice" }).map(v => v.name), ["own"]);
 assert.deepEqual(attachableVolumes(volumes, { owner: "admin" }), []);
});
