// Package verifylib recomputes a transaction's commitment from its original
// data and checks it against the on-chain anchor (spec section 15). This is
// the point of the whole pipeline: anyone holding the original data can
// verify, with no trust in the relayer.
package verifylib

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/relay/evm"
)

// AnchorReader is the read-only chain access verification needs;
// *evm.Client satisfies it.
type AnchorReader interface {
	GetAnchor(ctx context.Context, fabricTxID [32]byte) (*evm.AnchorRecord, error)
}

// RootReader is the read-only access batch verification needs; *evm.Client
// satisfies it.
type RootReader interface {
	GetRoot(ctx context.Context, root [32]byte) (*evm.RootRecord, error)
}

// Input is the original transaction data being verified.
type Input struct {
	FabricTxID  string // 64 hex chars
	ChannelID   string
	ChaincodeID string
	BlockNumber uint64
	Timestamp   uint64
	// Payload is the declared payload. Exactly one of Payload or
	// PayloadCanonicalHex must be set.
	Payload *canonical.Payload
	// PayloadCanonicalHex is the exact canonical event bytes (hex), for
	// callers who kept the raw MSTProofRequest payload.
	PayloadCanonicalHex string
}

// Result is the verification outcome.
type Result struct {
	Commitment [32]byte
	OnChain    *evm.AnchorRecord // nil when nothing is anchored for the tx
	Match      bool
}

// Recompute derives the commitment from the original data using the shared
// canonical encoder — the same code path the capture service used.
func Recompute(in Input) ([32]byte, error) {
	var payloadHash [32]byte
	switch {
	case in.Payload != nil && in.PayloadCanonicalHex != "":
		return [32]byte{}, fmt.Errorf("verify: provide either the payload fields or the canonical hex, not both")
	case in.Payload != nil:
		h, err := canonical.PayloadHash(*in.Payload)
		if err != nil {
			return [32]byte{}, fmt.Errorf("verify: payload: %w", err)
		}
		payloadHash = h
	case in.PayloadCanonicalHex != "":
		raw, err := hex.DecodeString(strings.TrimPrefix(in.PayloadCanonicalHex, "0x"))
		if err != nil {
			return [32]byte{}, fmt.Errorf("verify: payload hex: %w", err)
		}
		if _, err := canonical.Decode(raw); err != nil {
			return [32]byte{}, fmt.Errorf("verify: payload bytes are not canonical: %w", err)
		}
		payloadHash = canonical.Keccak256(raw)
	default:
		return [32]byte{}, fmt.Errorf("verify: no payload given")
	}

	txID, err := canonical.ParseFabricTxID(in.FabricTxID)
	if err != nil {
		return [32]byte{}, err
	}
	c := canonical.NewCommitment(txID, in.ChannelID, in.ChaincodeID, in.BlockNumber, in.Timestamp, payloadHash)
	return c.Hash()
}

// Verify recomputes the commitment and compares it with the on-chain anchor.
func Verify(ctx context.Context, reader AnchorReader, in Input) (*Result, error) {
	commitment, err := Recompute(in)
	if err != nil {
		return nil, err
	}
	txID, err := canonical.ParseFabricTxID(in.FabricTxID)
	if err != nil {
		return nil, err
	}
	onChain, err := reader.GetAnchor(ctx, txID)
	if err != nil {
		return nil, fmt.Errorf("verify: read anchor: %w", err)
	}
	return &Result{
		Commitment: commitment,
		OnChain:    onChain,
		Match:      onChain != nil && onChain.Commitment == commitment,
	}, nil
}

// BatchResult is the outcome of verifying a transaction inside a Merkle
// batch (the proof-of-combination path).
type BatchResult struct {
	// Leaf is the recomputed per-transaction commitment.
	Leaf [32]byte
	// ProofValid reports whether the inclusion proof links Leaf to Root.
	ProofValid bool
	// OnChain is the root's record on MST (nil when the root is not anchored).
	OnChain *evm.RootRecord
	// Match is the overall verdict: proof valid AND root anchored.
	Match bool
}

// VerifyInBatch recomputes the transaction's commitment, checks its Merkle
// inclusion proof against root, and confirms the root is anchored on MST.
func VerifyInBatch(ctx context.Context, reader RootReader, in Input, root [32]byte, proof [][32]byte) (*BatchResult, error) {
	leaf, err := Recompute(in)
	if err != nil {
		return nil, err
	}
	result := &BatchResult{
		Leaf:       leaf,
		ProofValid: canonical.VerifyMerkleProof(root, leaf, proof),
	}
	onChain, err := reader.GetRoot(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("verify: read root: %w", err)
	}
	result.OnChain = onChain
	result.Match = result.ProofValid && onChain != nil
	return result, nil
}

// DeriveProof computes the inclusion proof for a leaf given ALL of the
// batch's leaf commitments (e.g. published by the relayer's operator or
// exported with mst-proof). It also returns the recomputed root so callers
// can cross-check the expected root.
func DeriveProof(leaves [][32]byte, leaf [32]byte) (root [32]byte, proof [][32]byte, err error) {
	root, err = canonical.MerkleRoot(leaves)
	if err != nil {
		return [32]byte{}, nil, err
	}
	proof, err = canonical.MerkleProof(leaves, leaf)
	if err != nil {
		return [32]byte{}, nil, err
	}
	return root, proof, nil
}

// fieldSpec is the JSON shape of one declared field — the same shape the
// cross-language test vectors use:
//
//	[{"name":"amount","type":"int","value":"100"},
//	 {"name":"active","type":"bool","value":true},
//	 {"name":"memo","type":"string","value":"hi"},
//	 {"name":"blob","type":"bytes","valueHex":"0xdeadbeef"}]
type fieldSpec struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Value    json.RawMessage `json:"value,omitempty"`
	ValueHex string          `json:"valueHex,omitempty"`
}

// ParsePayloadJSON builds a canonical payload from the JSON field list.
func ParsePayloadJSON(data []byte) (*canonical.Payload, error) {
	var specs []fieldSpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, fmt.Errorf("verify: parse payload JSON: %w", err)
	}
	p := &canonical.Payload{}
	for _, s := range specs {
		var (
			f   canonical.Field
			err error
		)
		switch s.Type {
		case "int":
			var dec string
			if uerr := json.Unmarshal(s.Value, &dec); uerr != nil {
				return nil, fmt.Errorf("verify: field %q: int value must be a decimal string", s.Name)
			}
			v, ok := new(big.Int).SetString(dec, 10)
			if !ok {
				return nil, fmt.Errorf("verify: field %q: %q is not a base-10 integer", s.Name, dec)
			}
			f, err = canonical.IntField(s.Name, v)
		case "bool":
			var b bool
			if uerr := json.Unmarshal(s.Value, &b); uerr != nil {
				return nil, fmt.Errorf("verify: field %q: bool value: %w", s.Name, uerr)
			}
			f, err = canonical.BoolField(s.Name, b)
		case "string":
			var v string
			if uerr := json.Unmarshal(s.Value, &v); uerr != nil {
				return nil, fmt.Errorf("verify: field %q: string value: %w", s.Name, uerr)
			}
			f, err = canonical.StringField(s.Name, v)
		case "bytes":
			raw, herr := hex.DecodeString(strings.TrimPrefix(s.ValueHex, "0x"))
			if herr != nil {
				return nil, fmt.Errorf("verify: field %q: bytes hex: %w", s.Name, herr)
			}
			f, err = canonical.BytesField(s.Name, raw)
		default:
			return nil, fmt.Errorf("verify: field %q: unsupported type %q", s.Name, s.Type)
		}
		if err != nil {
			return nil, err
		}
		p.Fields = append(p.Fields, f)
	}
	return p, nil
}
