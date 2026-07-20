# Merging newer Fabric versions into this fork

The MST integration is deliberately additive. This document is the complete
inventory of what touches upstream Fabric and the procedure for pulling in a
new Fabric release.

## Coupling surface (everything that is not purely additive)

| File | Nature of change | Merge risk |
|---|---|---|
| `internal/peer/node/start.go` | **+18/−2 lines** in one location of `serve()`: capture the gateway server handle, start/stop the mstanchor service around the signal handlers | The only real conflict candidate. If upstream rewrites `serve()`, re-apply the block by hand (it is small and self-describing) |
| `sampleconfig/core.yaml` | `mst:` section **appended at EOF** | Conflicts only if upstream also appends at EOF; trivial |
| `go.mod` / `go.sum` | added `require`/`replace` block for the `mst/` modules; transitive bumps from go-ethereum | Never hand-merge: take upstream's version, re-add our block, re-run tidy (below) |
| `vendor/` | vendored mst deps + version bumps | Never hand-merge: regenerate (below) |

Everything else is **new, MST-owned territory** upstream will never touch:
`mst/**`, `internal/pkg/mstanchor/**`, `internal/peer/node/mst.go`,
`.github/workflows/mst.yml`.

### Phase 1.5 additional coupling (per-channel / channel-config / system chaincode)

Phase 1.5 ([`PHASE-1.5.md`](PHASE-1.5.md)) touches **hot upstream directories**,
raising merge risk beyond the single-file Phase 1 surface. Each edit is kept as
thin as possible (a self-contained file plus a few registration lines):

| File | Nature of change | Merge risk |
|---|---|---|
| `common/channelconfig/application.go`, `api.go` | one `ApplicationProtos` field (`MSTAnchor *structpb.Value`), a capability-gated parse block, one accessor added to the `Application` interface | Moderate — re-apply if upstream reworks `ApplicationConfig`. Also requires the 3 `channelconfig.Application` counterfeiter mocks to carry `MSTAnchorConfig` (regenerate with `go generate`, or hand-add) |
| `common/capabilities/application.go`, `common/channelconfig/api.go` | one `V2_5_MSTANCHOR` capability const + provider field/method, one `MSTAnchor()` method on the `ApplicationCapabilities` interface | Moderate — additive, but adding an interface method requires the 5 `ApplicationCapabilities` mocks (`core/chaincode`, `core/chaincode/lifecycle`, `core/scc/lscc`, `core/committer/txvalidator`, `gossip/privdata`) to carry `MSTAnchor` (regenerate with `go generate`, or hand-add) |
| `common/channelconfig/mstanchor.go` | **new file** (config type + validation + `MSTAnchorValue` helper) | None (additive) |
| `internal/peer/node/start.go` | `mstscc` added to `builtinSCCs`, constructed next to `qscc`, appended to the deploy loop; captured into `mstEndorserServer`; listed in the `ValidatorCommitter.EmbeddedSystemChaincodes` set | Low — small, localized lines near the existing SCC/endorser/validator wiring |
| `core/chaincode/lifecycle/deployedcc_infoprovider.go` | `ValidatorCommitter` gains an `EmbeddedSystemChaincodes` set; `ValidationInfo` returns a default any-member endorsement policy for those names | Moderate — a hot validation path. A built-in SCC invoked via an ordered transaction (mstscc's write-back) has no lscc/_lifecycle definition, so v20 validation would mark it `INVALID_CHAINCODE` ("chaincode mstscc not found"). The set gives it a default policy so the peer's own endorsement validates. Re-apply if upstream reworks `ValidationInfo` |
| `core/scc/mstscc/**` | **new package** (the system chaincode) | None (additive) |
| `internal/configtxgen/genesisconfig/config.go`, `encoder/encoder.go` | one profile struct + one encode block for the MSTAnchor value | Low |
| `sampleconfig/core.yaml` | `mstscc: enable` under `chaincode.system` | Trivial |
| `sampleconfig/configtx.yaml` | commented `MSTAnchor` example under `Application` | Trivial |
| `mst/fabric-config/` + `replace` in `go.mod` | **patched fork** of `github.com/hyperledger/fabric-config` (a separate module, like `mst/relay`) that registers the `MSTAnchor` value in the protolator so `configtxlator` can decode/encode it | Moderate — see below |

The encoding choice — JSON inside a `wrapperspb.StringValue`, not a new
`fabric-protos` message — is deliberate: it keeps the channelconfig change to a
plain field and avoids touching the protos module at all.

**The `fabric-config` fork (configtxlator support).** `configtxlator` decodes
config through `fabric-config`'s protolator, which has a hardcoded switch of
known Application config values and no extension hook — so a custom value
(`MSTAnchor`) makes `configtxlator proto_decode`/`proto_encode` fail
(`Unknown Application ConfigValue name: MSTAnchor`), breaking every
configtxlator-based config update on an MST channel. The fix is a local fork of
the module at `mst/fabric-config/` (wired via a `replace` in `go.mod`, same
pattern as the `mst/*` modules) that adds the `MSTAnchor` case. Two subtleties
that matter on a merge/upgrade:
- The value is stored (in `common/channelconfig`) as a `wrapperspb.StringValue`,
  but the protolator registers a small **golang/protobuf v1** carrier
  (`MSTAnchorConfigValue`, wire-compatible: a single `value` string, tag 1). This
  is because fabric-config v0.1.0's protolator reflects via the legacy
  `proto.GetProperties` and cannot introspect v2 well-known types (`structpb` /
  `wrapperspb`) or `oneof`s. Keep the carrier a v1 message.
- Because it is a `replace` to a source module (not a bare vendored patch), the
  fix **survives `go mod vendor`**. If you bump the real `fabric-config` version
  upstream, re-apply the one-case patch onto the new fork, or drop the fork if a
  future fabric-config exposes an extension hook. The round-trip is guarded by
  `internal/configtxlator/rest/mstanchor_roundtrip_test.go`.

## Upstream APIs the embedded mode depends on

All long-stable core surfaces; verify they still exist after a merge (the
compiler will tell you):

- `core/peer.Peer.GetLedger` / `GetChannelsInfo`
- `common/ledger.Ledger.GetBlocksIterator` (blocking tail iterator; `Close()`
  interrupts, closed iterator returns `nil, nil`)
- `protoutil` unmarshalers + `protoutil.CreateSignedTx` +
  `internal/pkg/txflags.ValidationFlags`
- `core/endorser.Endorser`'s `ProcessProposal` method (embedded write-back
  endorses `RecordAnchor` against the local endorser in-process — the gateway's
  `Endorse` cannot be used because it plans endorsement via discovery, and the
  built-in `mstscc` has no `_lifecycle` metadata)
- `internal/pkg/gateway.Server`'s `Submit` / `CommitStatus` methods (embedded
  write-back orders the signed envelope and polls commit through them; both are
  independent of chaincode discovery metadata)
- `bccsp/utils.SignatureToLowS`

The **sidecar `mst-relayd` has zero dependence on the fabric module** — it
talks to any Fabric 2.5+ peer over the public Gateway API. If a future
Fabric release ever makes the embedded patch expensive to carry, the sidecar
keeps working against the unpatched upstream peer unchanged: that is the
built-in escape hatch.

## Merge procedure

```bash
git fetch upstream
git checkout -b merge-fabric-vX.Y.Z mst-integration-phase-1-part-1
git merge vX.Y.Z          # upstream release tag

# 1. Resolve go.mod: take upstream's content, then re-append our block:
#      require ( ...mst/canonical, ...mst/relay )
#      replace ( ...mst modules => ./mst/... )
# 2. Regenerate — never hand-edit go.sum or vendor/:
go mod tidy
go mod vendor
# 3. If start.go conflicted, re-apply the small mstanchor block
#    (search upstream serve() for handleSignals; our block sits just above
#    it — see internal/peer/node/mst.go for what it calls).

# 4. Verify:
go build ./cmd/...                          # everything still builds
go test ./internal/pkg/mstanchor/...        # embedded glue
go test ./internal/peer/node/...            # peer startup
go build -o /tmp/peer ./cmd/peer && /tmp/peer version   # no proto-registration panic
(cd mst/relay && go test ./...)             # pipeline modules (own go.mod, usually untouched)
(cd mst/canonical && go test ./...)         # vector gate
```

The `mst/**` Go modules have their own `go.mod`s and do not share the fabric
module's dependency graph, so upstream merges normally do not affect them at
all.

## Known dependency interaction

Adding go-ethereum to the fabric module graph bumped a handful of shared
transitive deps (grpc-gateway, fastcache, fsnotify, x/*) via Go MVS. After a
merge, `go mod tidy` recomputes this automatically; if upstream pins a
conflicting minimum, MVS picks the higher version — re-run fabric's own test
suite for the affected areas (`protoutil`, `internal/pkg/gateway`) as a
smoke check, as was done when the dependency was first introduced.
