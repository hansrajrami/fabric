/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package channelconfig

import (
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/types/known/structpb"
)

// MSTAnchorKey is the Application-group config value key carrying the
// channel's MST anchoring settings. The value is a wrapperspb.BytesValue
// whose payload is the JSON encoding of MSTAnchorConfig (see application.go),
// deliberately kept as an opaque JSON blob so the fork does not have to add a
// message to the fabric-protos module. A peer or orderer that does not know
// this key (i.e. an unpatched binary) will reject a channel config that sets
// it — participation therefore requires every node on the channel to run the
// MST-enabled binary, which is the intended all-nodes-agree constraint.
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
	// ChainID is the MST EVM chain id the contract lives on (0 = let the peer's
	// operational config / node decide).
	ChainID uint64 `json:"chainID"`
}

// MSTAnchorValue returns the Application-group config value carrying the given
// MST anchoring configuration, for use by config tooling (configtxgen) and
// tests. It is a value for /Channel/Application. The config is stored as a
// JSON string inside a structpb.Value under the MSTAnchor key.
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
		value: structpb.NewStringValue(string(raw)),
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
	if !c.Enabled {
		return nil
	}
	if err := validateEVMAddress(c.ContractAddress); err != nil {
		return errors.Wrap(err, "MSTAnchor is enabled but contractAddress is invalid")
	}
	return nil
}

// validateEVMAddress checks a 0x-prefixed 20-byte hex address. It does not
// (and cannot, from inside Fabric) verify that the contract is actually
// deployed — that remains an operational precondition.
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
