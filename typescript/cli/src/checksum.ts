import crypto from "node:crypto";
import fs from "node:fs";
import { CHECKSUMS } from "./generated/checksums";
import { RELEASE_INFO } from "./generated/release";
import type { SupportedPlatform } from "./types";

/**
 * Verify the checksum of a downled binary against the known checksum for the platform
 */
export async function verifyChecksum(
  filePath: string,
  platform: SupportedPlatform,
): Promise<void> {
  const expectedChecksum = CHECKSUMS[platform];

  if (!expectedChecksum) {
    throw new Error(
      `ChecksumVerificationError: No checksum found for platform: ${platform}`,
    );
  }

  const fileBuffer = await fs.promises.readFile(filePath);
  const hash = crypto.createHash("sha256");
  hash.update(fileBuffer);
  const actualChecksum = hash.digest("hex");

  if (actualChecksum !== expectedChecksum) {
    throw new Error(
      `ChecksumVerificationError: actualChecksum=${actualChecksum} expectedChecksum=${expectedChecksum}`,
    );
  }

  console.log(
    `Verified binary integrity for version ${RELEASE_INFO.version} (${RELEASE_INFO.commitSha})`,
  );
}
