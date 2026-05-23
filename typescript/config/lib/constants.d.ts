/** One primary per network — max cohesion across methods + finalities. Default. */
export declare const NETWORK: string;
/** One primary per `(network, method)`; finalities share. */
export declare const NETWORK_METHOD: string;
/** One primary per `(network, finality)`; methods share. */
export declare const NETWORK_FINALITY: string;
/** One primary per slot — independent per `(network, method, finality)`. */
export declare const NETWORK_METHOD_FINALITY: string;
/** Bit 0 — request is reading live tip data (e.g. eth_blockNumber). */
export declare const REALTIME: number;
/** Bit 1 — request is reading a recent-but-not-yet-finalized block. */
export declare const UNFINALIZED: number;
/** Bit 2 — request is reading a finalized (no-reorg) block. */
export declare const FINALIZED: number;
/** Bit 3 — finality couldn't be determined for this request. */
export declare const UNKNOWN: number;
//# sourceMappingURL=constants.d.ts.map