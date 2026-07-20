# MST anchoring — local deployment and end-to-end walk (Phase 1.5)

This directory holds the local/e2e composition for the **new approach**:
per-channel contracts behind a proxy, channel-config governance, and the built-in
`mstscc` system chaincode. The core pipeline is fully tested WITHOUT any of this
(unit tests + synthetic blocks + a bare hardhat node); use this walk to see the
whole system move for real.

> **Legacy note.** The Phase 1 sidecar walk (shared contract, `mst-relayd`,
> `mst-anchor-status` user chaincode) is in git history. This walk is the embedded,
> channel-governed path.

## 1. MST side (EVM) — deploy this channel's contract behind a proxy

```bash
cd mst/anchor-contracts
npm install && npm run build
npx hardhat node                             # terminal 1: local EVM at :8545
npm run deploy:local                         # terminal 2: prints the PROXY address
```

`npm run deploy:local` writes `deployments/localhost.json` with the **proxy**
`address` (this is what the channel config points at), plus the `implementation` and
`proxyAdmin`. Record the proxy address. (Each channel gets its own contract; repeat
per channel.)

## 2. Fabric side — a network built from THIS fork, with NodeOUs

The embedded path needs this fork's peer binary and **NodeOUs enabled** on every org's
MSP. The quickest base is fabric-samples' test-network, run with peers built here:

```bash
# build this fork's peer image/binary and point the test-network at it
make peer                                    # or: go build -o build/bin/peer ./cmd/peer

git clone https://github.com/hyperledger/fabric-samples
cd fabric-samples/test-network
./network.sh up -ca                          # -ca gives NodeOUs-enabled MSPs
```

### 2a. Create the channel with the capability + MSTAnchor value

Add to the channel profile in `configtx.yaml` (see [SETUP.md](../SETUP.md) step 3):

```yaml
Capabilities:
    Application: &ApplicationCapabilities
        V2_5: true
        V2_5_MSTANCHOR: true
Application:
    Capabilities:
        <<: *ApplicationCapabilities
    MSTAnchor:
        Enabled: true
        ContractAddress: "0x<proxy-address-from-step-1>"
        ChainID: 1337
        CaptureMode: opt-in
        BatchStrategy: individual
        Confirmations: 1
```

Then create the channel (`./network.sh createChannel -c mychannel`). A vanilla peer
would refuse this channel — that's the capability gate working.

### 2b. Deploy the business chaincode (opt-in emitter)

```bash
./network.sh deployCC -c mychannel -ccn mst-example \
  -ccp ../../fabric/mst/fabric-chaincode/example-chaincode -ccl go
```

No write-back chaincode to deploy — the write-back target is the built-in `mstscc`
system chaincode, active automatically on this channel because the config enabled MST.

## 3. Enable the embedded relayer on the peer

In each anchoring peer's `core.yaml` (see [SETUP.md](../SETUP.md) step 4): enable
`chaincode.system.mstscc`, set `mst.enabled: true`, `mst.evm.rpcURL: http://127.0.0.1:8545`,
and `mst.writeback.*` to the peer's **own node signcert/key** (a NodeOUs peer
identity). Then:

```bash
export MST_RELAYER_KEY=ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80  # hardhat dev key 0
peer node start
```

(The `docker-compose.yml` and `Dockerfile.relayd` beside this file are the **legacy
Phase 1 sidecar** composition — EVM + `mst-relayd`. The embedded Phase 1.5 relayer
runs *inside* the peer, so there is no separate relayer container to start.)

## 4. Preflight, drive a transaction, and verify

```bash
# 0. sanity-check the channel config against the live chain BEFORE relying on it
peer mst preflight -C mychannel --rpc http://127.0.0.1:8545
#    [PASS] enabled / rpc endpoint / chain id / contract / nodeous

# 1. invoke the proof-enabled business tx (as org1)
peer chaincode invoke ... -n mst-example -c '{"function":"CreateAsset","Args":["asset-1","alice","100"]}'

# 2. watch capture -> submit -> confirm -> write-back
peer mst pipeline                    # relayer + per-channel outbox backlog by status

# 3. ask Fabric whether the tx is anchored (the mstscc ledger fact)
peer mst status <txid> -C mychannel

# 4. read the anchor straight from this channel's contract
peer mst onchain <txid> -C mychannel --rpc http://127.0.0.1:8545

# 5. independently verify against MST from the original data
peer mst verify <txid> -C mychannel --rpc http://127.0.0.1:8545
# -> MATCH (exit 0)
```

## 5. Exercise the new-approach behaviours

```bash
# Live reconfiguration: change a governed field and watch the pipeline hot-reload.
#   e.g. update the channel config CadenceMode per-tx -> batch (config-update tx),
#   then: peer mst channel-config -C mychannel     # shows the new value; the peer log
#   prints "channel MST config changed; reloading pipeline" within ~10s.

# In-place contract upgrade (address unchanged, history preserved):
(cd mst/anchor-contracts && NEW_CONTRACT=MSTAnchor npm run upgrade:local)
#   peer mst onchain <txid> ... still resolves — same proxy address.

# Relayer allowlist (only if the contract was deployed allowlist-enabled):
export MST_OWNER_KEY=<owner-key>
peer mst relayer add 0x<wallet> -C mychannel
```

## 6. Fault tolerance

Kill the peer mid-run, take the EVM node down and up — nothing is lost and nothing
double-anchors: that is the durable outbox + idempotent anchor doing their job. On
restart the relayer reconciles in-flight submissions and resumes from its per-channel
checkpoint.

---

**Coverage note.** This full-network walk is manual. The automated layers that cover
every component without it: the simulated pipeline e2e
(`internal/pkg/mstanchor/e2e_test.go`), the real-chain EVM integration test incl. the
proxy + relayer allowlist (`mst/relay/evm/integration_test.go`, run with
`MST_EVM_RPC`), the `peer mst` unit tests (`internal/peer/mst/*_test.go`), the
channelconfig/capability/mstscc unit tests, and the hardhat contract tests incl. the
in-place upgrade test. See [CLI.md](../CLI.md) for the coverage matrix.
