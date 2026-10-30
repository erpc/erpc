import type { Block, BlockRef } from "./types";

/** Refs of the merged chain kept: far deeper than any reorg window. */
const mergedRefs = 4096;
/** Blocks of each connection's branch kept. */
const laneBlocks = 256;

interface Entry {
  number: bigint;
  hash: string;
  /** Held only while the app has not applied this block. */
  block?: Block;
}

/** One endpoint's connection as the merge sees it (spec §9.5). */
export class Lane {
  /** Its branch this session, oldest first, contiguous. */
  chain: Entry[] = [];
  /** Highest number it delivered this session; an Undo does not lower it. */
  tip: bigint | undefined;
  /** It sent an Undo and no block since. */
  undone = false;

  constructor(
    readonly endpoint: string,
    /** Ends its stream; the run loop resumes it from the merged cursor. */
    readonly restart: () => void,
  ) {}
}

export interface Apply {
  apply(block: Block): Promise<void>;
  undo(forkPoint: BlockRef): Promise<void>;
}

/**
 * The chain the app applied, merged from one or two connections: it
 * follows the connection with the highest tip (ties keep the current one),
 * undoes to the fork point when it switches branch, and skips blocks it
 * already applied.
 */
export class Merger {
  readonly lanes: Lane[] = [];
  private merged: BlockRef[];
  private leader: Lane | undefined;

  constructor(
    cursor: BlockRef | undefined,
    private readonly app: Apply,
  ) {
    this.merged = cursor ? [cursor] : [];
  }

  /** The merged cursor: every connection resumes after it. */
  get cursor(): BlockRef | undefined {
    return this.merged.at(-1);
  }

  connected(lane: Lane): void {
    const c = this.cursor;
    lane.chain = c ? [{ number: c.number, hash: c.hash }] : [];
    lane.tip = c?.number;
    lane.undone = false;
  }

  async block(lane: Lane, b: Block): Promise<void> {
    let i = lane.chain.length;
    while (i > 0 && lane.chain[i - 1].number >= b.number) i--;
    lane.chain.length = i;
    const applied = this.at(b.number)?.hash === b.hash;
    lane.chain.push({ number: b.number, hash: b.hash, block: applied ? undefined : b });
    if (lane.chain.length > laneBlocks) lane.chain.splice(0, lane.chain.length - laneBlocks);
    if (lane.tip === undefined || b.number > lane.tip) lane.tip = b.number;
    lane.undone = false;
    await this.follow();
  }

  async undo(lane: Lane, fp: BlockRef): Promise<void> {
    let i = lane.chain.length;
    while (i > 0 && lane.chain[i - 1].number > fp.number) i--;
    lane.chain.length = i;
    const last = lane.chain.at(-1);
    if (last && last.number === fp.number && last.hash !== fp.hash) lane.chain.pop();
    if (lane.chain.at(-1)?.number !== fp.number) {
      lane.chain.push({ number: fp.number, hash: fp.hash });
    }
    lane.undone = true;
    await this.follow();
  }

  private at(n: bigint): BlockRef | undefined {
    const first = this.merged[0];
    if (!first || n < first.number) return undefined;
    const r = this.merged[Number(n - first.number)];
    return r?.number === n ? r : undefined;
  }

  private async follow(): Promise<void> {
    for (const l of this.lanes) {
      if (l.tip === undefined) continue;
      if (!this.leader || this.leader.tip === undefined || l.tip > this.leader.tip) {
        this.leader = l;
      }
    }
    const lane = this.leader;
    const last = lane?.chain.at(-1);
    if (!lane || !last) return;
    const top = this.cursor;
    if (top && this.at(last.number)?.hash === last.hash) {
      // Behind or level, unless its Undo says the merged top left its chain.
      if (lane.undone && last.number < top.number) await this.undoTo(last);
      lane.undone = false;
      return;
    }
    let fork: Entry | undefined;
    if (top) {
      for (let i = lane.chain.length - 1; i >= 0 && !fork; i--) {
        if (this.at(lane.chain[i].number)?.hash === lane.chain[i].hash) fork = lane.chain[i];
      }
      if (!fork) {
        const base = lane.chain[0];
        if (base.block || base.number >= this.merged[0].number) {
          lane.restart();
          return;
        }
        // The server placed the fork below every ref kept: trust it.
        fork = base;
      }
      if (fork.number < top.number) await this.undoTo(fork);
    }
    for (const e of lane.chain) {
      if (fork && e.number <= fork.number) continue;
      if (!e.block) continue;
      await this.app.apply(e.block);
      this.push({ number: e.number, hash: e.hash });
      e.block = undefined;
    }
    lane.undone = false;
  }

  private async undoTo(fp: BlockRef): Promise<void> {
    await this.app.undo({ number: fp.number, hash: fp.hash });
    const first = this.merged[0];
    if (first && fp.number >= first.number) {
      this.merged.length = Number(fp.number - first.number) + 1;
    } else {
      this.merged = [{ number: fp.number, hash: fp.hash }];
    }
  }

  private push(r: BlockRef): void {
    this.merged.push(r);
    if (this.merged.length > mergedRefs) this.merged.splice(0, this.merged.length - mergedRefs);
  }
}

/** Pending transactions both connections deliver, applied once by hash. */
export class PendingDedup {
  /** Insertion-ordered, so the oldest goes first past the bound. */
  private seen = new Set<string>();

  /** b without what was applied before; undefined when nothing is new. */
  fresh(b: Block): Block | undefined {
    const hashes = b.hashes.filter((h) => this.add(h));
    const transactions = b.transactions.filter((t) => {
      const h = `0x${Buffer.from(t.hash).toString("hex")}`;
      return this.add(h);
    });
    if (hashes.length === 0 && transactions.length === 0) return undefined;
    return { ...b, hashes, transactions };
  }

  private add(h: string): boolean {
    if (this.seen.has(h)) return false;
    this.seen.add(h);
    if (this.seen.size > 10_000) this.seen.delete(this.seen.values().next().value!);
    return true;
  }
}
