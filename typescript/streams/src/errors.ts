import { BinaryReader } from "@bufbuild/protobuf/wire";
import type { ServiceError } from "@grpc/grpc-js";
import type { BlockRef } from "./types";

/**
 * The server cannot continue from the cursor (spec §3.4 `out_of_range`):
 * the app was away longer than the window. Neither eRPC nor the library
 * backfills; `oldest` is the oldest block a stream can resume from.
 */
export class OutOfRangeError extends Error {
  constructor(
    readonly cursor: string | undefined,
    readonly reason: string,
    readonly oldest: BlockRef | undefined,
  ) {
    super(
      `out of range (${reason}): cursor ${cursor ?? "none"}, oldest ${oldest ? `0x${oldest.number.toString(16)}:${oldest.hash}` : "unknown"}`,
    );
    this.name = "OutOfRangeError";
  }
}

/** A reorg reverted a finalized block the app applied (`final_reverted`). */
export class FinalRevertedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "FinalRevertedError";
  }
}

const errorInfoType = "type.googleapis.com/google.rpc.ErrorInfo";

/**
 * The `google.rpc.ErrorInfo` in a status's `grpc-status-details-bin`:
 * reason and metadata, or undefined when the status carries none.
 */
export function errorInfo(
  err: ServiceError,
): { reason: string; metadata: Record<string, string> } | undefined {
  const raw = err.metadata?.get("grpc-status-details-bin")[0];
  if (!(raw instanceof Uint8Array)) return undefined;
  for (const any of fields(raw, 3)) {
    let typeUrl = "";
    let value: Uint8Array | undefined;
    const r = new BinaryReader(any);
    while (r.pos < r.len) {
      const [no, wt] = r.tag();
      if (no === 1) typeUrl = r.string();
      else if (no === 2) value = r.bytes();
      else r.skip(wt);
    }
    if (typeUrl !== errorInfoType || !value) continue;
    const info = { reason: "", metadata: {} as Record<string, string> };
    const ir = new BinaryReader(value);
    while (ir.pos < ir.len) {
      const [no, wt] = ir.tag();
      if (no === 1) info.reason = ir.string();
      else if (no === 3) {
        const er = new BinaryReader(ir.bytes());
        let k = "";
        let v = "";
        while (er.pos < er.len) {
          const [eno, ewt] = er.tag();
          if (eno === 1) k = er.string();
          else if (eno === 2) v = er.string();
          else er.skip(ewt);
        }
        info.metadata[k] = v;
      } else ir.skip(wt);
    }
    return info;
  }
  return undefined;
}

/** The length-delimited fields numbered no of message buf. */
function fields(buf: Uint8Array, no: number): Uint8Array[] {
  const out: Uint8Array[] = [];
  const r = new BinaryReader(buf);
  while (r.pos < r.len) {
    const [n, wt] = r.tag();
    if (n === no) out.push(r.bytes());
    else r.skip(wt);
  }
  return out;
}
