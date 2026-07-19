# MSTAnchor contracts

`MSTAnchor` is the on-chain anchor registry: it maps `fabricTxId → commitment`
(immutable per key) and records Merkle batch roots, emitting `Anchored` /
`RootAnchored` events. One contract is deployed per Fabric channel; the channel
config points at its address.

## Upgradeable-proxy architecture

`MSTAnchor` is deployed **behind an OpenZeppelin `TransparentUpgradeableProxy`**:

```
channel config ──► proxy address (stable, never changes)
                      │  delegatecall
                      ▼
                 MSTAnchor implementation  ◄── upgradeable in place
                      ▲
                 ProxyAdmin (owns upgrade rights)
```

- The **proxy address is what a channel anchors to** and never changes across
  implementation upgrades — so a bug fix or logic change does **not** force a new
  contract address and does **not** split the channel's anchor history.
- State (the anchor mapping) lives in the proxy's storage, so it survives
  upgrades. The implementation is `Initializable`; `initialize(bool,address[])`
  runs once in the proxy's storage (constructor is disabled on the
  implementation via `_disableInitializers()`).
- Storage layout is **append-only** (`__gap` reserved); never reorder or remove
  existing state variables in an upgrade. `npm run upgrade:*` uses the
  hardhat-upgrades plugin, which enforces this at upgrade time.

## ⚠️ Admin-key custody — read this

Upgradeability means the **ProxyAdmin owner can replace the implementation and
therefore alter, forge, or delete recorded anchors.** This is the trust cost of a
stable address. The anchor record is immutable **only while the proxy is not
upgraded to malicious logic**.

- The deploy script sets the ProxyAdmin owner to the **deployer key** (simplest;
  acceptable for dev/testnet). A single key that can rewrite every anchor is the
  weakest governance.
- **For production, transfer ProxyAdmin ownership to a timelock + multisig** and
  monitor upgrade events. That makes upgrades slow, multi-party, and observable
  instead of a single-key, instant capability. Verifiers should confirm the
  implementation was not swapped unexpectedly (pin an expected implementation
  address / watch upgrade events).

## Commands

```bash
npm install            # deps (incl. OpenZeppelin + hardhat-upgrades)
npm run build          # compile + export abi/ (used by the Go relay)
npm test               # hardhat tests (incl. the in-place upgrade test)

# Deploy the proxy + implementation. Records proxy/impl/admin in deployments/<network>.json.
ANCHOR_ALLOWLIST_ENABLED=true ANCHOR_RELAYERS=0x... npm run deploy:local   # or deploy:mst

# Upgrade the implementation in place (proxy address preserved). Requires the admin key.
NEW_CONTRACT=MSTAnchor npm run upgrade:local                               # or upgrade:mst
```

`npm run build` refreshes `abi/MSTAnchor.json` and
`abi/TransparentUpgradeableProxy.json`; the Go relay's `mst/relay/evm`
integration test reads both to deploy the proxy against a local node.
