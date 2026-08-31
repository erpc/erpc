# Quantum Blockchain API

A Python blockchain that connects **quantum computing** to **blockchain
security** at three concrete points, backed by a Flask REST API and
SQLite persistence — not just a buzzword mashup, but a working demo of
where quantum mechanics actually touches blockchain cryptography.

## Why quantum + blockchain?

| Connection | What it means here |
|---|---|
| **Threat** | Shor's algorithm breaks ECDSA (the signature scheme Bitcoin/Ethereum use). Grover's algorithm halves the effective security of SHA-256 proof-of-work. |
| **Defense** | Hash-based signatures (this project uses **Lamport signatures**) resist quantum attacks because they only depend on a hash function being one-way — something Shor's algorithm doesn't break. |
| **Utility** | Superposition + measurement gives *true* randomness, unlike deterministic PRNGs — used here to seed keys and block nonces. |

## Architecture

1. **`QuantumRNG`** — generates true random bits via Hadamard
   superposition + measurement (Qiskit + AerSimulator), used to seed
   Lamport private keys and block nonces.
2. **`LamportKeypair`** — post-quantum, hash-based one-time signatures
   replacing ECDSA for all transactions.
3. **`grover_search_demo()`** — runs real Grover's algorithm to show
   the quadratic quantum speedup over classical brute-force search,
   illustrating the same speedup that threatens PoW nonce search.
4. **`Database`** (SQLite) — persists the full chain, mempool, public
   keys, and an activity/news feed to `quantum_blockchain.db`, so node
   state survives restarts.
5. **Flask API** — exposes everything above as a REST interface.

## Install

```bash
pip install -r requirements.txt --break-system-packages
```

Pinned versions:
- `qiskit==2.5.2`
- `qiskit-aer==0.17.2`
- `Flask==3.1.3`

(`sqlite3` and `hashlib` are Python standard library.)

## Run

```bash
python3 quantum_blockchain.py
```

The API starts on `http://localhost:5000`.

## API Reference

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/` | List all available endpoints |
| `GET` | `/chain` | View the full blockchain |
| `POST` | `/mine` | Mine a new block. Body: `{"miner_address": str}` |
| `POST` | `/transactions/new` | Add a signed transaction |
| `GET` | `/transactions/pending` | List all pending (unmined) transactions |
| `GET` | `/transactions/<id>` | Get one pending transaction |
| `PUT` \| `PATCH` | `/transactions/<id>` | Update a pending transaction. Body: `{"sender", "recipient", "amount"}` |
| `DELETE` | `/transactions/<id>` | Remove a pending transaction |
| `POST` | `/keys/register` | Generate + store a new Lamport public key. Body: `{"owner": str}` |
| `GET` | `/qrng/<n_bits>` | Get `n_bits` true-random bits from a quantum circuit (max 512) |
| `GET` | `/grover_demo/<n_qubits>` | Run Grover's algorithm search demo (max 8 qubits) |
| `GET` | `/db/stats` | Database file size and row counts |
| `GET` | `/news` | Activity feed of chain events. Query params: `?limit=` (max 200), `?event_type=` |

### Example

```bash
curl -X POST http://localhost:5000/keys/register \
  -H "Content-Type: application/json" \
  -d '{"owner": "alice"}'

curl -X POST http://localhost:5000/transactions/new \
  -H "Content-Type: application/json" \
  -d '{"sender": "alice", "recipient": "bob", "amount": 10}'

curl -X POST http://localhost:5000/mine \
  -H "Content-Type: application/json" \
  -d '{"miner_address": "alice"}'

curl http://localhost:5000/news
```

## Database

All state lives in `quantum_blockchain.db` (SQLite), created
automatically on first run:

- **`blocks`** — the full chain
- **`pending_transactions`** — mempool, cleared once mined
- **`public_keys`** — registered Lamport public keys per address
- **`news_log`** — activity feed (blocks mined, transactions
  added/updated/removed, keys registered, Grover demo runs)

State survives process restarts — the chain is reloaded from disk on
startup rather than rebuilt from scratch.

## Known limitations

- Lamport signatures are **one-time use** and large (~8KB per
  signature) — a deliberate trade-off shown here to illustrate the
  cost of going post-quantum, not optimized for production size.
- The Qiskit simulator is used throughout (no real quantum hardware
  connection) — this is a demonstration of the algorithms and their
  properties, not a production quantum-random-number source.
- Single-node only; no peer-to-peer networking or consensus across
  multiple nodes.

## License

Add your preferred license here (e.g. MIT, Apache-2.0).