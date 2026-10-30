import { readFile, rename, writeFile } from "node:fs/promises";
import type { Cursor } from "./types";

/**
 * A cursor in a file, replaced atomically on each save so a crash leaves
 * the previous token or the new one, never a torn write.
 */
export function fileCursor(path: string): Cursor {
  const tmp = `${path}.tmp`;
  return {
    async load() {
      try {
        const token = (await readFile(path, "utf8")).trim();
        return token === "" ? undefined : token;
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code === "ENOENT") return undefined;
        throw err;
      }
    },
    async save(token) {
      await writeFile(tmp, `${token}\n`);
      await rename(tmp, path);
    },
  };
}
