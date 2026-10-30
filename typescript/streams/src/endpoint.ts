import {
  type ChannelCredentials,
  type ChannelOptions,
  credentials,
  Metadata,
} from "@grpc/grpc-js";
import { StreamServiceClient } from "./gen/stream";

/**
 * The stream window (spec §6.3). grpc-js has one knob for the stream and
 * the connection window, and each endpoint gets its own connection with
 * one stream, so the stream's 4 MiB covers both.
 */
const flowControlWindow = 4 << 20;

export interface Endpoint {
  url: string;
  address: string;
  credentials: ChannelCredentials;
  /** Routing metadata as the server reads it (`extractRequestInput`). */
  route: Record<string, string>;
}

/** Parses `grpc(s)://host:port/<project>/<architecture>/<chainId>`. */
export function parseEndpoint(url: string): Endpoint {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    throw new TypeError(`endpoint ${url}: not a URL`);
  }
  const tls = u.protocol === "grpcs:";
  if (!tls && u.protocol !== "grpc:") {
    throw new TypeError(`endpoint ${url}: scheme must be grpc or grpcs`);
  }
  const port = u.port || (tls ? "443" : "");
  if (!u.hostname || !port) {
    throw new TypeError(`endpoint ${url}: host and port required`);
  }
  const parts = u.pathname.split("/").filter((p) => p !== "");
  if (parts.length !== 3) {
    throw new TypeError(
      `endpoint ${url}: path must be /<project>/<architecture>/<chainId>`,
    );
  }
  const [project, architecture, chainId] = parts.map(decodeURIComponent);
  return {
    url,
    address: `${u.hostname}:${port}`,
    credentials: tls
      ? credentials.createSsl()
      : credentials.createInsecure(),
    route: {
      "x-erpc-project": project,
      "x-erpc-architecture": architecture,
      "x-erpc-chain-id": chainId,
    },
  };
}

/** A fresh channel: a reconnect never reuses a connection that went silent. */
export function dial(e: Endpoint): StreamServiceClient {
  const options: ChannelOptions = {
    "grpc-node.flow_control_window": flowControlWindow,
  };
  return new StreamServiceClient(e.address, e.credentials, options);
}

export function metadataOf(
  e: Endpoint,
  extra: Record<string, string> | undefined,
): Metadata {
  const md = new Metadata();
  for (const [k, v] of Object.entries({ ...extra, ...e.route })) md.set(k, v);
  return md;
}
