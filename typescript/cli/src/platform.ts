import os from "node:os";

type NodeArch = "x64" | "arm64" | "ia32";
type NodePlatform = "darwin" | "win32" | "linux";
type SupportedPlatform =
  | "darwin_x86_64"
  | "darwin_arm64"
  | "linux_x86_64"
  | "linux_arm64"
  | "windows_x86_64";

/**
 * Compute the current platform
 */
export function getPlatform(): SupportedPlatform {
  const type = os.type();
  const arch = os.arch() as NodeArch;

  // Map Node's arch to our arch
  let architecture: "x86_64" | "arm64" | "i386" = "x86_64";
  if (arch === "arm64") {
    architecture = "arm64";
  } else if (arch === "ia32") {
    architecture = "i386";
  }

  // Map Node's platform to our platform
  if (type === "Darwin") {
    const platform = `darwin_${architecture}` as SupportedPlatform;
    if (!isSupportedPlatform(platform)) {
      throw new Error(`Unsupported Darwin platform: ${platform}`);
    }
    return platform;
  }
  if (type === "Windows_NT") {
    const platform = `windows_${architecture}` as SupportedPlatform;
    if (!isSupportedPlatform(platform)) {
      throw new Error(`Unsupported Windows platform: ${platform}`);
    }
    return platform;
  }
  if (type === "Linux") {
    const platform = `linux_${architecture}` as SupportedPlatform;
    if (!isSupportedPlatform(platform)) {
      throw new Error(`Unsupported Linux platform: ${platform}`);
    }
    return platform;
  }

  throw new Error(
    `Unsupported platform: ${type} ${arch}. Please file an issue at [your-repo-url]`,
  );
}

function isSupportedPlatform(platform: string): platform is SupportedPlatform {
  const supportedPlatforms: SupportedPlatform[] = [
    "darwin_x86_64",
    "darwin_arm64",
    "linux_x86_64",
    "linux_arm64",
    "windows_x86_64",
  ];
  return supportedPlatforms.includes(platform as SupportedPlatform);
}

/**
 * Get the right binary name for the current platform
 */
export function getBinaryName(platform?: SupportedPlatform): string {
  if (!platform) {
    platform = getPlatform();
  }
  const baseFileName = `erpc_${platform}`;
  return os.type() === "Windows_NT" ? `${baseFileName}.exe` : baseFileName;
}
