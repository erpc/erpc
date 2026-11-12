import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const MESSAGE =
  "If eRPC helps you, a star on https://github.com/erpc/erpc helps others find it. (hide: ERPC_NOSTAR=1)";

/**
 * Print a one-line GitHub star request to stderr, once per machine.
 *
 * Skipped when ERPC_NOSTAR=1, in CI, or when stderr is not a TTY.
 * The line is printed only if this call created the marker file, so a
 * marker that cannot be written means no line, never a failed start.
 */
export function maybePrintStarPrompt(): void {
  if (
    process.env.ERPC_NOSTAR === "1" ||
    process.env.CI ||
    !process.stderr.isTTY
  ) {
    return;
  }
  try {
    const configHome =
      process.env.XDG_CONFIG_HOME || path.join(os.homedir(), ".config");
    const dir = path.join(configHome, "erpc");
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, "star-prompt-shown"), "", { flag: "wx" });
  } catch {
    return;
  }
  process.stderr.write(`${MESSAGE}\n`);
}
