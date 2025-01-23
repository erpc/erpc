import fs from "node:fs";
import path from "node:path";
import type { Checksums, SupportedPlatform } from "../types";

/**
 * Get the filename from a given platform
 */
function getPlatformFromFilename(filename: string): SupportedPlatform {
  // Remove .exe if present
  const baseName = filename.replace(".exe", "");
  // Remove 'erpc_' prefix
  const platform = baseName.replace("erpc_", "") as SupportedPlatform;

  if (!isSupportedPlatform(platform)) {
    throw new Error(`Unsupported platform in filename: ${filename}`);
  }

  return platform;
}

/**
 * Check if the platform is supported
 */
function isSupportedPlatform(platform: string): platform is SupportedPlatform {
  const supportedPlatforms: SupportedPlatform[] = [
    "darwin_arm64",
    "darwin_x86_64",
    "linux_arm64",
    "linux_x86_64",
    "windows_x86_64",
  ];
  return supportedPlatforms.includes(platform as SupportedPlatform);
}

/**
 * Parse the checksums.txt file from goreleaser
 */
async function parseChecksums(filePath: string): Promise<Map<string, string>> {
  const content = await fs.promises.readFile(filePath, "utf8");
  const checksumMap = new Map<string, string>();

  // Parse lines like:
  // <checksum>  <filename>
  content.split("\n").forEach((line) => {
    const [checksum, filename] = line.trim().split(/\s+/);
    if (checksum && filename) {
      checksumMap.set(filename, checksum);
    }
  });

  return checksumMap;
}

/**
 * Core step to generate release files
 */
async function main() {
  const version = process.env.VERSION;
  const commitSha = process.env.COMMIT_SHA;
  const checksumsPath = process.env.CHECKSUMS_FILE;

  if (!version || !commitSha || !checksumsPath) {
    throw new Error(
      "VERSION and COMMIT_SHA and CHECKSUMS_FILE environment variables are required",
    );
  }

  // Read checksums from goreleaser output
  const checksumMap = await parseChecksums(checksumsPath);
  const checksums: Partial<Checksums> = {};

  // Convert the checksums to our format
  for (const [filename, checksum] of checksumMap.entries()) {
    if (filename.startsWith("erpc_")) {
      const platform = getPlatformFromFilename(filename);
      checksums[platform] = checksum;
    }
  }

  // Verify we have all platforms
  const missingPlatforms = Object.keys(checksums).length !== 5;
  if (missingPlatforms) {
    throw new Error("Missing checksums for some platforms");
  }

  // Generate checksums.ts
  const checksumsOutput = `// This file is auto-generated by CI. Do not edit manually.
import type { Checksums } from "../types";

export const CHECKSUMS: Checksums = ${JSON.stringify(checksums, null, 2)};
`;

  // Generate release.ts
  const releaseOutput = `// This file is auto-generated by CI. Do not edit manually.
import type { ReleaseInfo } from "../types";

export const RELEASE_INFO: ReleaseInfo = {
  version: '${version}',
  commitSha: '${commitSha}',
};
`;

  await fs.promises.writeFile("src/generated/checksums.ts", checksumsOutput);
  await fs.promises.writeFile("src/generated/release.ts", releaseOutput);

  console.log(
    "Successfully generated release files with checksums from goreleaser",
  );
}

main().catch((error) => {
  console.error("Failed to generate release files:", error);
  process.exit(1);
});
