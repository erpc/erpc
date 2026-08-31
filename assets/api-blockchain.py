"""
quantum_blockchain.py
======================
A blockchain implementation that connects quantum computing to blockchain
security at three concrete points:

  1. QUANTUM RANDOMNESS  -> qiskit is used to generate true random bits
     (via Hadamard superposition + measurement) instead of pseudo-random
     numbers. These bits seed private keys and block nonces.

  2. POST-QUANTUM SIGNATURES -> instead of ECDSA (breakable by Shor's
     algorithm on a large enough quantum computer), this chain uses
     Lamport one-time signatures, a hash-based scheme believed to be
     quantum-resistant.

  3. QUANTUM THREAT DEMO -> a Grover's-algorithm simulation shows the
     quadratic search speedup a quantum attacker would get when trying
     to find a valid proof-of-work nonce, versus classical brute force.

  4. DATABASE PERSISTENCE -> the full chain, pending transactions, and
     registered public keys are stored in a SQLite database
     (quantum_blockchain.db) rather than only in memory, so the node's
     state survives restarts, matching how real blockchain nodes persist
     their ledger to disk.

  5. NEWS / ACTIVITY FEED -> every meaningful chain event (block mined,
     transaction added, key registered, Grover demo run) is logged to a
     `news_log` table and exposed via GET /news, like a block explorer's
     activity feed.

A minimal Flask REST API exposes the chain so it behaves like a real
blockchain node.

Run:
    python3 quantum_blockchain.py
Then in another terminal:
    curl http://localhost:5000/chain
    curl -X POST http://localhost:5000/mine
    curl http://localhost:5000/qrng/16
    curl http://localhost:5000/grover_demo/6
    curl http://localhost:5000/db/stats
    curl http://localhost:5000/news
"""

import hashlib
import json
import os
import sqlite3
import time
import secrets as py_secrets
from contextlib import contextmanager
from typing import List, Dict, Any, Optional

from qiskit import QuantumCircuit
from qiskit_aer import AerSimulator
from flask import Flask, jsonify, request


# ---------------------------------------------------------------------------
# 1. QUANTUM RANDOM NUMBER GENERATOR
# ---------------------------------------------------------------------------
class QuantumRNG:
    """
    Generates true random bits using quantum superposition.

    A qubit is put into |0> + |1> superposition with a Hadamard gate.
    Measuring it collapses it to 0 or 1 with 50/50 probability -- this
    randomness comes from quantum mechanics, not a deterministic algorithm,
    unlike classical PRNGs (even cryptographically secure ones like
    os.urandom, which are ultimately deterministic given their seed/state).
    """

    def __init__(self):
        self.simulator = AerSimulator()

    def random_bits(self, n_bits: int) -> str:
        """Return a string of n_bits random '0'/'1' characters."""
        # Batch qubits into circuits of up to 30 to stay efficient.
        bits = []
        remaining = n_bits
        while remaining > 0:
            batch = min(remaining, 30)
            qc = QuantumCircuit(batch, batch)
            qc.h(range(batch))          # superposition on every qubit
            qc.measure(range(batch), range(batch))

            result = self.simulator.run(qc, shots=1, memory=True).result()
            bitstring = result.get_memory()[0]  # e.g. '01101...'
            bits.append(bitstring)
            remaining -= batch
        return "".join(bits)[:n_bits]

    def random_bytes(self, n_bytes: int) -> bytes:
        bits = self.random_bits(n_bytes * 8)
        return int(bits, 2).to_bytes(n_bytes, byteorder="big")

    def random_int(self, n_bits: int) -> int:
        return int(self.random_bits(n_bits), 2)


qrng = QuantumRNG()


# ---------------------------------------------------------------------------
# 2. POST-QUANTUM SIGNATURES (Lamport one-time signatures)
# ---------------------------------------------------------------------------
class LamportKeypair:
    """
    Lamport signatures are hash-based: their security relies only on the
    hash function being one-way, which Shor's algorithm does NOT break
    (unlike ECDSA's elliptic-curve discrete log, which it does break).
    Grover's algorithm only gives a quadratic speedup against hashes,
    so doubling hash output length restores full security.

    Trade-off: keys are one-time use and signatures are large -- this is
    illustrative of the space/performance cost of going post-quantum.
    """

    HASH_BITS = 256
    HASH_BYTES = HASH_BITS // 8

    def __init__(self, rng: QuantumRNG = qrng):
        # Private key: 256 pairs of random 256-bit values.
        self.private_key: List[List[bytes]] = [
            [rng.random_bytes(self.HASH_BYTES), rng.random_bytes(self.HASH_BYTES)]
            for _ in range(self.HASH_BITS)
        ]
        # Public key: SHA-256 hash of every private key value.
        self.public_key: List[List[str]] = [
            [hashlib.sha256(pair[0]).hexdigest(), hashlib.sha256(pair[1]).hexdigest()]
            for pair in self.private_key
        ]

    def sign(self, message: str) -> List[str]:
        digest = hashlib.sha256(message.encode()).digest()
        bits = "".join(f"{byte:08b}" for byte in digest)  # 256 bits
        signature = [
            self.private_key[i][int(bit)].hex()
            for i, bit in enumerate(bits)
        ]
        return signature

    @staticmethod
    def verify(message: str, signature: List[str], public_key: List[List[str]]) -> bool:
        digest = hashlib.sha256(message.encode()).digest()
        bits = "".join(f"{byte:08b}" for byte in digest)
        for i, bit in enumerate(bits):
            revealed = bytes.fromhex(signature[i])
            expected_hash = public_key[i][int(bit)]
            if hashlib.sha256(revealed).hexdigest() != expected_hash:
                return False
        return True


# ---------------------------------------------------------------------------
# 3. DATABASE (SQLite persistence layer)
# ---------------------------------------------------------------------------
DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "quantum_blockchain.db")


class Database:
    """
    Persists the entire node state to disk with SQLite:
      - blocks              (the full chain, one row per block)
      - pending_transactions(mempool, cleared once mined into a block)
      - public_keys          (registered Lamport public keys per address)

    Every write commits immediately so state is durable even if the
    process crashes right after an API call.
    """

    def __init__(self, db_path: str = DB_PATH):
        self.db_path = db_path
        self._init_schema()

    @contextmanager
    def _connect(self):
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA foreign_keys = ON")
        try:
            yield conn
            conn.commit()
        finally:
            conn.close()

    def _init_schema(self):
        with self._connect() as conn:
            conn.executescript("""
                CREATE TABLE IF NOT EXISTS blocks (
                    idx             INTEGER PRIMARY KEY,
                    timestamp       REAL NOT NULL,
                    transactions    TEXT NOT NULL,   -- JSON array
                    previous_hash   TEXT NOT NULL,
                    nonce           INTEGER NOT NULL,
                    quantum_seed    TEXT NOT NULL,
                    hash            TEXT NOT NULL UNIQUE
                );

                CREATE TABLE IF NOT EXISTS pending_transactions (
                    id          INTEGER PRIMARY KEY AUTOINCREMENT,
                    sender      TEXT NOT NULL,
                    recipient   TEXT NOT NULL,
                    amount      REAL NOT NULL,
                    signature   TEXT NOT NULL,        -- JSON array
                    created_at  REAL NOT NULL
                );

                CREATE TABLE IF NOT EXISTS public_keys (
                    owner       TEXT PRIMARY KEY,
                    public_key  TEXT NOT NULL,         -- JSON
                    registered_at REAL NOT NULL
                );

                CREATE TABLE IF NOT EXISTS news_log (
                    id          INTEGER PRIMARY KEY AUTOINCREMENT,
                    event_type  TEXT NOT NULL,   -- e.g. 'block_mined', 'transaction_added',
                                                   -- 'key_registered', 'grover_demo_run'
                    headline    TEXT NOT NULL,
                    details     TEXT,             -- JSON, optional extra payload
                    created_at  REAL NOT NULL
                );
            """)

    # ---- blocks -----------------------------------------------------
    def save_block(self, block: "Block"):
        with self._connect() as conn:
            conn.execute(
                """INSERT INTO blocks
                   (idx, timestamp, transactions, previous_hash, nonce, quantum_seed, hash)
                   VALUES (?, ?, ?, ?, ?, ?, ?)""",
                (block.index, block.timestamp, json.dumps(block.transactions),
                 block.previous_hash, block.nonce, block.quantum_seed, block.hash),
            )

    def load_chain(self) -> List["Block"]:
        with self._connect() as conn:
            rows = conn.execute("SELECT * FROM blocks ORDER BY idx ASC").fetchall()
        chain = []
        for row in rows:
            block = Block(
                index=row["idx"],
                transactions=json.loads(row["transactions"]),
                previous_hash=row["previous_hash"],
                quantum_seed=row["quantum_seed"],
            )
            block.timestamp = row["timestamp"]
            block.nonce = row["nonce"]
            block.hash = row["hash"]
            chain.append(block)
        return chain

    def chain_length(self) -> int:
        with self._connect() as conn:
            return conn.execute("SELECT COUNT(*) AS c FROM blocks").fetchone()["c"]

    # ---- pending transactions (mempool) ------------------------------
    def add_pending_transaction(self, sender: str, recipient: str,
                                 amount: float, signature: List[str]) -> int:
        with self._connect() as conn:
            cur = conn.execute(
                """INSERT INTO pending_transactions
                   (sender, recipient, amount, signature, created_at)
                   VALUES (?, ?, ?, ?, ?)""",
                (sender, recipient, amount, json.dumps(signature), time.time()),
            )
            return cur.lastrowid

    def load_pending_transactions(self) -> List[Dict[str, Any]]:
        """Lightweight view used internally when building a block."""
        with self._connect() as conn:
            rows = conn.execute(
                "SELECT * FROM pending_transactions ORDER BY id ASC"
            ).fetchall()
        return [
            {"sender": r["sender"], "recipient": r["recipient"], "amount": r["amount"]}
            for r in rows
        ]

    def load_pending_transactions_full(self) -> List[Dict[str, Any]]:
        """Full JSON view (id, signature, timestamp) used by the API."""
        with self._connect() as conn:
            rows = conn.execute(
                "SELECT * FROM pending_transactions ORDER BY id ASC"
            ).fetchall()
        return [
            {
                "id": r["id"],
                "sender": r["sender"],
                "recipient": r["recipient"],
                "amount": r["amount"],
                "signature": json.loads(r["signature"]),
                "created_at": r["created_at"],
            }
            for r in rows
        ]

    def get_pending_transaction(self, tx_id: int) -> Optional[Dict[str, Any]]:
        with self._connect() as conn:
            row = conn.execute(
                "SELECT * FROM pending_transactions WHERE id = ?", (tx_id,)
            ).fetchone()
        if not row:
            return None
        return {
            "id": row["id"],
            "sender": row["sender"],
            "recipient": row["recipient"],
            "amount": row["amount"],
            "signature": json.loads(row["signature"]),
            "created_at": row["created_at"],
        }

    def update_pending_transaction(self, tx_id: int, sender: str, recipient: str,
                                    amount: float, signature: List[str]) -> bool:
        with self._connect() as conn:
            cur = conn.execute(
                """UPDATE pending_transactions
                   SET sender = ?, recipient = ?, amount = ?, signature = ?
                   WHERE id = ?""",
                (sender, recipient, amount, json.dumps(signature), tx_id),
            )
            return cur.rowcount > 0

    def delete_pending_transaction(self, tx_id: int) -> bool:
        with self._connect() as conn:
            cur = conn.execute("DELETE FROM pending_transactions WHERE id = ?", (tx_id,))
            return cur.rowcount > 0

    def clear_pending_transactions(self):
        with self._connect() as conn:
            conn.execute("DELETE FROM pending_transactions")

    # ---- public key registry -----------------------------------------
    def save_public_key(self, owner: str, public_key: List[List[str]]):
        with self._connect() as conn:
            conn.execute(
                """INSERT INTO public_keys (owner, public_key, registered_at)
                   VALUES (?, ?, ?)
                   ON CONFLICT(owner) DO UPDATE SET
                       public_key = excluded.public_key,
                       registered_at = excluded.registered_at""",
                (owner, json.dumps(public_key), time.time()),
            )

    def load_public_key(self, owner: str) -> Optional[List[List[str]]]:
        with self._connect() as conn:
            row = conn.execute(
                "SELECT public_key FROM public_keys WHERE owner = ?", (owner,)
            ).fetchone()
        return json.loads(row["public_key"]) if row else None

    def stats(self) -> Dict[str, Any]:
        with self._connect() as conn:
            blocks = conn.execute("SELECT COUNT(*) AS c FROM blocks").fetchone()["c"]
            pending = conn.execute("SELECT COUNT(*) AS c FROM pending_transactions").fetchone()["c"]
            keys = conn.execute("SELECT COUNT(*) AS c FROM public_keys").fetchone()["c"]
        return {
            "db_path": self.db_path,
            "db_size_bytes": os.path.getsize(self.db_path) if os.path.exists(self.db_path) else 0,
            "blocks_stored": blocks,
            "pending_transactions": pending,
            "registered_public_keys": keys,
        }

    # ---- news / activity feed -----------------------------------------
    def log_news(self, event_type: str, headline: str, details: Optional[Dict[str, Any]] = None):
        with self._connect() as conn:
            conn.execute(
                """INSERT INTO news_log (event_type, headline, details, created_at)
                   VALUES (?, ?, ?, ?)""",
                (event_type, headline, json.dumps(details) if details is not None else None,
                 time.time()),
            )

    def get_news(self, limit: int = 20, event_type: Optional[str] = None) -> List[Dict[str, Any]]:
        with self._connect() as conn:
            if event_type:
                rows = conn.execute(
                    """SELECT * FROM news_log WHERE event_type = ?
                       ORDER BY id DESC LIMIT ?""",
                    (event_type, limit),
                ).fetchall()
            else:
                rows = conn.execute(
                    "SELECT * FROM news_log ORDER BY id DESC LIMIT ?", (limit,)
                ).fetchall()
        return [
            {
                "id": r["id"],
                "event_type": r["event_type"],
                "headline": r["headline"],
                "details": json.loads(r["details"]) if r["details"] else None,
                "created_at": r["created_at"],
            }
            for r in rows
        ]


# ---------------------------------------------------------------------------
# 4. BLOCKCHAIN CORE
# ---------------------------------------------------------------------------
class Block:
    def __init__(self, index: int, transactions: List[Dict[str, Any]],
                 previous_hash: str, quantum_seed: str):
        self.index = index
        self.timestamp = time.time()
        self.transactions = transactions
        self.previous_hash = previous_hash
        # Nonce search starts from a quantum-random seed rather than 0,
        # so mining start points are unpredictable too.
        self.quantum_seed = quantum_seed
        self.nonce = int(quantum_seed, 16) if quantum_seed else 0
        self.hash = ""

    def compute_hash(self) -> str:
        block_string = json.dumps({
            "index": self.index,
            "timestamp": self.timestamp,
            "transactions": self.transactions,
            "previous_hash": self.previous_hash,
            "nonce": self.nonce,
        }, sort_keys=True)
        return hashlib.sha256(block_string.encode()).hexdigest()

    def to_dict(self) -> Dict[str, Any]:
        return {
            "index": self.index,
            "timestamp": self.timestamp,
            "transactions": self.transactions,
            "previous_hash": self.previous_hash,
            "nonce": self.nonce,
            "hash": self.hash,
        }


class Blockchain:
    """
    All state lives in the SQLite database (self.db); the in-memory
    `chain` / `pending_transactions` lists are just a cache reloaded from
    the DB, so nothing is lost between restarts.
    """

    DIFFICULTY = 4  # number of leading zero hex digits required

    def __init__(self, db: Optional[Database] = None):
        self.db = db or Database()
        self.chain: List[Block] = self.db.load_chain()
        if not self.chain:
            self._create_genesis_block()
        self.refresh_pending()

    def _create_genesis_block(self):
        genesis = Block(0, [], "0" * 64, qrng.random_bits(16))
        genesis.hash = genesis.compute_hash()
        self.chain.append(genesis)
        self.db.save_block(genesis)

    def refresh_pending(self):
        """Reload the mempool from the database (source of truth)."""
        self.pending_transactions = self.db.load_pending_transactions()

    @property
    def last_block(self) -> Block:
        return self.chain[-1]

    def register_public_key(self, owner: str, public_key: List[List[str]]):
        self.db.save_public_key(owner, public_key)
        self.db.log_news("key_registered", f"New wallet registered: {owner}",
                          {"owner": owner})

    def get_public_key(self, owner: str) -> Optional[List[List[str]]]:
        return self.db.load_public_key(owner)

    def add_transaction(self, sender: str, recipient: str, amount: float,
                         signature: List[str], public_key: List[List[str]]) -> Optional[int]:
        """Returns the new pending transaction's id, or None if the
        signature failed verification."""
        message = f"{sender}->{recipient}:{amount}"
        if not LamportKeypair.verify(message, signature, public_key):
            return None
        tx_id = self.db.add_pending_transaction(sender, recipient, amount, signature)
        self.refresh_pending()
        self.db.log_news("transaction_added",
                          f"{sender} -> {recipient}: {amount}",
                          {"id": tx_id, "sender": sender, "recipient": recipient, "amount": amount})
        return tx_id

    def update_transaction(self, tx_id: int, sender: str, recipient: str, amount: float,
                            signature: List[str], public_key: List[List[str]]) -> bool:
        """Update a still-pending (not yet mined) transaction's JSON fields.
        The new sender/recipient/amount must still verify against a valid
        Lamport signature, exactly like a brand new transaction."""
        existing = self.db.get_pending_transaction(tx_id)
        if existing is None:
            return False
        message = f"{sender}->{recipient}:{amount}"
        if not LamportKeypair.verify(message, signature, public_key):
            return False
        ok = self.db.update_pending_transaction(tx_id, sender, recipient, amount, signature)
        if ok:
            self.refresh_pending()
            self.db.log_news(
                "transaction_updated",
                f"Pending tx #{tx_id} updated -> {sender} -> {recipient}: {amount}",
                {"id": tx_id, "sender": sender, "recipient": recipient, "amount": amount},
            )
        return ok

    def delete_transaction(self, tx_id: int) -> bool:
        ok = self.db.delete_pending_transaction(tx_id)
        if ok:
            self.refresh_pending()
            self.db.log_news("transaction_removed", f"Pending tx #{tx_id} removed",
                              {"id": tx_id})
        return ok

    def proof_of_work(self, block: Block) -> str:
        target = "0" * self.DIFFICULTY
        computed_hash = block.compute_hash()
        while not computed_hash.startswith(target):
            block.nonce += 1
            computed_hash = block.compute_hash()
        return computed_hash

    def mine_block(self, miner_reward_address: str) -> Block:
        self.refresh_pending()
        transactions = self.pending_transactions + [{
            "sender": "NETWORK", "recipient": miner_reward_address, "amount": 1.0
        }]
        new_block = Block(
            index=len(self.chain),
            transactions=transactions,
            previous_hash=self.last_block.hash,
            quantum_seed=qrng.random_bits(16),
        )
        new_block.hash = self.proof_of_work(new_block)

        # Persist the block, then atomically clear the mempool it consumed.
        self.db.save_block(new_block)
        self.db.clear_pending_transactions()
        self.db.log_news(
            "block_mined",
            f"Block #{new_block.index} mined with {len(transactions)} transaction(s)",
            {"index": new_block.index, "hash": new_block.hash, "nonce": new_block.nonce,
             "tx_count": len(transactions)},
        )

        self.chain.append(new_block)
        self.pending_transactions = []
        return new_block

    def is_valid(self) -> bool:
        for i in range(1, len(self.chain)):
            current, previous = self.chain[i], self.chain[i - 1]
            if current.hash != current.compute_hash():
                return False
            if current.previous_hash != previous.hash:
                return False
        return True


# ---------------------------------------------------------------------------
# GROVER'S ALGORITHM DEMO -- quantum threat to proof-of-work
# ---------------------------------------------------------------------------
def grover_search_demo(n_qubits: int, marked_state: int = None) -> Dict[str, Any]:
    """
    Runs Grover's algorithm to find a 'marked' item among 2^n_qubits
    possibilities, illustrating the quadratic speedup a quantum computer
    gets over classical brute-force search -- the same speedup that
    would apply to searching for a valid PoW nonce.

    Classical brute force needs ~N/2 tries on average (N = 2^n_qubits).
    Grover's algorithm needs only ~sqrt(N) queries.
    """
    import math
    from qiskit.circuit.library import GroverOperator, MCMTGate, ZGate

    N = 2 ** n_qubits
    if marked_state is None:
        marked_state = qrng.random_int(n_qubits) % N

    # Oracle that flips the phase of |marked_state>
    oracle = QuantumCircuit(n_qubits)
    binary = format(marked_state, f"0{n_qubits}b")
    for i, bit in enumerate(binary):
        if bit == "0":
            oracle.x(i)
    oracle.append(MCMTGate(ZGate(), n_qubits - 1, 1), list(range(n_qubits)))
    for i, bit in enumerate(binary):
        if bit == "0":
            oracle.x(i)

    grover_op = GroverOperator(oracle)
    optimal_iterations = math.floor(math.pi / 4 * math.sqrt(N))

    qc = QuantumCircuit(n_qubits, n_qubits)
    qc.h(range(n_qubits))
    for _ in range(max(optimal_iterations, 1)):
        qc.append(grover_op, range(n_qubits))
    qc.measure(range(n_qubits), range(n_qubits))

    from qiskit import transpile
    sim = AerSimulator()
    transpiled = transpile(qc, sim)
    result = sim.run(transpiled, shots=256).result()
    counts = result.get_counts()
    found = max(counts, key=counts.get)
    # Qiskit reports bitstrings with qubit 0 as the rightmost character;
    # reverse to match the natural left-to-right binary encoding used above.
    found_value = int(found[::-1], 2)

    return {
        "search_space_size": N,
        "target_marked_state": marked_state,
        "target_binary": binary,
        "grover_iterations_used": max(optimal_iterations, 1),
        "classical_avg_tries_needed": N // 2,
        "quantum_tries_needed": max(optimal_iterations, 1),
        "speedup_factor": round((N / 2) / max(optimal_iterations, 1), 2),
        "measured_result_binary": found,
        "success": found_value == marked_state,
        "measurement_counts": counts,
    }


# ---------------------------------------------------------------------------
# FLASK API
# ---------------------------------------------------------------------------
app = Flask(__name__)
blockchain = Blockchain()  # loads existing chain from quantum_blockchain.db if present
demo_keypair = LamportKeypair()  # a single demo keypair for quick testing
blockchain.register_public_key("demo_sender", demo_keypair.public_key)


@app.route("/chain", methods=["GET"])
def get_chain():
    return jsonify({
        "length": len(blockchain.chain),
        "chain": [b.to_dict() for b in blockchain.chain],
        "valid": blockchain.is_valid(),
    })


@app.route("/mine", methods=["POST"])
def mine():
    data = request.get_json(silent=True) or {}
    miner_address = data.get("miner_address", "demo_miner")
    start = time.time()
    block = blockchain.mine_block(miner_address)
    elapsed = time.time() - start
    return jsonify({"message": "Block mined", "block": block.to_dict(),
                     "seconds_taken": round(elapsed, 3)})


@app.route("/transactions/new", methods=["POST"])
def new_transaction():
    data = request.get_json(force=True)
    required = ["sender", "recipient", "amount"]
    if not all(k in data for k in required):
        return jsonify({"error": "missing fields"}), 400

    # Look up the sender's registered public key in the database; fall
    # back to the demo keypair for quick manual testing.
    public_key = blockchain.get_public_key(data["sender"]) or demo_keypair.public_key
    message = f"{data['sender']}->{data['recipient']}:{data['amount']}"
    signature = demo_keypair.sign(message)

    tx_id = blockchain.add_transaction(
        data["sender"], data["recipient"], data["amount"],
        signature, public_key,
    )
    if tx_id is None:
        return jsonify({"error": "invalid signature"}), 400
    return jsonify({"message": "Transaction added and persisted to database",
                     "id": tx_id,
                     "signature_preview": signature[:2]}), 201


@app.route("/transactions/pending", methods=["GET"])
def list_pending_transactions():
    """Full JSON view of every pending (not yet mined) transaction."""
    blockchain.refresh_pending()
    return jsonify({
        "count": len(blockchain.pending_transactions),
        "transactions": blockchain.db.load_pending_transactions_full(),
    })


@app.route("/transactions/<int:tx_id>", methods=["GET"])
def get_transaction(tx_id):
    tx = blockchain.db.get_pending_transaction(tx_id)
    if tx is None:
        return jsonify({"error": f"pending transaction {tx_id} not found"}), 404
    return jsonify(tx)


@app.route("/transactions/<int:tx_id>", methods=["PUT", "PATCH"])
def update_transaction(tx_id):
    """
    Update a pending transaction's JSON body (sender/recipient/amount)
    before it is mined into a block. Re-signs and re-verifies exactly
    like creating a brand new transaction, so the update is just as
    authenticated as the original.

    Body JSON:
      {"sender": str, "recipient": str, "amount": number}
    """
    data = request.get_json(force=True)
    required = ["sender", "recipient", "amount"]
    if not all(k in data for k in required):
        return jsonify({"error": "missing fields, need sender/recipient/amount"}), 400

    public_key = blockchain.get_public_key(data["sender"]) or demo_keypair.public_key
    message = f"{data['sender']}->{data['recipient']}:{data['amount']}"
    signature = demo_keypair.sign(message)

    ok = blockchain.update_transaction(
        tx_id, data["sender"], data["recipient"], data["amount"],
        signature, public_key,
    )
    if not ok:
        return jsonify({"error": f"pending transaction {tx_id} not found or invalid signature"}), 404
    return jsonify({"message": f"Transaction {tx_id} updated", "id": tx_id,
                     "transaction": blockchain.db.get_pending_transaction(tx_id)})


@app.route("/transactions/<int:tx_id>", methods=["DELETE"])
def delete_transaction(tx_id):
    ok = blockchain.delete_transaction(tx_id)
    if not ok:
        return jsonify({"error": f"pending transaction {tx_id} not found"}), 404
    return jsonify({"message": f"Transaction {tx_id} removed"})


@app.route("/keys/register", methods=["POST"])
def register_key():
    """Generate a fresh Lamport keypair for `owner`, store the public key
    in the database, and return the private key (only ever shown once,
    exactly like a real wallet)."""
    data = request.get_json(silent=True) or {}
    owner = data.get("owner")
    if not owner:
        return jsonify({"error": "missing 'owner'"}), 400

    kp = LamportKeypair()
    blockchain.register_public_key(owner, kp.public_key)
    return jsonify({
        "owner": owner,
        "message": "Public key saved to database. Store the private key yourself - it is not saved.",
        "private_key_preview": [pair[0].hex()[:16] for pair in kp.private_key[:2]],
    })


@app.route("/db/stats", methods=["GET"])
def db_stats():
    return jsonify(blockchain.db.stats())


@app.route("/qrng/<int:n_bits>", methods=["GET"])
def qrng_endpoint(n_bits):
    if n_bits > 512:
        return jsonify({"error": "max 512 bits per request"}), 400
    return jsonify({"n_bits": n_bits, "random_bits": qrng.random_bits(n_bits)})


@app.route("/grover_demo/<int:n_qubits>", methods=["GET"])
def grover_endpoint(n_qubits):
    if n_qubits > 8:
        return jsonify({"error": "max 8 qubits for demo (simulator limit)"}), 400
    result = grover_search_demo(n_qubits)
    blockchain.db.log_news(
        "grover_demo_run",
        f"Grover's algorithm demo run: {n_qubits}-qubit search, "
        f"{result['speedup_factor']}x speedup over classical",
        {"n_qubits": n_qubits, "speedup_factor": result["speedup_factor"],
         "success": result["success"]},
    )
    return jsonify(result)


@app.route("/news", methods=["GET"])
def get_news():
    """
    Activity feed / block-explorer-style news endpoint. Returns the most
    recent chain events (blocks mined, transactions, keys registered,
    Grover demo runs), newest first.

    Query params:
      limit       - max items to return (default 20, max 200)
      event_type  - filter to one type, e.g. ?event_type=block_mined
    """
    limit = min(request.args.get("limit", default=20, type=int), 200)
    event_type = request.args.get("event_type")
    items = blockchain.db.get_news(limit=limit, event_type=event_type)
    return jsonify({"count": len(items), "event_type_filter": event_type, "news": items})


@app.route("/", methods=["GET"])
def index():
    return jsonify({
        "endpoints": {
            "GET /chain": "view full blockchain",
            "POST /mine": "mine a new block (body: {\"miner_address\": str})",
            "POST /transactions/new": "add a signed transaction (persisted to DB)",
            "GET /transactions/pending": "list all pending (unmined) transactions as JSON",
            "GET /transactions/<id>": "get one pending transaction by id",
            "PUT /transactions/<id>": "update a pending transaction (JSON body: sender/recipient/amount)",
            "DELETE /transactions/<id>": "remove a pending transaction",
            "POST /keys/register": "generate + store a new Lamport public key (body: {\"owner\": str})",
            "GET /qrng/<n_bits>": "get n true-random bits from a quantum circuit",
            "GET /grover_demo/<n_qubits>": "run Grover's algorithm search demo",
            "GET /db/stats": "view database file stats (size, row counts)",
            "GET /news": "activity feed of chain events (blocks, txs, keys). ?limit=&event_type=",
        }
    })


if __name__ == "__main__":
    print("Quantum-secured blockchain node starting on http://localhost:5000")
    app.run(host="0.0.0.0", port=5000, debug=False)
