# MST Integration — Phase 1 Part 1: Proof Anchoring

This directory contains the MST proof-anchoring pipeline: when an opted-in
transaction commits on Hyperledger Fabric, a tamper-evident **commitment**
(a keccak256 fingerprint of a fixed transaction tuple) is automatically,
reliably, and exactly-once-in-effect recorded on the MST public blockchain
(EVM-compatible), and Fabric records the anchoring result. The private data
never leaves Fabric — only hashes travel.

There is **no zero-knowledge component** in Part 1; the pipeline is designed
so ZK proofs (Part 2) can ride the same outbox/sender/write-back later.

## Layout

| Directory | Contents |
|---|---|
| `anchor-contracts/` | `MSTAnchor.sol` (idempotent, immutable-per-key anchor registry) + Hardhat tests/deploy |
| `canonical/` | Go module: canonical payload encoding + commitment builder (the load-bearing correctness piece) |
| `testvectors/` | `vectors.json` shared cross-language vectors + TypeScript reference implementation |
| `fabric-chaincode/` | opt-in proof helper, example chaincode, anchor-status (write-back) chaincode |
| `relay/` | capture service, durable outbox, relayer/sender, verification CLI |
| `deploy/` | local dev/e2e composition |

## The commitment

```
commitment = keccak256( abi.encode(
    uint16   schema_version,   // 1
    bytes32  domain_tag,       // keccak256("MST_FABRIC_TX_ANCHOR_v1")
    bytes32  fabric_tx_id,
    string   channel_id,
    string   chaincode_id,
    uint64   block_number,
    uint64   timestamp,
    bytes32  payload_hash      // keccak256(canonical_encode(declared_payload))
) )
```

Three independent implementations must agree byte-identically on the vectors
in `testvectors/vectors.json`, enforced in CI (`.github/workflows/mst.yml`):

1. **Go** (`canonical/`) — hand-rolled fixed-tuple ABI encoding + keccak256.
2. **TypeScript** (`testvectors/reference-ts/`) — independent canonical
   encoder + ethers v6 `AbiCoder`.
3. **Solidity** (`anchor-contracts/contracts/test/CommitmentCheck.sol`) — the
   EVM's own `abi.encode` + `keccak256`.

Changing any expected vector value requires a conscious `schema_version` bump.
Regenerate goldens with: `go test ./mst/canonical -run TestVectors -update`.

## Spec clarifications (deviations from the Part 1 spec text)

- **`timestamp` source (spec §6.1/§8.2):** Fabric block headers carry **no
  timestamp** (`common.BlockHeader` has only number/prev-hash/data-hash), and
  wall-clock commit time is not recoverable from the ledger. The commitment
  therefore uses the transaction's **`ChannelHeader.Timestamp`** (unix
  seconds) — the client-asserted time inside the signed transaction envelope.
  It is deterministic and recomputable by any verifier holding the original
  transaction, which is exactly what independent verification requires.
- **String normalization:** the canonical encoding requires **NFC** for
  string *values* and field *names*; non-normalized or invalid UTF-8 input is
  rejected with an error, never silently normalized.
- **Ints are unsigned:** `int` fields are unsigned 256-bit (32-byte
  big-endian, one canonical form per value). Negative values are rejected.

## Development

```bash
# Go canonical package (unit + vector + fuzz tests)
cd mst/canonical && go test ./...

# TypeScript reference (cross-language vector gate)
cd mst/testvectors/reference-ts && npm install && npm test

# Anchor contract (unit tests + EVM vector check)
cd mst/anchor-contracts && npm install && npx hardhat test

# Local EVM node + deploy
cd mst/anchor-contracts && npx hardhat node   # terminal 1
npm run deploy:local                          # terminal 2
```
