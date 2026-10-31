import { FinalRevertedError } from "./errors";
import type { Block, BlockRef } from "./types";

/** Refs of the applied chain kept to find fork points. */
const chainKeep = 4096;
/**
 * Blocks kept per connection, which a switch to it applies (spec §9.5). A
 * connection whose fork lies deeper restarts from the merged cursor, and
 * its server sends the undo.
 */
const branchKeep = 256;

const same = (a: BlockRef, b: BlockRef) => a.number === b.number && a.hash === b.hash;
const refOf = (b: BlockRef): BlockRef => ({ number: b.number, hash: b.hash });

/**
 * One endpoint's connection as the run sees it: what its current session
 * delivered, blocks linked from `base`, the block it resumed from or the
 * parent of its first.
 */
export class Lane {
  /** The current session; events of an older one are dropped. */
  sess = 0;
  /** Its stream is open and the library did not restart it. */
  live = false;
  base: BlockRef | undefined;
  blocks: Block[] = [];
  /** Highest number the session delivered: an undo does not lower it. */
  high = 0n;
  /**
   * The session's last word was an undo, which drops the chain above the
   * lane's top even before the new branch arrives; cleared by the follow
   * it triggers.
   */
  undone = false;
  /** `base` is a fork point the server named, by undo or resend. */
  trusted = false;
  /** Ends the current session's stream; the run resumes it. */
  cancel: () => void = () => {};

  constructor(readonly endpoint: string) {}

  get ready(): boolean {
    return this.live && this.base !== undefined;
  }

  /** Starts a session resuming from `from`; returns its id. */
  open(from: BlockRef | undefined): number {
    this.sess++;
    this.live = true;
    this.base = from && refOf(from);
    this.blocks = [];
    this.high = from?.number ?? 0n;
    this.undone = false;
    this.trusted = false;
    return this.sess;
  }

  /**
   * Appends b, dropping the blocks at and above its number: a resend after
   * a reorg names its new parent. False when b links to nothing the lane
   * holds.
   */
  add(b: Block): boolean {
    if (b.number === 0n) return false;
    const parent: BlockRef = { number: b.number - 1n, hash: b.parentHash };
    if (!this.base) {
      this.base = parent;
      this.blocks = [b];
    } else if (parent.number < this.base.number) {
      // A resend from below the resume cursor: an undo to the parent.
      this.base = parent;
      this.blocks = [b];
      this.trusted = true;
    } else if (same(parent, this.base)) {
      this.blocks = [b];
    } else if (parent.number === this.base.number) {
      return false;
    } else {
      const i = Number(parent.number - this.base.number - 1n);
      if (i >= this.blocks.length || !same(this.blocks[i], parent)) return false;
      this.blocks.length = i + 1;
      this.blocks.push(b);
    }
    if (b.number > this.high) this.high = b.number;
    this.undone = false;
    if (this.blocks.length > branchKeep) {
      const cut = this.blocks.length - branchKeep;
      this.base = refOf(this.blocks[cut - 1]);
      this.blocks = this.blocks.slice(cut);
      this.trusted = false;
    }
    return true;
  }

  /** Drops the blocks above fp. */
  undo(fp: BlockRef): void {
    this.undone = true;
    if (this.base && fp.number > this.base.number) {
      const i = Number(fp.number - this.base.number - 1n);
      if (i < this.blocks.length && same(this.blocks[i], fp)) {
        this.blocks.length = i + 1;
        return;
      }
    }
    if (this.base && same(this.base, fp)) {
      this.blocks = [];
      return;
    }
    this.base = refOf(fp);
    this.blocks = [];
    this.trusted = true;
  }
}

export interface Apply {
  apply(block: Block): Promise<void>;
  /** Absent when the handler has none (finalized streams). */
  undo?(forkPoint: BlockRef): Promise<void>;
}

/**
 * The chain the app applied, merged from one or two lanes (spec §9.5): it
 * follows the lane with the highest block, the current one on a tie, else
 * the next ready one; undoes to the fork point when it switches branch;
 * never applies a block the chain holds. No app call starts once `signal`
 * aborted.
 */
export class Merger {
  /** Oldest first, each the parent of the next; empty until the first block. */
  private chain: BlockRef[];
  private leader: Lane | undefined;

  constructor(
    private readonly lanes: Lane[],
    cursor: BlockRef | undefined,
    private readonly app: Apply,
    private readonly signal: AbortSignal,
  ) {
    this.chain = cursor ? [cursor] : [];
  }

  /** The merged cursor: every session resumes from it. */
  get cursor(): BlockRef | undefined {
    return this.chain.at(-1);
  }

  async block(lane: Lane, b: Block): Promise<void> {
    if (!lane.add(b)) this.restart(lane);
    await this.follow();
  }

  async undo(lane: Lane, fp: BlockRef): Promise<void> {
    lane.undo(fp);
    await this.follow();
  }

  /** Applies the leader's branch; a lane that shares no block with the chain restarts. */
  async follow(): Promise<void> {
    try {
      const lead = this.leader;
      const order: Lane[] = lead?.ready ? [lead] : [];
      for (const l of this.lanes) {
        if (l === lead || !l.ready) continue;
        if (order.length > 0 && l.high > order[0].high) order.unshift(l);
        else order.push(l);
      }
      for (const l of order) {
        if (await this.followLane(l)) {
          this.leader = l;
          return;
        }
        this.restart(l);
      }
    } finally {
      for (const l of this.lanes) l.undone = false;
    }
  }

  private restart(lane: Lane): void {
    lane.live = false;
    lane.cancel();
  }

  /**
   * Moves the chain onto lane's branch: undo to the fork point when the
   * chain holds blocks above it, then apply each block above it. False
   * when the branch shares no block with the chain.
   */
  private async followLane(lane: Lane): Promise<boolean> {
    const base = lane.base!;
    let k = lane.blocks.length - 1;
    while (k >= 0 && !this.has(lane.blocks[k])) k--;
    let fork: BlockRef;
    if (k >= 0) fork = refOf(lane.blocks[k]);
    else if (this.chain.length === 0) {
      fork = base;
      this.chain = [fork];
    } else if (this.has(base)) fork = base;
    else if (lane.trusted && base.number <= this.chain[0].number) {
      // Below every block the chain holds: the server's undo, or resend,
      // to a block this client applied before.
      fork = base;
    } else return false;
    const above = lane.blocks.slice(k + 1);
    if (above.length === 0 && !lane.undone) return true;
    lane.undone = false;
    if (!same(this.chain.at(-1)!, fork)) {
      if (!this.app.undo) {
        throw new FinalRevertedError(
          lane.endpoint,
          `final_reverted: blocks above ${fork.number} were reverted`,
        );
      }
      if (this.signal.aborted) return true;
      await this.app.undo(fork);
      this.chain = this.has(fork)
        ? this.chain.slice(0, Number(fork.number - this.chain[0].number) + 1)
        : [fork];
    }
    for (const b of above) {
      if (this.signal.aborted) return true;
      await this.app.apply(b);
      this.chain.push(refOf(b));
      if (this.chain.length > 2 * chainKeep) this.chain = this.chain.slice(-chainKeep);
    }
    return true;
  }

  private has(ref: BlockRef): boolean {
    const first = this.chain[0];
    if (!first || ref.number < first.number) return false;
    const r = this.chain[Number(ref.number - first.number)];
    return r !== undefined && same(r, ref);
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
