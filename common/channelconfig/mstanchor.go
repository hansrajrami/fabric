/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package channelconfig

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// MSTAnchorKey is the Application-group config value key carrying the
// channel's MST anchoring settings. The value is a wrapperspb.StringValue
// whose payload is the JSON encoding of MSTAnchorConfig (see application.go),
// deliberately kept as an opaque JSON blob so the fork does not have to add a
// message to the fabric-protos module. A StringValue (not structpb.Value) is
// used so configtxlator's protolator can decode/encode it. A peer or orderer
// that does not know this key (i.e. an unpatched binary) will reject a channel
// config that sets it — participation therefore requires every node on the
// channel to run the MST-enabled binary, which is the intended all-nodes-agree
// constraint.
const MSTAnchorKey = "MSTAnchor"

// MSTAnchorConfig is the channel-level MST anchoring configuration, agreed by
// all organizations through the Application group's modification policy. It is
// the authoritative, consensus-backed record of whether a channel anchors to
// MST and which per-channel contract it uses — replacing the per-peer
// core.yaml enablement of Phase 1.
type MSTAnchorConfig struct {
	// Enabled turns MST anchoring on for the channel. When false (or the value
	// is absent), the channel behaves exactly like a non-anchoring channel and
	// the mst system chaincode is inert.
	Enabled bool `json:"enabled"`
	// ContractAddress is this channel's OWN deployed MSTAnchor contract
	// (0x + 40 hex). Each channel anchors to a dedicated contract so channels'
	// anchor histories stay isolated on the MST chain. The operator deploys the
	// contract out of band and records its address here at channel creation.
	ContractAddress string `json:"contractAddress"`
	// ChainID is the MST EVM chain id the contract lives on. It is validated
	// against the chain the peer is actually connected to; a peer refuses to
	// anchor a channel whose ChainID does not match its node (0 = skip the
	// check and trust the peer's connected chain).
	ChainID uint64 `json:"chainID"`

	// The following fields are channel-governed anchoring policy: keeping them
	// here (rather than in each peer's core.yaml) guarantees every peer on the
	// channel anchors the same transactions, the same way, to the same chain —
	// per-peer divergence in these would produce inconsistent or ambiguous
	// anchoring that the idempotent contract cannot reconcile. All are optional;
	// an omitted field falls back to the built-in default, never to core.yaml.

	// CaptureMode selects which transactions are anchored: "opt-in" (default,
	// only MSTProofRequest emitters) or "all" (every valid transaction).
	CaptureMode string `json:"captureMode,omitempty"`
	// IncludeChaincodes restricts CaptureMode "all" to these chaincodes (empty =
	// every chaincode). Ignored in opt-in mode.
	IncludeChaincodes []string `json:"includeChaincodes,omitempty"`
	// ExcludeChaincodes are never captured (in addition to the mst system
	// chaincode, which is always excluded as the echo-loop guard).
	ExcludeChaincodes []string `json:"excludeChaincodes,omitempty"`
	// BatchStrategy selects the on-chain representation: "individual" (default,
	// one record per tx) or "merkle" (one root per flush; verifiers need
	// inclusion proofs). This is the channel's verification model, so it must be
	// uniform across peers.
	BatchStrategy string `json:"batchStrategy,omitempty"`
	// Confirmations is the number of MST confirmations required before the
	// anchor is written back to Fabric (0 = the relayer's default). Sets one
	// consistent finality guarantee for the channel.
	Confirmations uint64 `json:"confirmations,omitempty"`

	// Cadence controls WHEN the relayer flushes accumulated anchors to MST,
	// and therefore how entries are grouped into each on-chain submission — the
	// latency/gas trade-off, and (with BatchStrategy "merkle") the batch
	// boundaries that determine each root. It is channel-governed so peers
	// produce comparable batching rather than diverging roots.
	//
	// CadenceMode: "per-tx" (default, flush whatever is due), "batch" (flush at
	// CadenceN entries or CadenceMaxWait), "interval" (flush every
	// CadenceInterval), or "cron" (flush on CadenceCron).
	CadenceMode string `json:"cadenceMode,omitempty"`
	// CadenceN is the batch threshold for "batch" mode.
	CadenceN int `json:"cadenceN,omitempty"`
	// CadenceInterval is the flush period for "interval" mode (a Go duration
	// string, e.g. "30s").
	CadenceInterval string `json:"cadenceInterval,omitempty"`
	// CadenceMaxWait caps how long an entry waits under "batch" mode even if
	// CadenceN is never reached (a Go duration string; empty = the relayer's
	// default).
	CadenceMaxWait string `json:"cadenceMaxWait,omitempty"`
	// CadenceCron is a 5-field cron expression for "cron" mode (e.g. "0 * * * *"
	// hourly). Its syntax is validated by the peer at pipeline start, not here.
	CadenceCron string `json:"cadenceCron,omitempty"`
}

// Allowed values for the channel-governed enum fields. These literals are kept
// in sync by hand with mst/relay/capture.Mode and mst/relay/sender.BatchStrategy
// — channelconfig is a low-level core package and must not import the relay
// module (which would pull go-ethereum into its dependency graph).
const (
	mstCaptureModeOptIn    = "opt-in"
	mstCaptureModeAll      = "all"
	mstBatchStrategyIndiv  = "individual"
	mstBatchStrategyMerkle = "merkle"
)

// mstCadenceModes are the accepted CadenceMode values (kept in sync by hand
// with mst/relay/sender.CadenceMode — see the enum comment above).
var mstCadenceModes = map[string]bool{
	"": true, "per-tx": true, "batch": true, "interval": true, "cron": true,
}

// MSTAnchorValue returns the Application-group config value carrying the given
// MST anchoring configuration, for use by config tooling (configtxgen) and
// tests. It is a value for /Channel/Application. The config is stored as a
// JSON string inside a wrapperspb.StringValue under the MSTAnchor key. A
// StringValue (single scalar field, no oneof) is chosen over structpb.Value so
// configtxlator's protolator — which cannot render oneof fields — can decode and
// encode config blocks/updates on MST-enabled channels.
func MSTAnchorValue(cfg *MSTAnchorConfig) (*StandardConfigValue, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "marshal MSTAnchor config")
	}
	return &StandardConfigValue{
		key:   MSTAnchorKey,
		value: wrapperspb.String(string(raw)),
	}, nil
}

// unmarshalMSTAnchorConfig decodes and validates the JSON payload carried in
// the MSTAnchor config value. A nil/empty payload yields (nil, nil): the
// channel simply has no MST configuration.
func unmarshalMSTAnchorConfig(raw []byte) (*MSTAnchorConfig, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	cfg := &MSTAnchorConfig{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, errors.Wrap(err, "invalid MSTAnchor config value")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate enforces the shape a peer relies on before it will start anchoring
// a channel. Enablement without a well-formed contract address is rejected so
// a misconfiguration is visible when the config is applied, not later when the
// first transaction fails to anchor.
func (c *MSTAnchorConfig) validate() error {
	switch c.CaptureMode {
	case "", mstCaptureModeOptIn, mstCaptureModeAll:
	default:
		return errors.Errorf("MSTAnchor captureMode must be %q or %q, got %q", mstCaptureModeOptIn, mstCaptureModeAll, c.CaptureMode)
	}
	switch c.BatchStrategy {
	case "", mstBatchStrategyIndiv, mstBatchStrategyMerkle:
	default:
		return errors.Errorf("MSTAnchor batchStrategy must be %q or %q, got %q", mstBatchStrategyIndiv, mstBatchStrategyMerkle, c.BatchStrategy)
	}
	if !mstCadenceModes[c.CadenceMode] {
		return errors.Errorf("MSTAnchor cadenceMode must be one of per-tx/batch/interval/cron, got %q", c.CadenceMode)
	}
	if err := validateOptionalDuration("cadenceInterval", c.CadenceInterval); err != nil {
		return err
	}
	if err := validateOptionalDuration("cadenceMaxWait", c.CadenceMaxWait); err != nil {
		return err
	}
	if err := c.validateCadenceConsistency(); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if err := validateEVMAddress(c.ContractAddress); err != nil {
		return errors.Wrap(err, "MSTAnchor is enabled but contractAddress is invalid")
	}
	if isZeroEVMAddress(c.ContractAddress) {
		return errors.New("MSTAnchor is enabled but contractAddress is the zero address")
	}
	return nil
}

// validateCadenceConsistency rejects a cadence mode whose required companion
// field is missing, so a mode that could never flush deterministically (e.g.
// "interval" with no interval) is caught at config-apply time rather than
// silently defaulting at pipeline start. The enum and duration formats are
// already checked in validate; this covers the cross-field requirements.
func (c *MSTAnchorConfig) validateCadenceConsistency() error {
	if c.CadenceN < 0 {
		return errors.Errorf("MSTAnchor cadenceN must not be negative, got %d", c.CadenceN)
	}
	switch c.CadenceMode {
	case "batch":
		if c.CadenceN <= 0 {
			return errors.New("MSTAnchor cadenceMode \"batch\" requires a positive cadenceN")
		}
	case "interval":
		if c.CadenceInterval == "" {
			return errors.New("MSTAnchor cadenceMode \"interval\" requires cadenceInterval")
		}
	case "cron":
		if c.CadenceCron == "" {
			return errors.New("MSTAnchor cadenceMode \"cron\" requires cadenceCron")
		}
	}
	return nil
}

// validateOptionalDuration rejects a non-empty duration string that Go cannot
// parse, so a malformed cadence duration is caught when the config is applied.
func validateOptionalDuration(field, value string) error {
	if value == "" {
		return nil
	}
	if _, err := time.ParseDuration(value); err != nil {
		return errors.Wrapf(err, "MSTAnchor %s is not a valid duration", field)
	}
	return nil
}

// isZeroEVMAddress reports whether addr is the all-zero 20-byte address
// (0x0000…0000), which is well-formed but never a real deployment. Assumes addr
// already passed validateEVMAddress. The comparison is case-insensitive on the
// hex body (all zeros, so case is moot) and tolerant of an absent 0x only in
// theory — callers validate the format first.
func isZeroEVMAddress(addr string) bool {
	return strings.EqualFold(addr, "0x0000000000000000000000000000000000000000")
}

// validateEVMAddress checks a 0x-prefixed 20-byte hex address. It does not
// (and cannot, from inside Fabric) verify that the contract is actually
// deployed — that remains an operational precondition (see `peer mst preflight`).
func validateEVMAddress(addr string) error {
	if !strings.HasPrefix(addr, "0x") {
		return errors.New("contract address must be 0x-prefixed")
	}
	if len(addr) != 42 {
		return errors.Errorf("contract address must be 0x + 40 hex chars, got %d chars", len(addr))
	}
	if _, err := hex.DecodeString(addr[2:]); err != nil {
		return errors.Wrap(err, "contract address is not hex")
	}
	return nil
}
