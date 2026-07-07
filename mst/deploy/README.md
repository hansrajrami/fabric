# MST anchoring — local deployment and end-to-end walk

This directory holds the local/e2e composition. The core pipeline is fully
tested WITHOUT any of this (unit tests + synthetic blocks + a bare hardhat
node); use this walk to see the whole system move for real.

## 1. MST side (EVM)

```bash
cd mst/anchor-contracts
npm install
npx hardhat node                             # terminal 1: local EVM at :8545
npm run deploy:local                         # terminal 2: prints the contract address
```

Or via docker compose (EVM + relayer together):

```bash
export MST_RELAYER_KEY=ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80  # hardhat dev key 0
mkdir -p mst/deploy/config && cp mst/deploy/mst-relayd.example.json mst/deploy/config/mst-relayd.json
# edit config/mst-relayd.json: contract address, fabric paths
docker compose -f mst/deploy/docker-compose.yml up --build
```

## 2. Fabric side

Any Fabric 2.5 network works. The quickest is fabric-samples' test-network:

```bash
git clone https://github.com/hyperledger/fabric-samples
cd fabric-samples/test-network
./network.sh up createChannel -c mychannel -ca

# deploy the demo business chaincode (opt-in emitter)
./network.sh deployCC -c mychannel -ccn mst-example \
  -ccp ../../fabric/mst/fabric-chaincode/example-chaincode -ccl go

# deploy the write-back chaincode (non-anchorable; restrict writes)
./network.sh deployCC -c mychannel -ccn mst-anchor-status \
  -ccp ../../fabric/mst/fabric-chaincode/anchor-status -ccl go
```

Point `mst-relayd.json`'s `fabric` section at a peer of the network (TLS CA,
relayer MSP cert/key from the test-network's organizations tree, channel
`mychannel`, `anchorStatusChaincode: mst-anchor-status` — the relayer
excludes it from capture automatically).

## 3. Run the relayer

```bash
export MST_RELAYER_KEY=<hex key funded on the EVM node>
cd mst/relay && go run ./cmd/mst-relayd --config /path/to/mst-relayd.json
```

## 4. Drive a transaction and verify it

```bash
# invoke the proof-enabled business tx (from test-network, as org1)
peer chaincode invoke ... -n mst-example -c '{"function":"CreateAsset","Args":["asset-1","alice","100"]}'

# watch the relayer log: captured -> submitted -> confirmed -> written back
curl -s localhost:9464/metrics    # backlog by status

# ask Fabric whether the tx is anchored
peer chaincode query -n mst-anchor-status -c '{"function":"QueryAnchorStatus","Args":["<txid>"]}'

# independently verify against MST from the original data
go run ./cmd/mst-verify \
  --tx-id <txid> --channel mychannel --chaincode mst-example \
  --block <block> --timestamp <channel-header-ts> \
  --payload payload.json \
  --rpc http://127.0.0.1:8545 --contract <MSTAnchor address>
# -> MATCH (exit 0)
```

Kill the relayer mid-run, take the EVM node down and up — nothing is lost
and nothing double-anchors: that is the durable outbox + idempotent anchor
doing their job.
