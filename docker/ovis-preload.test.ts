import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const directories: string[] = [];
afterEach(() => {
  for (const directory of directories.splice(0)) rmSync(directory, { recursive: true, force: true });
});

function fixture(source?: string) {
  const directory = mkdtempSync(join(tmpdir(), "ovis-preload-"));
  directories.push(directory);
  mkdirSync(join(directory, "server"));
  const file = join(directory, "server/updater.ts");
  writeFileSync(file, source ?? `const KILL_GRACE_MS = 2_000;
export function runCommand(cwd: string, argv: string[], signal?: AbortSignal, timeout = 30_000, grace = KILL_GRACE_MS): Promise<string> {
  return Promise.resolve(String(timeout));
}
`);
  return file;
}

function run(file: string, code: string, grandchild = false) {
  const script = `const { runCommand } = await import(${JSON.stringify(file)}); ${code}`;
  return Bun.spawnSync([process.execPath, "-e", grandchild
    ? `const child = Bun.spawnSync([process.execPath, "-e", ${JSON.stringify(script)}]); process.stdout.write(child.stdout); process.stderr.write(child.stderr); process.exit(child.exitCode);`
    : script], {
    env: { ...process.env, BUN_OPTIONS: `--preload=${join(import.meta.dir, "ovis-preload.ts")}` },
    stdout: "pipe", stderr: "pipe",
  });
}

test("extends only Git timeouts, never shortens explicit longer bounds, and leaves the checkout unchanged", () => {
  const file = fixture();
  const before = readFileSync(file, "utf8");
  const result = run(file, `console.log(JSON.stringify(await Promise.all([
    runCommand(".", ["git", "checkout"]),
    runCommand(".", ["git", "clone"], undefined, 900_000),
    runCommand(".", ["bun", "run", "build"], undefined, 120_000),
    runCommand(".", ["node"]),
  ])));`);
  expect(result.exitCode).toBe(0);
  expect(JSON.parse(result.stdout.toString())).toEqual(["600000", "900000", "120000", "30000"]);
  expect(readFileSync(file, "utf8")).toBe(before);
});

test("the hook is inherited by new release supervisors without a source patch", () => {
  const result = run(fixture(), 'console.log(await runCommand(".", ["git", "fetch"]));', true);
  expect(result.exitCode).toBe(0);
  expect(result.stdout.toString().trim()).toBe("600000");
});

test("an incompatible upstream updater fails explicitly instead of silently losing the fix", () => {
  const result = run(fixture("export const runCommand = () => 30000;"), "console.log(runCommand());");
  expect(result.exitCode).not.toBe(0);
  expect(result.stderr.toString()).toContain("Ovis updater changed");
});
