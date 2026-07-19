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
| `internal/peer/node/start.go` | `mstscc` added to `builtinSCCs`, constructed next to `qscc`, appended to the deploy loop | Low — 3 small lines near the existing SCC wiring |
| `core/scc/mstscc/**` | **new package** (the system chaincode) | None (additive) |
| `internal/configtxgen/genesisconfig/config.go`, `encoder/encoder.go` | one profile struct + one encode block for the MSTAnchor value | Low |
| `sampleconfig/core.yaml` | `mstscc: enable` under `chaincode.system` | Trivial |
| `sampleconfig/configtx.yaml` | commented `MSTAnchor` example under `Application` | Trivial |

The encoding choice — JSON inside a `structpb.Value`, not a new `fabric-protos`
message — is deliberate: it keeps the channelconfig change to a plain field and
avoids touching the protos module at all.

## Upstream APIs the embedded mode depends on

All long-stable core surfaces; verify they still exist after a merge (the
compiler will tell you):

- `core/peer.Peer.GetLedger` / `GetChannelsInfo`
- `common/ledger.Ledger.GetBlocksIterator` (blocking tail iterator; `Close()`
  interrupts, closed iterator returns `nil, nil`)
- `protoutil` unmarshalers + `internal/pkg/txflags.ValidationFlags`
- `internal/pkg/gateway.Server`'s `Endorse` / `Submit` / `CommitStatus`
  methods (embedded write-back invokes them in-process)
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
