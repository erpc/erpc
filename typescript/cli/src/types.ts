// Binary platforms we support
export type SupportedPlatform =
  | "darwin_arm64"
  | "darwin_x86_64"
  | "linux_arm64"
  | "linux_x86_64"
  | "windows_x86_64";

// Release information
export interface ReleaseInfo {
  version: string;
  commitSha: string;
}

// Simple checksum mapping
export type Checksums = Record<SupportedPlatform, string>;
