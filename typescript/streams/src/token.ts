import type { BlockRef } from "./types";

export function hex(b: Uint8Array): string {
  return `0x${Buffer.from(b.buffer, b.byteOffset, b.byteLength).toString("hex")}`;
}

export function bytes(h: string): Buffer {
  const s = h.startsWith("0x") || h.startsWith("0X") ? h.slice(2) : h;
  if (s.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(s)) {
    throw new TypeError(`not hex: ${h}`);
  }
  return Buffer.from(s, "hex");
}

/** The resume token `<numberHex>:<hash>` (spec §3.5). */
export function formatToken(ref: BlockRef): string {
  return `0x${ref.number.toString(16)}:${ref.hash}`;
}

export function parseToken(token: string): BlockRef {
  const m = /^0x([0-9a-fA-F]+):(0x[0-9a-fA-F]{64})$/.exec(token.trim());
  if (!m) throw new TypeError(`malformed cursor token: ${token}`);
  return { number: BigInt(`0x${m[1]}`), hash: m[2].toLowerCase() };
}
