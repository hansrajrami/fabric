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

## End-to-end flow

```
 Business chaincode ── proofhelper.Emit("MSTProofRequest", canonicalBytes)
        │  (block commits; Fabric Gateway delivers it post-commit)
        ▼
 capture (relay/capture) ──► outbox (relay/outbox, LevelDB) ──► sender (relay/sender) ──► [MST] MSTAnchor
   valid txs only              atomic entry+checkpoint            cadence, workers,        idempotent,
   echo-loop guard             idempotent, crash-safe             backoff, pre-submit      immutable/key,
   quarantine                                                     getAnchor short-circuit  emits Anchored
                                                                        │
                                                                        ▼
                                                  fabricwb ──► [Fabric] mst-anchor-status (RecordAnchor)
```

Fabric's commit path is never touched: capture consumes already-committed
blocks, and all network work is asynchronous behind the durable outbox.
**Durability stops loss; idempotency stops duplication — together:
exactly-once in effect.**

## Deployment modes

The same pipeline (capture core, outbox, sender — all proto-free via
`relay/txmodel`) ships in two interchangeable forms:

| | **Sidecar** (`mst-relayd`) | **Embedded** (in the peer binary) |
|---|---|---|
| Block source | Fabric Gateway block events (`relay/gwsource`, apiv2 protos) | The peer's own ledger iterator (`internal/pkg/mstanchor`, in-tree protos) |
| Config | JSON file (`mst/deploy/mst-relayd.example.json`) | `core.yaml` `mst:` section (see `sampleconfig/core.yaml`) |
| Enable | run the daemon | `mst.enabled: true` (**default false** — vanilla peer otherwise) |
| Channels | one per process | all joined channels (or `mst.channels` allowlist), one outbox/checkpoint per channel |
| Write-back | gateway client over the peer connection | the peer's own gateway server invoked **in-process** |
| Fault isolation | full (separate process) | shares the peer process |
| Outbox backend | LevelDB dir, or CouchDB via `outbox.type` in the JSON config | **auto-follows `ledger.state.stateDatabase`**: LevelDB peers keep it on local disk, CouchDB peers keep it in `mst_outbox_<channel>` databases on the peer's CouchDB server (own databases — never the peer's state DBs) |
| EVM key | `MST_RELAYER_KEY` env | `MST_RELAYER_KEY` env on the peer process |

Choose the sidecar when operational isolation matters most; choose embedded
when shipping a single differentiated peer binary matters most. Both are
safe to run redundantly against the same contract — the idempotent anchor
absorbs duplicates — but running both intentionally is wasteful.

The two proto worlds never meet: the peer links `fabric-protos-go`, the
sidecar's gateway source links `fabric-protos-go-apiv2`, and everything
shared between them is proto-agnostic (`relay/txmodel`). Linking both proto
modules into one binary would panic at init on duplicate proto registration,
which is why `relay/gwsource` must never be imported from peer-linked code.

## Acceptance criteria traceability (spec §17)

| # | Criterion | Where proven |
|---|---|---|
| 1 | Opt-in by emitting `MSTProofRequest` via the helper | `proofhelper` API + tests; `example-chaincode` contract tests |
| 2 | Every opted-in tx → exactly one commitment anchored automatically | capture tests (`TestCaptureEndToEnd`), sender happy path, EVM integration test |
| 3 | Exactly-once in effect (duplicates/retries never alter an anchor) | contract duplicate-no-op tests; `TestPutBlockIsIdempotentOnRedelivery`; `TestAlreadyAnchoredShortCircuits…`; integration duplicate test |
| 4 | Nothing lost across relayer crash / MST outage | outbox `TestCrashRecoveryReopen`; sender `TestConfirmTimeout…`, `TestCrashRecoveryFromSubmitted…`; capture `TestCaptureRestartExactlyOnce` |
| 5 | Fabric commit never blocked or slowed | structural: capture consumes post-commit block events only (`relay/capture` docs + design) |
| 6 | Anchor status recorded on Fabric; no echo loop | `anchor-status` tests (never emits events; idempotent; MSP-gated); capture exclusion test |
| 7 | Anyone with original data can verify | `verifylib` tests incl. tamper matrix; `mst-verify` MATCH/NO-MATCH demo |
| 8 | Cadence switchable by config | `TestBatchCadence…` tests; `cadenceMode` in relayd config |
| 9 | Cross-language vectors pass in CI in all implementations | `.github/workflows/mst.yml`: Go + TS + Solidity vector jobs |

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
