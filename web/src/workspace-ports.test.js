import test from "node:test";
import assert from "node:assert/strict";
import { validPortDraft } from "./workspace-ports.js";

test("exports accept HTTP, TCP and UDP internal ports", () => {
 for (const protocol of ["HTTP", "TCP", "UDP"]) {
  assert.equal(validPortDraft("my-app", protocol, "3000"), true);
 }
});

test("exports reject invalid or built-in ports and invalid names", () => {
 for (const port of ["", "0", "65536", "1.5", "abc", "2222", "7317"]) {
  assert.equal(validPortDraft("app", "TCP", port), false);
 }
 for (const name of ["", "App", "bad/name", "a-", "a".repeat(41)]) {
  assert.equal(validPortDraft(name, "HTTP", "80"), false);
 }
 assert.equal(validPortDraft("app", "TPC", "80"), false);
});

test("exports reject duplicate names and enforce the per-workspace limit", () => {
 assert.equal(validPortDraft("app", "HTTP", "80", [{ name: "app" }]), false);
 assert.equal(validPortDraft("app", "HTTP", "80", Array.from({ length: 16 }, (_, i) => ({ name: `app-${i}` }))), false);
});
