# MST Integration — Phase 1.5: Per-Channel Anchoring, Channel-Config Governance & the System Chaincode

Phase 1 (see [`README.md`](README.md)) anchors Fabric transactions onto **one
shared** `MSTAnchor` contract, is enabled **per peer** through `core.yaml`, and
records the anchor result back into Fabric through a **user chaincode**
(`mst-anchor-status`). Phase 1.5 keeps the same capture → outbox → sender core
untouched and changes the three surfaces around it so anchoring becomes a
first-class, channel-governed property:

1. **Per-channel contracts.** Each channel anchors to its **own** deployed
   `MSTAnchor` contract, so channels' anchor histories are isolated on the MST
   chain.
2. **Channel-config governance.** Whether a channel anchors — and to which
   contract — is a **channel configuration** value agreed by all orgs through
   the Application group's modification policy, not a per-peer flag.
3. **Anchor status as a ledger fact via a system chaincode.** The write-back
   target is a **built-in system chaincode** (`mstscc`), active only on channels
   that enabled anchoring, so the anchor record is a consensus-backed ledger
   fact every org can authoritatively query.

Phase 1.5 is **embedded-only**: a sidecar cannot run a built-in system chaincode
or honor channel-config governance. The Phase 1 sidecar (`mst-relayd`) remains
supported for the legacy shared-contract, user-chaincode flow.

## Architecture

```
 channel config  (Application group value "MSTAnchor", all-org agreed)
   ├─ enabled: true
   ├─ contractAddress: 0x…   (this channel's OWN contract)
   └─ chainID
        │  read at runtime by the peer
        ▼
 Business chaincode ── proofhelper.Emit("MSTProofRequest", …)   (opt-in, unchanged)
        │  block commits
        ▼
 capture ─► outbox (per channel) ─► sender ─► [MST] this channel's MSTAnchor contract
   (pipeline runs only for channels the config enables; evm.Binding pins the
    per-channel address onto the ONE shared relayer account / nonce sequence)
        │  Anchored
        ▼
 LoopbackWriteBack ─► [Fabric] mstscc.RecordAnchor  (system chaincode)
   • idempotent per fabric_tx_id     • only a PEER-role identity may submit
   • never emits events (echo-safe)  • rejected unless the channel enabled MST
```

## The three pieces

### 1. Channel-config value (`common/channelconfig`)

The channel's MST settings live in an **Application-group config value** under
the key `MSTAnchor` (`common/channelconfig/mstanchor.go`). The payload is a JSON
`{enabled, contractAddress, chainID}` carried inside a `structpb.Value` — a
deliberately additive encoding that avoids adding a message to the
`fabric-protos` module. `ApplicationConfig.MSTAnchorConfig()` exposes it; the
peer reads it via `Peer.GetApplicationConfig(channel).MSTAnchorConfig()`.

An **enabled** value with a malformed contract address is rejected when the
config is parsed, so misconfiguration surfaces at config-apply time, not at the
first failed anchor.

Operators declare it in `configtx.yaml` under the `Application` profile:

```yaml
Application:
    MSTAnchor:
        Enabled: true
        ContractAddress: "0xYourPerChannelContract…"
        ChainID: 1337
        CaptureMode: opt-in          # or "all"
        # IncludeChaincodes: [myapp] # "all" mode scope
        # ExcludeChaincodes: []
        BatchStrategy: individual    # or "merkle"
        Confirmations: 1
```

`configtxgen` encodes it (`internal/configtxgen/encoder`); turning it on or off
later is an ordinary channel config update, subject to the Application group's
modification policy (all-org agreement).

**The anchoring *policy* is channel-governed, not per-peer.** Because every peer
on a channel anchors the same transactions to the same contract, any per-peer
disagreement about *what/how/where* to anchor would produce inconsistent or
ambiguous anchoring that the idempotent contract cannot reconcile. So alongside
`Enabled`/`ContractAddress`, the following live in the channel config (one
all-org-agreed value) rather than each peer's `core.yaml`:

| Channel field | Governs |
|---|---|
| `CaptureMode` + `IncludeChaincodes` + `ExcludeChaincodes` | which transactions are anchored (scope) |
| `BatchStrategy` | on-chain representation + verification model (`individual`/`merkle`) |
| `Confirmations` | finality threshold before write-back |
| `Cadence*` (`CadenceMode`/`N`/`Interval`/`MaxWait`/`Cron`) | when the relayer flushes — the latency/gas trade-off, and the batch boundaries that shape `merkle` roots |
| `ChainID` | which MST chain the contract lives on |

Each field is optional and falls back to a fixed built-in default (never to
`core.yaml`), so peers cannot diverge even if a field is omitted. `core.yaml`
keeps only peer-local plumbing (RPC URL, relayer key, outbox path, workers,
cadence, gas tuning, metrics). `ChainID` is a validated assertion: a peer has one
RPC endpoint, so it refuses to anchor a channel whose `ChainID` does not match
the chain its node reports (`internal/pkg/mstanchor/service.go`). **Limitation:**
promoted fields are read when a channel's pipeline starts; changing one via a
later config update takes effect on pipeline restart (same as enablement today).

### 2. System chaincode (`core/scc/mstscc`)

`mstscc` is a built-in system chaincode — the same thin `RecordAnchor` /
`QueryAnchorStatus` / `IsAnchored` surface as the Phase 1 user chaincode, but
compiled into the peer and gated two ways:

- **Peer level:** deployed only if `chaincode.system.mstscc` is enabled in the
  peer's `core.yaml` (the peer opting in / running the MST-enabled binary).
- **Channel level:** every invoke is **rejected** unless the channel's
  `MSTAnchorConfig` has anchoring enabled. The SCC is therefore inert on
  channels that never turned anchoring on — which is what "the system chaincode
  is active only when the channel enables MST anchoring" means for a built-in
  SCC.

**Authorization** requires a **peer-role identity** (NodeOUs): only a peer
*node's* signing identity may submit `RecordAnchor`, not a client/user identity.
The SCC evaluates the transaction creator against an `MSPRole{PEER}` principal
using the channel MSP (`defaultRequirePeer`), so it honors the network's NodeOUs
configuration and fails closed if NodeOUs are not enabled. In embedded mode the
relayer therefore signs the write-back with the peer's own node identity
(`mst.writeback.*` must be the peer signcert). Reads (`QueryAnchorStatus`,
`IsAnchored`, `ListAnchors`, `CountAnchors`) are not identity-gated. The
`aclProvider` field and the injectable `requirePeer` seam allow tightening this
further (e.g. a named-relayer allowlist or M-of-N committee) without reshaping
the chaincode.

> **Precondition:** the channel's MSPs must have **NodeOUs enabled** so peer vs
> client identities can be distinguished; otherwise the peer-role check fails
> closed and no write-back is accepted.

**Echo-loop safe:** `mstscc` never calls `SetEvent`, and its name is always
added to the capture service's excluded-chaincodes set, so a write-back can
never re-enter the pipeline.

### 3. Per-channel EVM binding (`mst/relay/evm`, `internal/pkg/mstanchor`)

The embedded service keeps **one** `evm.Client` (one relayer account, one
serialized nonce sequence) and creates an `evm.Binding` per channel that pins
that channel's contract address (`Client.Bind`). Every submission still flows
through the single client's nonce lock, so N per-channel contracts do not cause
the head-of-line nonce collisions that N independent clients on one account
would. The service reads each channel's contract address and enablement from the
channel config in `startNewPipelines` / `startPipeline`
(`internal/pkg/mstanchor/service.go`); `core.yaml` keeps only peer-local
operational knobs (RPC URL/credentials, workers, cadence, outbox path, and the
`MST_RELAYER_KEY` env var).

## What is intentionally NOT solved (gaps)

- **"Deployed" is really built-in + gated.** A built-in SCC is always linked; it
  is not per-channel deployed. Every endorsing peer on an enabled channel must
  run the MST-enabled binary, or the write-back cannot be endorsed. This is a
  hard deployment prerequisite.
- **Anchor status is a claim, not a proof.** The SCC records that a submitter
  asserted a tx was anchored; it does not verify against the MST chain (a Fabric
  SCC cannot read EVM state without an oracle), so truth is still established
  off-ledger with `mst-verify` / `peer mst verify`. The peer-role gate narrows
  who can write to peer *nodes* (removing the "any channel member forges/squats"
  case), but a rogue member org's peer could still write a false record — a
  fully trustless design would need a named-relayer allowlist, multi-party
  attestation, or an EVM light-client oracle.
- **Config-update validation is minimal.** The value's address format is checked;
  nothing verifies the contract is deployed or that all peers are patched.
- **Phase 1 coexistence / migration is out of scope.** New channels use Phase
  1.5; existing channels stay on Phase 1 until explicitly migrated.
- **Contract rotation splits history.** A channel's contract is effectively
  immutable once anchoring begins; pointing the config at a new address splits
  the anchor history across contracts, and verifiers must know both.
- **MST reorg vs. recorded hash.** Confirmations mitigate but do not eliminate a
  reorg invalidating a recorded `evmTxHash`.

## Challenges & trade-offs

- **Higher upstream-merge risk.** Phase 1.5 edits hot upstream directories
  (`common/channelconfig`, `core/scc` + `builtinSCCs`, `internal/configtxgen`),
  where Phase 1 touched only `internal/peer/node/start.go`. The value is
  JSON-in-`structpb` and the SCC is self-contained precisely to keep those edits
  thin. See [`UPGRADING.md`](UPGRADING.md).
- **All endorsing peers must be patched.** Mixed networks with vanilla peers
  cannot satisfy an endorsement policy needing the SCC, and a vanilla peer/orderer
  will reject a channel config carrying the `MSTAnchor` value.
- **Governance is heavyweight.** Turning anchoring on/off is a channel config
  update under MAJORITY-admin (typically) policy, not a peer restart.
- **Operational surface grows.** N channels = N contracts to deploy, fund, and
  verify against; the shared relayer account is a single point of nonce
  serialization.
- **Write-backs consume ordering throughput.** Each `RecordAnchor` is a real
  channel transaction; in `all` capture mode this can be substantial (batched
  write-back is future work).
- **No sidecar for the new path.** Operators who valued relayer/peer process
  isolation keep it only on legacy Phase 1 channels.

## Files

| Area | Files |
|---|---|
| Channel-config value | `common/channelconfig/mstanchor.go`, `application.go`, `api.go` |
| System chaincode | `core/scc/mstscc/mstscc.go`; registered in `internal/peer/node/start.go` |
| Write-back → SCC | `internal/peer/node/mst.go`, `internal/pkg/mstanchor/writeback.go` |
| Per-channel EVM | `mst/relay/evm/client.go` (`Binding`), `internal/pkg/mstanchor/service.go`, `config.go` |
| Config tooling | `internal/configtxgen/genesisconfig/config.go`, `internal/configtxgen/encoder/encoder.go`, `sampleconfig/configtx.yaml` |
| Peer opt-in | `sampleconfig/core.yaml` (`chaincode.system.mstscc`) |
