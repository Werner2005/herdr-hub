// Loaded only by Ovis via its plugin environment, including update supervisors.
// Transform in memory: editing updater.ts on disk would block clean-tree updates.
import { plugin } from "bun";
import { readFile } from "node:fs/promises";

const signature = "export function runCommand(cwd: string, argv: string[], signal?: AbortSignal, timeout = 30_000, grace = KILL_GRACE_MS): Promise<string> {";

plugin({
  name: "herdr-ovis-slow-storage",
  setup(build) {
    build.onLoad({ filter: /\/server\/updater\.ts$/ }, async ({ path }) => {
      const source = await readFile(path, "utf8");
      if (source.split(signature).length !== 2) {
        throw new Error("Ovis updater changed: review the Herdr slow-storage timeout compatibility hook before updating.");
      }
      return {
        loader: "ts",
        contents: source.replace(signature, signature + '\n  if (argv[0] === "git") timeout = Math.max(timeout, 600_000);'),
      };
    });
  },
});
