# MST Anchoring — Setup Guide

Operator-oriented, step-by-step setup for both deployment modes. For
architecture and design rationale see [README.md](README.md); for the local
demo walk see [deploy/README.md](deploy/README.md); for upgrading the Fabric
base see [UPGRADING.md](UPGRADING.md).

---

## 0. What you are deploying

| Piece | Where | Purpose |
|---|---|---|
| `MSTAnchor` contract | MST chain (EVM) | public record of commitments / batch roots |
| `mst-anchor-status` chaincode | every anchoring channel | write-back target ("is tx X anchored?") |
| business chaincodes + `proofhelper` | your channels | opt-in: emit `MSTProofRequest` per transaction (not needed in capture mode `all`) |
| the relayer | **sidecar** `mst-relayd` daemon **or** **embedded** in the patched peer | capture → outbox → anchor → write-back |
| `mst-verify` / `mst-proof` CLIs | anywhere | independent verification / inclusion-proof export |

## 1. Prerequisites

- Go 1.24+, Node 22+ (contract toolchain), a Fabric 2.5 network.
- An MST (EVM) endpoint: any JSON-RPC URL. For dev: `cd mst/anchor-contracts && npm install && npx hardhat node`.
- A funded EVM account for the relayer (it pays gas). Keep its key OUT of
  files — both modes read it from the `MST_RELAYER_KEY` env var only.

## 2. Deploy the anchor contract (once per MST network)

```bash
cd mst/anchor-contracts
npm install && npm run build
# optional allowlist: ANCHOR_ALLOWLIST_ENABLED=true ANCHOR_RELAYERS=0xRelayerAddr
MST_RPC_URL=https://<mst-rpc> MST_CHAIN_ID=<id> MST_RELAYER_KEY=<hex> npm run deploy:mst
# -> prints the MSTAnchor address; record it for the relayer config
```

## 3. Deploy the chaincodes (per channel)

```bash
# write-back target (required for Fabric-side acknowledgment)
./network.sh deployCC -c mychannel -ccn mst-anchor-status \
  -ccp .../mst/fabric-chaincode/anchor-status -ccl go
# optionally restrict writers: set MST_RELAYER_MSPID=Org1MSP on the chaincode
# process, and use an endorsement policy for the real guard.

# your business chaincode: import proofhelper and emit per proof-enabled tx:
#   proofhelper.New().AddString("asset_id", id).AddInt64("value", v).Emit(ctx.GetStub())
# (skip entirely if you run captureMode "all")
```

## 4A. Mode A — sidecar daemon (`mst-relayd`)

Runs next to a peer; talks to it over the Gateway API; works with an
UNPATCHED upstream peer.

```bash
cd mst/relay && go build ./cmd/mst-relayd ./cmd/mst-verify ./cmd/mst-proof
cp ../deploy/mst-relayd.example.json /etc/mst-relay/mst-relayd.json   # edit it
export MST_RELAYER_KEY=<hex>
./mst-relayd --config /etc/mst-relay/mst-relayd.json
```

Config essentials (`mst-relayd.example.json` documents every field):
- `fabric`: peer endpoint + TLS CA, the relayer's Fabric MSP cert/key, the
  channel, `anchorStatusChaincode` (auto-excluded from capture).
- `evm`: `rpcURL`, `contractAddress`, `confirmations`, `minBalanceGwei`.
- `outbox`: default embedded LevelDB at `outboxPath`; or `type: couchdb`
  with server URL + database.
- `sender`: `batchStrategy` + cadence (section 5).
- One daemon per channel (each with its own outbox path/database).

## 4B. Mode B — embedded in the peer binary

Build the peer from THIS fork; everything lives in the peer process.

```bash
make peer     # or: go build ./cmd/peer
```

`core.yaml` (full commented reference in `sampleconfig/core.yaml`):

```yaml
mst:
    enabled: true                       # default false = vanilla peer
    outboxPath: /var/hyperledger/production/mst-outbox
    channels: []                        # empty = all joined channels
    captureMode: opt-in                 # or: all
    anchorStatusChaincode: mst-anchor-status
    evm:
        rpcURL: https://<mst-rpc>
        contractAddress: "0x..."
        confirmations: 1
        minBalanceGwei: 100000000       # alert threshold; 0 = off
    sender:
        batchStrategy: individual       # or: merkle
        cadenceMode: per-tx             # batch | interval | cron
    writeback:                          # Fabric identity used for RecordAnchor
        mspID: Org1MSP
        certPath: /etc/hyperledger/mst/relayer-cert.pem
        keyPath: /etc/hyperledger/mst/relayer-key.pem
    metricsAddr: ":9464"
```

Then `MST_RELAYER_KEY=<hex> peer node start`. Requirements: write-back needs
`peer.gateway.enabled` (default true); the outbox backend auto-follows
`ledger.state.stateDatabase` (CouchDB peers keep it on their CouchDB server
in `mst_outbox_<channel>` databases). Misconfiguration fails peer startup
with a clear error; MST outages at runtime never affect the peer.

## 5. Choosing capture, batching, and cadence

| Setting | Options | Notes |
|---|---|---|
| `captureMode` | `opt-in` (default) / `all` | `all` anchors EVERY valid tx (non-opted ones with the empty payload hash — existence proof); scope with `includeChaincodes`; gas scales with traffic, use batching |
| `batchStrategy` | `individual` (default) / `merkle` | individual: one record per tx, flushes share one EVM tx (~40% gas saving). merkle: one root per flush (~95% saving); verifiers need inclusion proofs |
| `cadenceMode` | `per-tx` / `batch` (+`cadenceN`, `cadenceMaxWait`) / `interval` / `cron` (+`cadenceCron`, e.g. `"0 * * * *"`) | WHEN flushes happen; independent of strategy |

Sane pairings: low-latency compliance → `opt-in` + `individual` + `per-tx`.
High-volume audit trail → `all` + `merkle` + `cron "0 * * * *"`.

## 6. Verify it works

```bash
# invoke a proof-enabled tx, then:
curl -s localhost:9464/metrics          # PENDING should drain to DONE
peer chaincode query -n mst-anchor-status \
  -c '{"function":"QueryAnchorStatus","Args":["<txid>"]}'

# independent verification (individual strategy):
mst-verify --tx-id <txid> --channel mychannel --chaincode <cc> \
  --block <n> --timestamp <channel-header-ts> --payload payload.json \
  --rpc <mst-rpc> --contract <MSTAnchor>          # exit 0 = MATCH
# captureMode "all", non-opted tx: use --payload-hex 0x00000000

# merkle strategy: export the inclusion proof from the outbox, then verify:
mst-proof --outbox /var/.../mst-outbox/mychannel --tx-id <txid> > proof.json
mst-verify ... --batch-proof proof.json
```

## 7. Operate

- **Metrics** (`metricsAddr`, Prometheus text): `mst_outbox_entries{status=…}`,
  `mst_outbox_oldest_active_age_seconds`, `mst_outbox_quarantined_total`,
  `mst_relayer_balance_gwei`. Alert on growing PENDING/oldest-age (MST needs
  attention; Fabric is unaffected) and on the balance gauge.
- **Funding**: set `minBalanceGwei`; the relayer logs an error every minute
  while underfunded and recovers automatically once topped up.
- **Crash/outage recovery is automatic**: the durable outbox + idempotent
  contract guarantee nothing is lost and nothing double-anchors; on restart
  the relayer reconciles in-flight submissions and resumes from its
  checkpoint (including blocks committed while it was down).
- **The one log line needing a human**: `anchor exists with mismatched
  commitment` — an on-chain anchor disagrees with local capture; it is never
  retried and must be investigated.

## 8. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Peer refuses to start with `mstanchor:` error | incomplete `mst:` config or missing `MST_RELAYER_KEY` — intentional fail-fast |
| Entries stuck PENDING, balance gauge low/absent | relayer account out of gas, or MST RPC unreachable (check logs; backoff retries automatically) |
| `mst.anchorStatusChaincode requires the embedded gateway` | enable `peer.gateway.enabled` + discovery, or unset the chaincode (anchoring works without write-back) |
| Quarantine count rising | a chaincode emits malformed `MSTProofRequest` payloads — inspect the `q/` records; fix the emitter (use `proofhelper`) |
| `mst-proof: anchored individually (no batch)` | the tx was anchored with `batchStrategy: individual` — verify it directly, no proof file needed |
