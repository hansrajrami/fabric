# MST Integration — Phase 1.5 Knowledge Transfer

**Audience:** a developer who knows the Phase 1 pipeline (or is new to the branch)
and needs to understand *what Phase 1.5 changed, why, and how the pieces fit* —
well enough to read the diff, extend it, or operate it.

This is the **narrative** document for the new approach. The references you'll
reach for afterwards:

| Doc | Read it for |
|---|---|
| **KNOWLEDGE_TRANSFER.md** (this) | the idea, the reasoning, what each new piece does |
| [`PHASE-1.5.md`](PHASE-1.5.md) | the deep architecture: channel-config value, the SCC, per-channel EVM, gaps & trade-offs |
| [`README.md`](README.md) | architecture summary + commitment spec + acceptance-criteria traceability |
| [`SETUP.md`](SETUP.md) | how to deploy and operate the new approach, step by step |
| [`CLI.md`](CLI.md) | the `peer mst` operator command reference |
| [`UPGRADING.md`](UPGRADING.md) | how to merge a newer Fabric release into this fork |
| [`deploy/README.md`](deploy/README.md) | the hands-on local end-to-end demo walk |

> Read sections 1–2 for the *unchanged foundation*, section 3 for *what Phase 1.5
> changes and why* (the heart of this doc), sections 4–5 for the *new
> decisions and file map*, and keep section 8 handy for the *gotchas*.

---

## 1. The problem, unchanged

Hyperledger Fabric is a **private, permissioned** ledger. A counterparty (auditor,
regulator, another consortium) generally cannot independently verify that a given
transaction happened, exactly as claimed, at a given time, without trusted access
to the network. We keep the data private in Fabric but publish a **tamper-evident,
publicly verifiable fingerprint** of each relevant transaction onto a public
EVM-compatible chain ("MST"). Anyone later holding the original data recomputes the
fingerprint and checks it against the public chain — no trust in us required.

## 2. The unchanged foundation (still load-bearing)

These four insights from Phase 1 still drive everything. Phase 1.5 does **not**
touch them:

1. **Only a hash travels, never business data.** Each opted-in transaction yields a
   32-byte `keccak256` **commitment** over a fixed tuple (tx id, channel, chaincode,
   block, timestamp, and a hash of the declared fields). Only that hash is anchored.
2. **The commitment is byte-for-byte reproducible by anyone**, guarded by a
   **cross-language test gate** (Go ≡ TypeScript ≡ Solidity, in CI).
3. **Never touch Fabric's commit path.** Only already-committed blocks are read, and
   every slow/fallible step (EVM submit, confirmations, write-back) runs
   asynchronously behind a durable queue.
4. **Exactly-once in effect** = durability (crash-safe outbox with an atomic
   checkpoint) + idempotency (the contract records each key once; a repeat is a
   quiet no-op).

The capture → outbox → sender **spine is identical** to Phase 1. Phase 1.5 changes
the three surfaces *around* that spine. If you know Phase 1, you already know 80% of
this branch — read section 3 for the other 20%.

## 3. What Phase 1.5 changes, and why (the heart of it)

Phase 1 anchored to **one shared** contract, was enabled **per peer** via
`core.yaml`, and wrote back through a **user chaincode**. That works, but anchoring
was a *per-peer operational choice*, not a property the whole channel agreed on.
Phase 1.5 makes anchoring a **first-class, channel-governed** property by changing
exactly three surfaces:

### 3a. Per-channel contracts (isolation)
Each channel anchors to its **own** deployed `MSTAnchor` contract, so channels'
anchor histories are isolated on the MST chain. The embedded service keeps **one**
`evm.Client` (one relayer account, one serialized nonce sequence) and creates an
`evm.Binding` per channel that pins that channel's contract address — so N
per-channel contracts don't cause the head-of-line nonce collisions that N
independent clients on one account would.

### 3b. Channel-config governance (all orgs agree)
Whether a channel anchors — and *how* — is a **channel configuration** value, agreed
by all orgs through the Application group's modification policy, not a per-peer flag:

- The `MSTAnchor` value lives in the Application group (`common/channelconfig/mstanchor.go`),
  carried as JSON inside a `structpb.Value` (a deliberately additive encoding that
  avoids adding a message to the `fabric-protos` module).
- It holds `Enabled`/`ContractAddress`/`ChainID` **plus** the anchoring *policy* that
  must be uniform across peers: `CaptureMode`, `Include`/`ExcludeChaincodes`,
  `BatchStrategy`, `Confirmations`, and `Cadence*`. Per-peer divergence on *what/how/
  where* to anchor would produce ambiguous anchoring the idempotent contract can't
  reconcile — so these are channel-governed, each falling back to a fixed built-in
  default (never to `core.yaml`). `core.yaml` keeps only peer-local plumbing (RPC
  URL, relayer key, outbox path, workers, gas tuning).
- **Capability-gated.** The `MSTAnchor` value may appear only when the channel enables
  the **`V2_5_MSTANCHOR` application capability** (`common/capabilities/application.go`).
  This is the Fabric-native way to express "every node must run the MST-enabled
  binary": a vanilla binary doesn't report the capability, so it **cleanly refuses to
  join** the channel rather than choking on the unknown value or silently failing to
  endorse — turning "all peers must be patched" into an explicit, safe upgrade gate.
- `configtxgen` encodes it (`internal/configtxgen/encoder`); turning anchoring on/off
  later is an ordinary channel-config update under the Application mod policy.

### 3c. Anchor status as a ledger fact via a system chaincode
The write-back target is a **built-in system chaincode** (`mstscc`,
`core/scc/mstscc/`), not a user chaincode:

- **Peer-gated:** deployed only where `chaincode.system.mstscc` is enabled in
  `core.yaml` (the peer opting in / running the MST-enabled binary).
- **Channel-gated:** every invoke is rejected unless the channel's `MSTAnchorConfig`
  has anchoring enabled — so the SCC is inert on channels that never turned it on.
- **Peer-role authorization:** `RecordAnchor` requires a **peer-role identity**
  (NodeOUs) — only a peer *node's* signing identity may write anchor status, not a
  client/user cert. The SCC evaluates the tx creator against an `MSPRole{PEER}`
  principal using the channel MSP (`defaultRequirePeer`), so it honors NodeOUs and
  fails closed if NodeOUs are off. Reads (`QueryAnchorStatus`, `IsAnchored`,
  `ListAnchors`, `CountAnchors`) are open.
- **Echo-loop safe:** `mstscc` never calls `SetEvent`, and its name is always added
  to the capture service's excluded set, so a write-back can never re-enter the
  pipeline.

Because the SCC record is consensus-backed, every org can authoritatively query it.
But it is an **attestation + pointer**, not a proof: a Fabric SCC cannot read EVM
state, so `status: CONFIRMED` means "a peer node asserts this," not "verified
on-chain." Independent truth is still established off-ledger with `peer mst verify`.

### 3d. The knock-on additions
Making anchoring channel-governed pulled in four supporting pieces:

- **Upgradeable contract.** `MSTAnchor` is deployed behind an OpenZeppelin
  **TransparentUpgradeableProxy**, so a channel's contract address is **stable across
  implementation upgrades** — a same-chain fix no longer forces a new address that
  would split anchor history. Trade-off: the ProxyAdmin (deployer key by default) can
  upgrade the logic and thereby alter recorded anchors, so on-chain immutability is
  now *conditional on no malicious upgrade* — use a timelock+multisig admin in
  production (see [`anchor-contracts/README.md`](anchor-contracts/README.md)).
- **Live reconfiguration.** The embedded service's discovery loop reconciles each
  channel's running pipeline with its current config every tick: a governed-field
  change **hot-reloads** the channel's pipeline (stop + restart with the new config),
  and disabling MST **stops** it. Safe because the outbox is durable and capture
  resumes from its checkpoint.
- **Config validation + preflight.** `channelconfig` rejects a malformed/zero contract
  address and inconsistent cadence at config-apply time; `peer mst preflight` checks
  the config against the live chain (node reachable, chain-id match, contract
  deployed + ABI-compatible, MSP NodeOUs) *before* an operator applies an update.
- **Operator CLI.** `peer mst …` wraps the common tasks (see [`CLI.md`](CLI.md)).

### 3e. Embedded-only
Phase 1.5 is **embedded-only**: a sidecar cannot run a built-in system chaincode or
honor channel-config governance. The Phase 1 sidecar (`mst-relayd`) remains supported
for the legacy shared-contract / user-chaincode flow, but the new path lives in the
peer.

## 4. Mental model (new approach)

```
 channel config  (Application group value "MSTAnchor", all-org agreed, capability-gated)
   ├─ enabled, contractAddress (this channel's OWN proxy), chainID
   └─ captureMode / batchStrategy / confirmations / cadence  (governed policy)
        │  read at runtime by the peer; changes hot-reload the pipeline
        ▼
 Business chaincode ── proofhelper.Emit("MSTProofRequest", …)   (opt-in, unchanged)
        │  block commits
        ▼
 CAPTURE ─► OUTBOX (per channel) ─► SENDER ─► [MST] this channel's MSTAnchor proxy
   (unchanged spine; evm.Binding pins the per-channel address onto ONE shared
    relayer account / nonce sequence)
        │  Anchored
        ▼
 WRITE-BACK ─► [Fabric] mstscc.RecordAnchor   (built-in system chaincode)
   • only a PEER-role identity may submit     • rejected unless the channel enabled MST
   • never emits events (echo-safe)           • consensus-backed ledger fact
```

## 5. New / changed files (the map)

Everything under `mst/canonical`, `mst/relay/{capture,outbox,sender,evm}`,
`mst/fabric-chaincode/proofhelper`, and the commitment/vector gate is **unchanged
from Phase 1** — see the Phase 1 file map if you need it. Phase 1.5 adds/changes:

### Channel-config value + capability (`common/`)
- `common/channelconfig/mstanchor.go` — the `MSTAnchorConfig` type, validation
  (enum + duration + zero-address + cadence-consistency checks), and `MSTAnchorValue`.
- `common/channelconfig/application.go`, `api.go` — one `ApplicationProtos` field, a
  capability-gated parse block, and the `MSTAnchorConfig()` accessor on the
  `Application` interface.
- `common/channelconfig/nodeous.go` — `ApplicationOrgsMissingPeerNodeOUs` (detects the
  NodeOUs precondition from the raw config).
- `common/capabilities/application.go`, `common/channelconfig/api.go` — the
  `V2_5_MSTANCHOR` capability + the `MSTAnchor()` capability method.

### System chaincode (`core/scc/mstscc/`)
- `mstscc.go` — `RecordAnchor` / `QueryAnchorStatus` / `IsAnchored` / `ListAnchors` /
  `CountAnchors`; the channel-enablement gate; `defaultRequirePeer` (peer-role gate);
  the injectable `requirePeer` / `WithPeerAuthorizer` seam. Registered in
  `internal/peer/node/start.go` `builtinSCCs`.

### Config tooling
- `internal/configtxgen/genesisconfig/config.go`, `encoder/encoder.go` — the
  `MSTAnchor` profile struct + encode block (rejects the value without the capability).
- `sampleconfig/configtx.yaml` — the commented `MSTAnchor` example + the capability.
- `sampleconfig/core.yaml` — `chaincode.system.mstscc` peer opt-in.

### Per-channel EVM + embedded service
- `mst/relay/evm/client.go` — `Binding` (per-contract address), `Client.ChainID()`,
  `SetRelayer`.
- `internal/pkg/mstanchor/service.go` — per-channel pipeline lifecycle with
  **hot-reload** (`planPipelineAction`, `pipelineFingerprint`, `stopPipeline`), the
  ChainID/allowlist/NodeOUs startup warnings, per-pipeline cancellation.
- `internal/pkg/mstanchor/config.go` — reads only peer-local knobs; `CaptureConfigFor`/
  `SenderConfigFor` build from the channel config.
- `internal/peer/node/mst.go` — write-back wired to the SCC: endorsed in-process
  against the local endorser (the built-in `mstscc` has no discovery metadata for
  the gateway to plan against), then ordered + commit-polled through the gateway.

### Operator CLI (`internal/peer/mst/`)
- `mst.go` (shared setup), `query.go`, `config.go` (channel-config / onchain),
  `preflight.go`, `verify.go`, `pipeline.go`, `relayer.go` — the `peer mst` group.

### Upgradeable contract (`mst/anchor-contracts/`)
- `contracts/MSTAnchor.sol` — now `Initializable` (constructor → `initialize`,
  `allowlistEnabled` in storage, `_disableInitializers`, `__gap`).
- `contracts/proxy/Proxies.sol` — re-exports OZ's `TransparentUpgradeableProxy` so its
  bytecode compiles (for the Go deploy path).
- `scripts/deploy.ts` (deploys the proxy), `scripts/upgrade.ts` (in-place upgrade
  runbook), `abi/TransparentUpgradeableProxy.json`, `README.md`.

## 6. Deployment modes, side by side

| | **Phase 1 sidecar** (`mst-relayd`) | **Phase 1.5 embedded** (this doc) |
|---|---|---|
| Enable | run the daemon | channel-config `MSTAnchor` value + `V2_5_MSTANCHOR` capability + `chaincode.system.mstscc` |
| Governance | per-peer JSON config | all-org channel-config value |
| Contract | one shared contract | one per channel (behind a proxy) |
| Write-back | `mst-anchor-status` **user** chaincode via Gateway | `mstscc` **system** chaincode, peer-role gated |
| Config changes | restart the daemon | hot-reloaded live |
| Works against upstream peer | yes | no (needs this fork's binary + the capability) |

Both drive the *same* `capture` + `outbox` + `sender`.

## 7. Build, run, test, verify

```bash
# Contract (now behind a proxy) + cross-language gate
(cd mst/anchor-contracts && npm install && npm run build && npx hardhat test)  # incl. the upgrade test
(cd mst/testvectors/reference-ts && npm install && npm test)

# Live EVM integration (deploys impl + proxy; point at a running node)
(cd mst/anchor-contracts && npx hardhat node &)                                  # terminal 1
(cd mst/relay && MST_EVM_RPC=http://127.0.0.1:8545 go test -run TestIntegration ./evm/)

# Fabric-side unit tests for the new surfaces
go test ./common/channelconfig/ ./common/capabilities/ ./core/scc/mstscc/ \
        ./internal/pkg/mstanchor/ ./internal/peer/mst/ ./internal/configtxgen/...

# Build the peer + prove no proto-registration panic
go build -o /tmp/peer ./cmd/peer && /tmp/peer version

# Fabric's own lint — PINNED tools (see gotchas)
PATH=$(go env GOPATH)/bin:$PATH ./scripts/golinter.sh
```

To *operate* it (deploy the proxy, set the channel config + capability, opt the peer
into `mstscc`, verify a real tx), follow [`SETUP.md`](SETUP.md) and
[`deploy/README.md`](deploy/README.md).

## 8. Gotchas a newcomer will hit (new approach)

- **The two-proto rule still holds.** `mst/relay/gwsource` (apiv2) must never be
  imported by peer-linked code; `github.com/hyperledger/fabric` (old protos) must
  never be imported by the relay module. Anything shared goes through `txmodel`.
- **The `MSTAnchor` value needs the `V2_5_MSTANCHOR` capability.** `channelconfig` and
  `configtxgen` both reject the value without it; enable the capability only once
  every peer on the channel runs the MST binary.
- **NodeOUs is a hard precondition.** The peer-role write-back gate fails *closed*
  without NodeOUs enabled on the channel MSPs — write-backs are silently rejected.
  `peer mst preflight` and a startup warning surface this; `mst.writeback.*` must be
  the peer's own node signcert, not a client cert.
- **The proxy admin is the trust cost.** With the default deployer-key ProxyAdmin, a
  single key can upgrade the implementation and rewrite anchors. Use a
  timelock+multisig admin for anything real.
- **Re-vendor after non-test `mst/relay/evm` source changes.** The fabric root vendors
  the relay packages; changing `client.go`/`calldata.go` needs `go mod vendor` to
  re-sync (test files are not vendored).
- **Refresh `abi/` after contract changes.** `npm run build` regenerates
  `abi/MSTAnchor.json` and `abi/TransparentUpgradeableProxy.json`, which the Go
  integration test reads to deploy.
- **Lint uses pinned tools** (`gofumpt v0.1.0`, not `@latest`) — format with the
  pinned version or you'll wrongly reformat upstream files.
- **`MST_RELAYER_KEY` / `MST_OWNER_KEY` are env-only** by design — never in a config
  file.

## 9. What is intentionally NOT solved (current caveats)

See [`PHASE-1.5.md`](PHASE-1.5.md) for the full list. In brief:

- **Anchor status is an attestation, not a proof** — narrowed to peer-role writers;
  truth is established off-ledger with `peer mst verify`.
- **All endorsing peers must run the MST binary** — now an explicit capability gate,
  not a silent failure; scope write-back endorsement to the MST-running org(s) for
  mixed networks.
- **Anchors are governance-mutable via the proxy admin** — the trade-off for a stable
  address; harden with a timelock+multisig admin.
- **Cross-chain migration still splits history**; **MST reorgs** can invalidate a
  recorded hash (mitigated by confirmations); **batched write-back** and a
  **contract-history registry** are future work.
- **No single fully-automated live-network e2e** — covered by layered automation
  (simulated pipeline e2e, real-chain EVM integration incl. the proxy + allowlist, CLI
  unit tests, hardhat) plus the manual walkthrough in [`deploy/README.md`](deploy/README.md).

## 10. Glossary (new-approach additions)

- **Channel-config MST value** — the all-org-agreed `MSTAnchor` value in the
  Application group; the authoritative record of whether/how a channel anchors.
- **`V2_5_MSTANCHOR` capability** — the application capability gating the `MSTAnchor`
  value; makes "all peers patched" an explicit upgrade gate.
- **System chaincode (`mstscc`)** — the built-in, channel-gated write-back target;
  peer-role authorized; the consensus-backed anchor-status ledger fact.
- **Peer-role gate** — the `MSPRole{PEER}` check on `RecordAnchor`; requires NodeOUs.
- **Transparent proxy** — the OZ upgradeable proxy in front of `MSTAnchor`; gives a
  stable per-channel address at the cost of a governed upgrade authority.
- **Hot-reload** — the discovery loop restarting a channel's pipeline when its
  governed config changes (live reconfiguration).
- **Preflight** — `peer mst preflight`, the pre-config-update check against the live
  chain (contract deployed, chain-id match, NodeOUs).

*(For the unchanged terms — commitment, canonical encoding, outbox, capture mode,
batch strategy, cadence, inclusion proof — see the Phase 1 glossary; they are
identical.)*
