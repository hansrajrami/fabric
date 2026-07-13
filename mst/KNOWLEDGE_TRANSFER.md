# MST Integration — Phase 1 Knowledge Transfer

**Audience:** a developer who has never seen this branch and needs to understand
*what we built, why, and how the pieces fit* — well enough to read the diff, extend
it, or operate it.

This is the **narrative** document. The others are references you'll reach for
afterwards:

| Doc | Read it for |
|---|---|
| **KNOWLEDGE_TRANSFER.md** (this) | the idea, the reasoning, the build order, what each file does |
| [`README.md`](README.md) | architecture summary + commitment spec + acceptance-criteria traceability |
| [`SETUP.md`](SETUP.md) | how to deploy and operate it (both modes), step by step |
| [`UPGRADING.md`](UPGRADING.md) | how to merge a newer Fabric release into this fork |
| [`deploy/README.md`](deploy/README.md) | the hands-on local end-to-end demo walk |

> Read sections 1–4 for the *idea*, section 5 as the *build-order walkthrough*, and
> keep section 6 open as a *file map* while you browse the code.

---

## 1. The problem, in one paragraph

Hyperledger Fabric is a **private, permissioned** ledger — great for confidentiality,
but a counterparty (auditor, regulator, another consortium) generally cannot
independently verify that a given transaction happened, exactly as claimed, at a
given time, without being given trusted access to the Fabric network. We want the
best of both: keep the data private in Fabric, but publish a **tamper-evident,
publicly verifiable fingerprint** of each relevant transaction onto a public
EVM-compatible chain ("MST"). Anyone later holding the original transaction data can
recompute that fingerprint and check it against the public chain — no trust in us
required.

## 2. The core idea (the "thought process")

Four insights drove every design decision. If you internalise these, the code reads
itself:

1. **Only a hash travels, never business data.** For each opted-in transaction we
   compute a 32-byte `keccak256` **commitment** over a fixed tuple (tx id, channel,
   chaincode, block, timestamp, and a hash of the declared business fields). That
   single hash is what we anchor publicly. The raw values never leave Fabric, so
   anchoring is privacy-safe by construction and the on-chain footprint is constant.

2. **The commitment must be byte-for-byte reproducible by anyone.** A proof is
   worthless if two honest implementations compute different hashes for the same
   data. So the encoding is *canonical* (deterministic, no ambiguity) and is guarded
   by a **cross-language test gate**: Go, TypeScript, and Solidity must agree on
   every byte, in CI, forever.

3. **Never touch Fabric's commit path.** The anchoring pipeline must never slow,
   block, or endanger Fabric consensus/commit. We achieve this *structurally*: we
   only ever read **already-committed** blocks, and every slow/fallible thing (EVM
   submission, confirmations, write-back) happens **asynchronously behind a durable
   queue**. A relayer crash or an MST outage can never hurt Fabric.

4. **Exactly-once, despite an unreliable world.** Blocks get redelivered, processes
   crash, RPCs time out, multiple peers see the same block. We get exactly-once *in
   effect* by combining two cheap guarantees:
   **durability** (a crash-safe outbox with an atomic checkpoint → nothing is ever
   lost) **+ idempotency** (the on-chain contract records each key once; a repeat is
   a quiet no-op → nothing is ever duplicated). Neither alone is enough; together
   they are.

Everything else — sidecar vs embedded, LevelDB vs CouchDB, single vs Merkle
batching — is an *option* layered on this spine without disturbing it.

## 3. Mental model

```
 Business chaincode                     (opts in per tx: proofhelper.Emit → MSTProofRequest event)
        │  block commits on Fabric (final; nothing anchored yet)
        ▼
 CAPTURE  ──►  OUTBOX  ──►  SENDER  ──►  [MST] MSTAnchor contract
 (reads committed  (durable, crash-   (cadence, retries,   (idempotent, immutable
  blocks; builds    safe; atomic       confirmations,       per key; emits event)
  commitment)       entry+checkpoint)  batching)                │
                                                                ▼
                                          WRITE-BACK ──► [Fabric] mst-anchor-status chaincode
                                          (records "tx X is anchored" on Fabric's own ledger)
```

Each outbox entry moves through a state machine, and the sender only ever advances
it one atomic step at a time (compare-and-set), so a crash resumes cleanly:

```
PENDING ──submit──► SUBMITTED ──confirmations──► CONFIRMED ──write-back──► WRITTEN_BACK ──► DONE
   ▲                    │  (failure/timeout re-queues)
   └────────────────────┘
```

**Why this is exactly-once:** the outbox `PutBlock` writes a block's new entries
*and* advances the "last processed block" checkpoint in **one atomic, fsync'd
batch** — so a crash can never skip a block or double-insert one (durability). And
the contract stores each `fabric_tx_id` (or batch root) exactly once — a resubmit is
a quiet no-op (idempotency). At-least-once delivery therefore becomes exactly-once
*in effect*.

## 4. Load-bearing design decisions (the "why" behind the code)

**(a) Canonical encoding + commitment + 3-way gate.** `mst/canonical` encodes the
declared payload deterministically (fields sorted by raw UTF-8 name bytes,
length-prefixed, type-tagged; floats/nesting/duplicates/non-NFC strings *rejected*,
never silently coerced) and hashes the fixed tuple with hand-rolled EVM ABI encoding
+ keccak256. Three independent implementations (Go, TypeScript via ethers, Solidity
via the contract's own `abi.encode`) must produce identical bytes for every vector
in `mst/testvectors/vectors.json`, enforced in CI. This is the single most important
correctness asset — build/verify it first.

**(b) Timestamp = the transaction's `ChannelHeader.Timestamp`.** Fabric *block*
headers carry no timestamp, and wall-clock commit time isn't recoverable from the
ledger. The only deterministic, verifier-recomputable time is the client-asserted
timestamp inside the signed transaction envelope — so that's what the commitment
uses. (This is a documented clarification of the original spec.)

**(c) Two deployment modes, and the proto-module split that shapes the code.** The
pipeline ships as a **sidecar daemon** (`mst-relayd`, talks to a peer over the
Gateway API — works against an unpatched upstream peer) *and* **embedded in the peer
binary** (behind a `core.yaml` flag, default off). The catch: the peer links the old
`fabric-protos-go`, while the Gateway client links `fabric-protos-go-apiv2`, and the
two register the same proto file paths — linking both into one binary panics at
init. So the *capture core, outbox, and sender are proto-free* (they consume
`mst/relay/txmodel`), and each world has a thin proto adapter: `mst/relay/blockparse`
(apiv2, sidecar) and `internal/pkg/mstanchor/parse.go` (old protos, in-peer). **Never
import `mst/relay/gwsource` into peer-linked code, and never import
`github.com/hyperledger/fabric` into the relay module.**

**(d) The outbox is the relayer's OWN store, never the peer's ledger.** Putting relay
bookkeeping on the Fabric ledger would drag the relay back onto the commit path
(every status flip = a consensus tx) — exactly what insight #3 forbids. And the
peer's own LevelDB is single-process-locked. So the relayer keeps a private
embedded LevelDB, *or* — when the peer runs CouchDB — its own dedicated
`mst_outbox_<channel>` databases on the same CouchDB server (never the peer's state
DBs). The backend hides behind one `Store` interface; correctness is identical (see
`couchdb.go`'s "write ordering + idempotency" note where atomic batches aren't
available).

**(e) Capture modes: `opt-in` vs `all`.** By default only transactions that emit the
`MSTProofRequest` event are anchored (the chaincode opts in per tx and declares which
fields the proof covers). `all` anchors *every* valid transaction — opted-in ones
keep their declared payload; the rest get the well-known *empty-payload* commitment
(an existence-and-timing proof). System/write-back chaincodes are always excluded
(the echo-loop guard becomes load-bearing here).

**(f) Batching: `individual` vs `merkle` — and where the "combined proof" lives.**
`individual` keeps one on-chain record per tx but shares one EVM transaction for a
flush (~40% gas saving; verification unchanged). `merkle` anchors only the **Merkle
root** over the flush's commitments (~95% saving); verifying one tx then needs its
**inclusion proof** (the ~log₂N sibling hashes), exported from the outbox by
`mst-proof` and checked by `mst-verify --batch-proof`. Batch membership is persisted
*before* submission so proofs survive a crash. The tree is OpenZeppelin-compatible
(sorted leaves, sorted-pair keccak) so a future on-chain verifier can use the audited
library.

**(g) Cadence is orthogonal to strategy.** *When* to flush — `per-tx`, `batch` of N,
`interval`, or `cron` (5-field expression) — is independent of *how* (individual /
merkle). High-volume audit trails typically pair `all` + `merkle` + `cron`.

**(h) Gas-balance monitoring.** The relayer's EVM account pays gas; if it empties,
anchoring stalls *safely* (entries queue) but *silently*. A metrics gauge
(`mst_relayer_balance_gwei`) plus an optional low-balance watcher make that visible.

## 5. Build walkthrough — step by step, in order

The 20 commits map to 17 logical steps (plus 3 fix commits). This is the order to
read the branch; each step builds on the last. `git log --oneline
origin/release-2.5..HEAD` shows them 1:1.

**Step 1 — Anchor contract** (`mst/anchor-contracts/`). The public record, built
first so everything else has a target. `MSTAnchor.sol`: `anchor(txId, commitment,
blockNumber)` idempotent (duplicate = quiet no-op), immutable per key, self-stamped
`block.timestamp`, optional relayer allowlist. Hardhat tests + deploy script; ABI
exported to `abi/MSTAnchor.json` (Go tests read it to catch selector drift).

**Step 2 — Canonical encoding + commitment + cross-language gate** (`mst/canonical/`,
`mst/testvectors/`). The load-bearing correctness piece (decision *a*). `encode.go`,
`decode.go` (strict — re-encode must equal input), `commitment.go` (ABI tuple +
keccak), `keccak.go`; `vectors.json` with golden values; the TypeScript reference in
`testvectors/reference-ts`; the Solidity spot-check `CommitmentCheck.sol`. **Do not
proceed past a red gate.** This unblocks everyone.

**Step 3 — Opt-in helper + example chaincode** (`mst/fabric-chaincode/proofhelper`,
`.../example-chaincode`). `proofhelper.New().Add…().Emit(stub)` validates at
endorsement time and emits the `MSTProofRequest` event whose payload *is* the
canonical encoding (one format end to end). The example chaincode shows the pattern.

**Step 4 — Durable outbox** (`mst/relay/outbox/`, starts the `mst/relay` module).
The crash-safe queue (decision *d*). `entry.go` (the record + state machine),
`store.go` (`Store` interface + LevelDB impl with the atomic entry+checkpoint
batch), `quarantine.go`. This is the backbone of exactly-once.

**Step 5 — Capture service** (`mst/relay/blockparse`, `.../capture`,
`.../internal/blocktest`). Turns committed blocks into outbox entries, strictly
post-commit. `blockparse` extracts tx facts; `capture` filters (valid txs,
`MSTProofRequest`, echo-loop exclusion), builds commitments, quarantines poison
pills, writes one atomic batch per block. Tested entirely on synthetic block protos —
no live Fabric needed.

**Step 6 — Relayer/sender + EVM client** (`mst/relay/evm`, `.../sender`, `cmd/mst-relayd`).
Drains the outbox to MST. `evm/` (ethclient wrapper, hand-packed calldata, EIP-1559,
nonce mgmt, confirmations); `sender/` (cadence, bounded workers, backoff, pre-submit
`getAnchor` short-circuit, crash recovery). Fake-EVM unit tests + a live hardhat
integration test.

**Step 7 — Anchor-status chaincode + write-back** (`mst/fabric-chaincode/anchor-status`,
`mst/relay/fabricwb`). The Fabric-side acknowledgment: `RecordAnchor` (idempotent,
MSP-gated, **never emits events** → can't echo-loop). `fabricwb` submits it via the
Gateway.

**Step 8 — Verification tool** (`mst/relay/verifylib`, `cmd/mst-verify`). Recompute
the commitment from original data, read the anchor, print MATCH/NO-MATCH. This is the
whole point made runnable; demonstrated live (MATCH on good data, NO-MATCH after
tampering one field).

**Step 9 — CI + deploy + docs.** `.github/workflows/mst.yml` (Go + TS + Hardhat + the
cross-language gate as required jobs), `mst/deploy/` (compose + example config),
`mst/README.md` with the acceptance-criteria table.

**Step 10 — Proto-free refactor** (`mst/relay/txmodel`, move gateway code to
`mst/relay/gwsource`). Preparation for embedding: the capture core stops importing
apiv2 protos and consumes the proto-free `txmodel` instead (decision *c*). No
behaviour change; this is the pivot that makes step 11 possible.

**Steps 11–12 — Embed into the peer** (`internal/pkg/mstanchor/*`,
`internal/peer/node/mst.go`, one hook in `internal/peer/node/start.go`,
`sampleconfig/core.yaml`, `go.mod`). The same capture/outbox/sender now runs inside
`peer node start` behind `mst.enabled` (default off → vanilla peer). Blocks come from
the peer's own ledger (`source.go` + `parse.go`, old protos); write-back
(`writeback.go`) calls the peer's own gateway server **in-process** (no network hop),
signed with a configured relayer MSP identity (low-S ECDSA). The only pre-existing
Fabric file touched is `start.go` (+18/-2).

**Step 13 — Docs + CI for embedded mode.** README deployment-mode comparison; a CI
job that builds the patched peer and runs `peer version` to prove there's no
proto-registration panic. (`UPGRADING.md` — the merge playbook — was added just
after.)

**Step 14 — CouchDB outbox, auto-following the peer's stateDatabase**
(`mst/relay/outbox/couchdb.go`, config in both modes). A second `Store`
implementation on plain `net/http`; the embedded mode picks LevelDB or CouchDB to
match `ledger.state.stateDatabase`. A backend-agnostic contract-test suite runs
against LevelDB, an in-process fake CouchDB, and (in CI) a real `couchdb:3`.

**Step 15 — Gas-balance monitoring** (`mst/relay/sender/balance.go`, `metrics.go`).
Decision *h*: gauge + low-balance watcher, wired into both modes.

**Step 16 — Anchor-all capture mode** (`capture.go` + both parsers surface the
invoked chaincode id). Decision *e*: `captureMode: all` anchors every valid tx, with
`includeChaincodes` scoping and system-chaincode exclusion.

**Step 17 — Configurable batching + cron** (`mst/canonical/merkle.go`, contract
`anchorRoot`/`getRoot`, `mst/relay/sender/batch.go`, `cadence.go`, `cmd/mst-proof`,
`verifylib` batch verdicts). Decisions *f* and *g*: individual/anchorBatch and merkle
roots with inclusion proofs; cron cadence. This is the largest single step.

*(Fix commits: dropped an accidentally committed chaincode binary; removed a
`mst/relay/vendor` directory a stray `go mod vendor` created — twice — now
`.gitignore`d; fixed gofumpt formatting for Fabric's lint.)*

## 6. File-by-file reference (the map)

### `mst/canonical/` — the shared correctness core (own Go module; deps: x/crypto, x/text)
- `canonical.go` — field types, the `Payload`/`Field` model, validation.
- `encode.go` / `decode.go` — canonical serialization; `decode` is strict (accepts
  only canonical bytes).
- `commitment.go` — the 8-field tuple, ABI encoding, `keccak256`, `domain_tag`,
  `schema_version`.
- `keccak.go` — legacy keccak256 (the EVM's, not NIST SHA3).
- `merkle.go` — batch root + inclusion proof (OpenZeppelin-compatible).

### `mst/testvectors/` — the cross-language gate
- `vectors.json` — golden encodings/hashes (regenerate only with a `schema_version`
  bump: `go test ./mst/canonical -run TestVectors -update`).
- `reference-ts/` — the independent TypeScript implementation (ethers v6).

### `mst/anchor-contracts/` — the on-chain record (Hardhat)
- `contracts/MSTAnchor.sol` — `anchor`, `anchorBatch`, `anchorRoot`, `getAnchor`,
  `getRoot`; idempotent, immutable per key, optional allowlist.
- `contracts/test/CommitmentCheck.sol` — Solidity leg of the vector gate.
- `abi/MSTAnchor.json` — checked-in ABI the Go tests verify selectors against.
- `scripts/deploy.ts`, `hardhat.config.ts` — deploy + solc pinned via npm (proxy-friendly).

### `mst/fabric-chaincode/` — chaincode artifacts (own Go modules)
- `proofhelper/proofhelper.go` — the opt-in builder + `Emit`.
- `example-chaincode/` — demo business chaincode using it.
- `anchor-status/contract.go` — the write-back target (idempotent, MSP-gated,
  non-anchorable).

### `mst/relay/` — the pipeline (own Go module)
- `txmodel/txmodel.go` — **proto-free** Tx/Event/Block; the boundary both worlds share.
- `blockparse/blockparse.go` — apiv2 → txmodel (sidecar parser).
- `gwsource/gwsource.go` — Gateway block-event source (**sidecar-only**; never
  peer-linked).
- `capture/capture.go` — the proto-free capture core (opt-in/all modes, quarantine,
  atomic write).
- `outbox/` — `entry.go` (record + state machine), `store.go` (`Store` + LevelDB),
  `couchdb.go` (CouchDB), `quarantine.go`.
- `evm/` — `client.go` (ethclient wrapper), `calldata.go` (hand-packed calls +
  selector tests).
- `sender/` — `sender.go` (drain loop, recovery), `batch.go` (individual/merkle),
  `cadence.go` (per-tx/batch/interval/cron), `backoff.go`, `balance.go`,
  `metrics.go`, `writeback.go` (interface + no-op).
- `fabricwb/` — Gateway write-back client (sidecar).
- `verifylib/verifylib.go` — recompute + compare; single-anchor and batch verdicts.
- `config/config.go` — the sidecar JSON config.
- `cmd/mst-relayd` (daemon), `cmd/mst-verify` (verify), `cmd/mst-proof` (export
  inclusion proof).
- `internal/blocktest/` — synthetic block builder for tests.

### Fabric-tree changes (the embedded mode + wiring)
- `internal/pkg/mstanchor/config.go` — reads the `core.yaml` `mst:` section;
  auto-follows `stateDatabase`; validates.
- `internal/pkg/mstanchor/parse.go` — old-proto → txmodel (in-peer parser).
- `internal/pkg/mstanchor/source.go` — ledger blocks-iterator source.
- `internal/pkg/mstanchor/service.go` — per-channel pipeline lifecycle + metrics.
- `internal/pkg/mstanchor/writeback.go` — in-process gateway write-back + identity signer.
- `internal/peer/node/mst.go` — the start/stop glue; `mstGatewayServer` handoff.
- `internal/peer/node/start.go` — **the only pre-existing file changed** (+18/-2): one
  hook in `serve()`.
- `sampleconfig/core.yaml` — the commented `mst:` config reference.
- `.github/workflows/mst.yml` — the whole CI matrix.
- `go.mod` / `go.sum` / `vendor/` — the mst modules added via local `replace`; the
  fabric module vendors the relay packages it needs.

## 7. The two deployment modes, side by side

| | **Sidecar** (`mst-relayd`) | **Embedded** (in the peer) |
|---|---|---|
| Enable | run the daemon | `mst.enabled: true` in core.yaml (default false) |
| Block source | Gateway `BlockEvents` (`gwsource`, apiv2) | peer ledger iterator (`mstanchor/source.go`, old protos) |
| Config | `mst-relayd.json` (`relay/config`) | `core.yaml` `mst:` (`internal/pkg/mstanchor/config.go`) |
| Write-back | Gateway client (`fabricwb`) | peer's own gateway, in-process (`mstanchor/writeback.go`) |
| Works against upstream peer | yes | no (needs this fork's binary) |
| Fault isolation | full (separate process) | shares the peer process |

Both drive the *same* `capture` + `outbox` + `sender`. The embedded mode is glued in
by exactly one hook in `internal/peer/node/start.go` (capture the gateway server
handle into `mstGatewayServer`, start/stop the service around the signal handlers).

## 8. Build, run, test, verify

```bash
# Go core + relay (per module)
(cd mst/canonical && go test ./...)
(cd mst/relay && go vet ./... && go test -race ./...)

# Cross-language gate
(cd mst/testvectors/reference-ts && npm install && npm test)
(cd mst/anchor-contracts && npm install && npx hardhat test)

# Live EVM integration (spawns nothing; point at a running node)
(cd mst/anchor-contracts && npx hardhat node &)          # terminal 1
(cd mst/relay && MST_EVM_RPC=http://127.0.0.1:8545 go test -run TestIntegration ./evm/)

# Embedded peer: build + prove no proto-registration panic
go build -o /tmp/peer ./cmd/peer && /tmp/peer version
go test ./internal/pkg/mstanchor/... ./internal/peer/node/...

# Fabric's own lint — use the PINNED tools, not @latest (see gotchas)
(cd tools && GOFLAGS=-mod=mod go install mvdan.cc/gofumpt golang.org/x/tools/cmd/goimports honnef.co/go/tools/cmd/staticcheck)
PATH=$(go env GOPATH)/bin:$PATH ./scripts/golinter.sh
```

To *operate* it (deploy contract/chaincode, configure, verify a real tx), follow
[`SETUP.md`](SETUP.md).

## 9. Gotchas a newcomer will hit

- **The two-proto rule.** `mst/relay/gwsource` (apiv2) must never be imported by
  peer-linked code, and `github.com/hyperledger/fabric` (old protos) must never be
  imported by the relay module — linking both proto modules panics at init. Anything
  shared goes through the proto-free `txmodel`.
- **The `go mod vendor` trap.** Running `go mod vendor` from `mst/relay` creates a
  34 MB `mst/relay/vendor/` that must NOT be committed (the relay module resolves
  from the cache; only the fabric root vendors). It's now `.gitignore`d — but it bit
  us twice, so don't un-ignore it.
- **Lint uses pinned tools.** Fabric's CI runs `gofumpt v0.1.0` (from `tools/go.mod`),
  which is *older/looser* than `@latest`. Format with the pinned version or you'll
  "fix" dozens of upstream files wrongly.
- **`MST_RELAYER_KEY` is env-only.** The relayer's EVM private key is never in any
  config file, by design. Both modes read it from the environment.
- **One log line needs a human:** `anchor exists with mismatched commitment`. It
  means an on-chain anchor disagrees with local capture; it is deliberately never
  retried (retrying can't fix it) and must be investigated.
- **Vectors are frozen.** Changing any expected value in `vectors.json` is a
  breaking change — only do it with a `schema_version` bump; otherwise old proofs
  stop verifying.
- **Merkle mode wants a batching cadence.** `merkle` with `per-tx` cadence makes
  single-leaf "batches" (works — root = leaf — but wastes the aggregation). Pair it
  with `batch`/`interval`/`cron`.

## 10. What is intentionally NOT done (and the Part 2 hooks)

- **Zero-knowledge proofs (Part 2)** are out of scope. The design leaves room:
  outbox entries carry an `EntryType`, the commitment carries `schema_version` and
  `domain_tag`, and the sender's submit step is small/extensible — a second payload
  type (a proof) and a second destination contract (a verifier) slot in without
  disturbing the pipeline.
- **Open productionizing items** (suggested, not built): relayer key in a
  keystore/HSM instead of an env var; a periodic re-verification auditor (guards
  against deep reorgs after `DONE`); outbox retention/pruning (`DONE` + quarantine
  currently grow unbounded); a dedicated stuck-entry alarm metric; wiring embedded
  metrics into the peer's operations endpoint.
- **The full live end-to-end on a real Fabric network** has not been run in-session
  (needs Docker/Fabric images); every component is unit/integration-tested and the
  procedure is in [`deploy/README.md`](deploy/README.md).
- **PR #21** carries this branch; CI (including the cross-language gate and the
  real-CouchDB job) runs there.

## 11. Glossary

- **Commitment** — the 32-byte keccak256 fingerprint of a transaction's tuple; the
  thing anchored.
- **Canonical encoding** — the deterministic, byte-exact serialization that makes
  commitments reproducible across languages.
- **Declared payload** — the business fields a chaincode chooses to cover with a
  proof; only their hash travels.
- **Capture** — reading committed blocks and turning opted-in txs into outbox entries.
- **Outbox** — the relayer's own crash-safe queue (LevelDB or CouchDB).
- **Relayer / sender** — drains the outbox to MST, confirms, writes back.
- **Anchor** — the on-chain record `fabric_tx_id → commitment` (or the batch root).
- **Root / inclusion proof** — in merkle batching, the single anchored root and the
  sibling hashes that prove one tx is under it.
- **Write-back** — recording on Fabric's own ledger that a tx was anchored.
- **Capture mode** — `opt-in` (only `MSTProofRequest` emitters) vs `all` (every valid tx).
- **Batch strategy** — `individual` (one record per tx) vs `merkle` (one root per flush).
- **Cadence** — *when* to flush: `per-tx` / `batch` / `interval` / `cron`.
