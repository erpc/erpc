---
name: quantum-blockchain-api
description: Build, extend, or debug a Python quantum-secured blockchain Flask API with SQLite persistence — QRNG-seeded nonces (Qiskit), post-quantum Lamport signatures, a Grover's-algorithm threat demo, a news/activity feed, and full JSON CRUD for transactions. Use this whenever the user asks to combine "quantum" with "blockchain" or "API" in Python, wants a quantum-random-number-generator demo, post-quantum cryptography example, or is iterating on the existing quantum_blockchain.py project (adding endpoints, updating the database schema, adding news/activity logging, etc.), even if they phrase it tersely like "update database", "add news endpoints", or "install".
---

# Quantum Blockchain API

A reference implementation and extension pattern for a Flask API that
demonstrates three real connections between quantum computing and
blockchain security, backed by SQLite persistence.

## When to use this

- The user asks to build a Python file/API connecting "quantum" and
  "blockchain" (or just "quantum" + "API" + "database").
- The user is iterating on an existing quantum blockchain project —
  short follow-ups like "update database", "add news endpoints",
  "use JSON transaksi update", "install" almost always mean: extend
  `assets/quantum_blockchain.py` with a new capability.
- The user wants a working demo of: quantum random number generation,
  post-quantum (hash-based) signatures, Grover's algorithm vs.
  classical brute force, or SQLite-backed API persistence.

## The three-part architecture (always preserve this framing)

1. **Quantum randomness** (`QuantumRNG`) — Hadamard superposition +
   measurement via Qiskit/AerSimulator generates *true* random bits,
   not pseudo-random. Used to seed Lamport private keys and block
   nonces.
2. **Post-quantum signatures** (`LamportKeypair`) — hash-based
   one-time signatures. Security rests only on SHA-256 being one-way,
   which Shor's algorithm does not break (unlike ECDSA's discrete-log
   problem, which it does).
3. **Quantum threat demo** (`grover_search_demo`) — runs real Grover's
   algorithm to show the quadratic speedup a quantum attacker gets
   searching a space, illustrating the same speedup that threatens
   proof-of-work nonce search.

Everything else (Blockchain, Database, Flask routes, news feed) is
built around these three pillars. When extending the project, prefer
adding to this structure over introducing a parallel one.

## Starting point

Copy `assets/quantum_blockchain.py` and `assets/requirements.txt` into
the working directory as the base to edit — don't regenerate from
scratch. Install with:

```bash
pip install -r requirements.txt --break-system-packages
```

Qiskit's simulator needs a transpile step before `sim.run()` for
anything beyond a bare Hadamard+measure circuit (Grover operators,
custom gates) — use `from qiskit import transpile; transpiled =
transpile(qc, sim)` or you'll hit `AerError: unknown instruction`.

## Database-backed extension pattern

The project stores everything in SQLite (`quantum_blockchain.db`) via
a single `Database` class — `blocks`, `pending_transactions`,
`public_keys`, `news_log` tables. When asked to add a new capability
(e.g. wallets, staking, a leaderboard):

1. Add a table to `Database._init_schema()`.
2. Add typed read/write methods on `Database` (mirror the existing
   `save_x` / `load_x` / `get_x` naming).
3. Wire the new methods into `Blockchain` (business logic + signature
   verification lives here, not in the Flask routes).
4. Add Flask routes that call into `Blockchain`, not `Database`
   directly.
5. Log a `news_log` entry for any user-meaningful event via
   `self.db.log_news(event_type, headline, details_dict)` — this is
   the project's activity-feed convention and users often ask for
   "news" or "activity" updates that just mean "log this to
   `news_log` and expose it."
6. Update the `/` index route's endpoint listing and the module
   docstring's `curl` examples to match.

## Testing requirement — do this before delivering

Never hand back the file without running it. Use the Flask test
client, not a live server:

```python
from quantum_blockchain import app
client = app.test_client()
r = client.post('/transactions/new', json={...})
print(r.status_code, r.get_json())
```

For anything touching persistence, test across a **simulated
restart** — create state in one process, then import fresh in a new
`python3 -c "..."` call and confirm it reloads correctly. This is the
only way to actually verify SQLite persistence works, as opposed to
just trusting the code.

Before final delivery: `rm -f quantum_blockchain.db` so the shipped
file doesn't carry test data, then copy to
`/mnt/user-data/outputs/` and call `present_files`.

## Common pitfalls seen in this project

- Qiskit measurement bit order is reversed (qubit 0 is the rightmost
  character in the result string) — reverse before comparing to a
  binary-encoded target int.
- Signature verification must be re-run on **updates**, not just
  creates — an edited transaction is a new claim and needs a fresh
  signature check, not a bypass.
- Keep `pending_transactions` lightweight (`load_pending_transactions`)
  for internal block-building vs. full JSON (`load_pending_transactions_full`,
  with id/signature/timestamp) for API responses — don't conflate the
  two or every `/chain` response balloons with hex signature dumps.

## Reference files

- `assets/quantum_blockchain.py` — full working implementation,
  tested end-to-end (QRNG, Lamport sign/verify, mining, Grover demo,
  SQLite persistence across restart, news feed, transaction CRUD).
- `assets/requirements.txt` — pinned dependency versions, verified
  against a clean virtualenv install.
