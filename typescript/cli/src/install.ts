import fs from 'node:fs';
import path from 'node:path';
import https from 'node:https';
import { promisify } from 'node:util';
import { pipeline } from 'node:stream';
import { getBinaryName } from './platform';

const mkdir = promisify(fs.mkdir);
const chmod = promisify(fs.chmod);
const pipelineAsync = promisify(pipeline);

const BINARY_DIR = path.join(__dirname, 'bin');
const VERSION = process.env.npm_package_version;

interface DownloadOptions {
  url: string;
  dest: string;
}

class DownloadError extends Error {
  constructor(
    message: string,
    public readonly statusCode?: number
  ) {
    super(message);
    this.name = 'DownloadError';
  }
}

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
        reject(new DownloadError("Failed to download binary", response.statusCode));
        return;
      }

      const totalSize = Number.parseInt(response.headers['content-length'] || '0', 10);
      let downloadedSize = 0;

      const fileStream = fs.createWriteStream(dest);
      
      response.on('data', (chunk: Buffer) => {
        downloadedSize += chunk.length;
      });

      pipelineAsync(response, fileStream)
        .then(resolve)
        .catch(reject);
    });

    request.on('error', (error) => {
      reject(new DownloadError(`Network error: ${error.message}`));
    });
  });
}

async function install(): Promise<void> {
  try {
    const binaryName = getBinaryName();
    const binaryPath = path.join(BINARY_DIR, binaryName);
    const downloadUrl = `https://github.com/erpc/erpc/releases/download/${VERSION}/${binaryName}`;

    console.log(`Downloading erpc binary for ${binaryName}...`);
    
    await downloadFile({
      url: downloadUrl,
      dest: binaryPath,
    });

    // Make binary executable
    await chmod(binaryPath, 0o755);

    console.log('\nerpc binary has been installed successfully');
    process.exit(0);
  } catch (error) {
    if (error instanceof DownloadError) {
      console.error(`Download failed: ${error.message}`);
      if (error.statusCode === 404) {
        console.error('Binary not found. Please check if the version exists in the releases.');
      }
    } else {
      console.error('Installation failed:', error instanceof Error ? error.message : error);
    }
    process.exit(1);
  }
}

// Only run install script if we're not in CI
if (!process.env.CI) {
  install();
}