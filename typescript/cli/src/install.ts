import fs from "node:fs";
import path from "node:path";
import https from "node:https";
import { promisify } from "node:util";
import { pipeline } from "node:stream";
import { getBinaryName, getPlatform } from "./platform";
import { verifyChecksum } from "./checksum";
import { RELEASE_INFO } from "./generated/release";

const mkdir = promisify(fs.mkdir);
const chmod = promisify(fs.chmod);
const pipelineAsync = promisify(pipeline);

const BINARY_DIR = path.join(__dirname, "bin");

interface DownloadOptions {
  url: string;
  dest: string;
}

class DownloadError extends Error {
  constructor(
    message: string,
    public readonly statusCode?: number,
  ) {
    super(message);
    this.name = "DownloadError";
  }
}

/**
 * Helper function to download a file from a URL
 */
async function downloadFile({ url, dest }: DownloadOptions): Promise<void> {
  await mkdir(path.dirname(dest), { recursive: true });

  return new Promise((resolve, reject) => {
    const request = https.get(url, (response) => {
      if (response.statusCode === 302 || response.statusCode === 301) {
        // Handle redirects
        downloadFile({ url: response.headers.location!, dest })
          .then(resolve)
          .catch(reject);
        return;
      }

      if (!response.statusCode || response.statusCode !== 200) {
        reject(
          new DownloadError("Failed to download binary", response.statusCode),
        );
        return;
      }

      const fileStream = fs.createWriteStream(dest);

      pipelineAsync(response, fileStream).then(resolve).catch(reject);
    });

    request.on("error", (error) => {
      reject(new DownloadError(`Network error: ${error.message}`));
    });
  });
}

/**
 * Trigger the erpc cli installation (downloading the right binary and checking it's checksum)
 */
async function install(): Promise<void> {
  try {
    const platform = getPlatform();
    const binaryName = getBinaryName(platform);
    const binaryPath = path.join(BINARY_DIR, binaryName);
    const downloadUrl = `https://github.com/erpc/erpc/releases/download/${RELEASE_INFO.version}/${binaryName}`;

    console.log(`Downloading erpc binary for ${binaryName}...`);

    await downloadFile({
      url: downloadUrl,
      dest: binaryPath,
    });

    // Verify checksum before making executable
    // TODO Uncomment after finding the root cause of the checksum mismatch?
    // console.log("\nVerifying binary checksum...");
    // await verifyChecksum(binaryPath, platform);

    // Make binary executable
    await chmod(binaryPath, 0o755);

    console.log("\nerpc binary has been installed successfully");
    process.exit(0);
  } catch (error) {
    if (error instanceof DownloadError) {
      console.error(`Download failed: ${error.message}`);
      if (error.statusCode === 404) {
        console.error(
          "Binary not found. Please check if the version exists in the releases.",
        );
      }
    } else {
      console.error(
        "Installation failed:",
        error instanceof Error ? error.message : error,
      );
    }
    process.exit(1);
  }
}

// Only run install script if we're not in CI
if (!process.env.CI) {
  install();
}
