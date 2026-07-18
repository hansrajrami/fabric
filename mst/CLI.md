# `peer mst` — operator commands for MST anchoring

The `peer mst` command group talks to the MST anchor-status system chaincode
(`mstscc`), a channel's MST configuration, the per-channel MST contract, and the
local relayer. It wraps what you would otherwise do with raw
`peer chaincode query -n mstscc …` calls and config-block fetches.

All commands take `-C/--channelID` and, optionally, `--peerAddresses` /
`--tlsRootCertFiles` (default: the local peer from `core.yaml`). Commands that
read the MST chain also accept `--rpc` (default: `mst.evm.rpcURL`).

## Query the anchor-status ledger fact (`mstscc`)

```bash
# Full status record for one transaction
peer mst status <txid> -C mychannel
#   fabric tx id : <txid>
#   anchor ref   : 0x… (the MST tx hash)
#   status       : CONFIRMED
#   recorded at  : 2026-07-18T… (unix)

# Quick boolean
peer mst is-anchored <txid> -C mychannel      # -> true / false

# Enumerate / count anchored transactions on the channel
peer mst list -C mychannel [--limit 50] [--json]
peer mst count -C mychannel                    # -> a number
```

`--json` emits the raw chaincode payload for `status`/`list`/`channel-config`.

## Inspect the channel's agreed MST policy

```bash
peer mst channel-config -C mychannel
#   enabled           : true
#   contract address  : 0x…
#   chain id          : 1337
#   capture mode      : opt-in (default)
#   batch strategy    : individual (default)
#   confirmations     : 1
#   cadence           : per-tx
```

Reads the channel configuration (via `cscc GetChannelConfig`), i.e. the value
all orgs agreed on — not any single peer's `core.yaml`.

## Verify a transaction end-to-end

```bash
peer mst verify <txid> -C mychannel
#   commitment        : 0x…
#   fabric ledger fact: recorded (ref 0x…)
#   on-chain anchor   : commitment 0x…, block N
#   MATCH
```

`verify` fetches the transaction from the ledger, recomputes its commitment,
reads the anchor from the channel's contract, and reports **MATCH**/**NO-MATCH**
— and whether the anchor status is recorded on Fabric. Exit code: `0` = MATCH,
`1` = NO-MATCH / not anchored. Opted-in transactions verify against their
declared payload; anchor-all transactions against the empty canonical payload.

## Read the MST contract directly

```bash
peer mst onchain <txid> -C mychannel
#   contract    : 0x…
#   commitment  : 0x…
#   block number: N
#   evm time    : …
```

Bypasses Fabric and calls `getAnchor` on the channel's contract.

## Check the local relayer

```bash
peer mst pipeline
#   relayer      : ok
#   channel mychannel
#     mst_outbox_entries{status="PENDING"} 0
#     mst_outbox_entries{status="DONE"} 42
#   relayer account
#     mst_relayer_balance_gwei …
```

Reads this peer's relayer metrics endpoint (`mst.metricsAddr` in `core.yaml`).
Requires the MST metrics endpoint to be enabled.

## Manage the contract's relayer allowlist (owner only)

```bash
# Requires the contract owner's EVM key in MST_OWNER_KEY (or MST_RELAYER_KEY)
export MST_OWNER_KEY=0x<owner-private-key>
peer mst relayer add    0x<wallet> -C mychannel
peer mst relayer remove 0x<wallet> -C mychannel
```

Calls `setRelayer` on the channel's contract to add/remove a wallet from its
relayer allowlist. Only has an effect if the contract was deployed with the
allowlist enabled. See [`PHASE-1.5.md`](PHASE-1.5.md) for the allowlist model.

## Notes

- `status`/`is-anchored`/`list`/`count`/`channel-config` need only a running,
  reachable peer — no MST RPC access.
- `verify`/`onchain`/`relayer` additionally reach the MST chain (`--rpc` or
  `mst.evm.rpcURL`); reads use a throwaway key, only `relayer` signs (owner key).
- These are convenience wrappers; the underlying calls remain available via
  `peer chaincode query -n mstscc` and the standalone `mst-verify` tool.
