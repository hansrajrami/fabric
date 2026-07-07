// Package main is the anchor-status chaincode (spec section 14): the
// dedicated write-back target where the relayer records that a transaction
// was anchored on MST, so Fabric can answer "is this tx anchored?" from its
// own ledger.
//
// Non-anchorable by construction: this chaincode NEVER calls SetEvent, so a
// write-back transaction can never carry an MSTProofRequest and re-enter the
// capture pipeline (echo loop). The capture service additionally excludes
// this chaincode by name — belt and braces.
//
// It stores a thin status pointer only (where the proof lives publicly);
// it never re-stores the commitment or the declared payload, and it can
// never touch business state.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// EnvRelayerMSPID restricts writes to a single MSP when set on the chaincode
// process (e.g. MST_RELAYER_MSPID=Org1MSP). Defense in depth: the primary
// production write guard is the chaincode's endorsement policy; this check
// stops misdirected writes even from inside an endorsing org.
const EnvRelayerMSPID = "MST_RELAYER_MSPID"

const keyPrefix = "anchor:"

// StatusConfirmed is the only status Part 1 records.
const StatusConfirmed = "CONFIRMED"

// AnchorStatus is the thin pointer stored per anchored transaction.
type AnchorStatus struct {
	FabricTxID string `json:"fabric_tx_id"`
	AnchorRef  string `json:"anchor_ref"` // MST tx hash (0x + 64 hex)
	Status     string `json:"status"`
	RecordedAt int64  `json:"recorded_at"` // recording tx's timestamp (deterministic)
}

// SmartContract implements RecordAnchor / QueryAnchorStatus.
type SmartContract struct {
	contractapi.Contract

	// relayerMSPID caches the env lookup; empty means unrestricted (rely on
	// endorsement policy).
	relayerMSPID string
	mspLoaded    bool
}

func (s *SmartContract) allowedMSP() string {
	if !s.mspLoaded {
		s.relayerMSPID = os.Getenv(EnvRelayerMSPID)
		s.mspLoaded = true
	}
	return s.relayerMSPID
}

// RecordAnchor records that fabricTxID was anchored on MST. Idempotent by
// fabricTxID: re-recording an existing id is a quiet success that changes
// nothing, so relayer retries can never duplicate or overwrite.
func (s *SmartContract) RecordAnchor(ctx contractapi.TransactionContextInterface, fabricTxID, anchorRef, status string) error {
	if msp := s.allowedMSP(); msp != "" {
		caller, err := ctx.GetClientIdentity().GetMSPID()
		if err != nil {
			return fmt.Errorf("identity: %w", err)
		}
		if caller != msp {
			return fmt.Errorf("MSP %q is not authorized to record anchors", caller)
		}
	}

	fabricTxID = strings.ToLower(fabricTxID)
	if err := validateTxID(fabricTxID); err != nil {
		return err
	}
	if err := validateAnchorRef(anchorRef); err != nil {
		return err
	}
	if status != StatusConfirmed {
		return fmt.Errorf("unsupported status %q (only %q is recorded in Part 1)", status, StatusConfirmed)
	}

	stub := ctx.GetStub()
	key := keyPrefix + fabricTxID
	existing, err := stub.GetState(key)
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	if existing != nil {
		return nil // idempotent: already recorded, quiet no-op
	}

	ts, err := stub.GetTxTimestamp()
	if err != nil {
		return fmt.Errorf("tx timestamp: %w", err)
	}
	record := AnchorStatus{
		FabricTxID: fabricTxID,
		AnchorRef:  strings.ToLower(anchorRef),
		Status:     status,
		RecordedAt: ts.GetSeconds(),
	}
	raw, err := json.Marshal(&record)
	if err != nil {
		return err
	}
	return stub.PutState(key, raw)
}

// QueryAnchorStatus returns the status pointer for fabricTxID.
func (s *SmartContract) QueryAnchorStatus(ctx contractapi.TransactionContextInterface, fabricTxID string) (*AnchorStatus, error) {
	fabricTxID = strings.ToLower(fabricTxID)
	if err := validateTxID(fabricTxID); err != nil {
		return nil, err
	}
	raw, err := ctx.GetStub().GetState(keyPrefix + fabricTxID)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("transaction %s is not anchored", fabricTxID)
	}
	var record AnchorStatus
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("corrupt record: %w", err)
	}
	return &record, nil
}

// IsAnchored is a convenience boolean query.
func (s *SmartContract) IsAnchored(ctx contractapi.TransactionContextInterface, fabricTxID string) (bool, error) {
	fabricTxID = strings.ToLower(fabricTxID)
	if err := validateTxID(fabricTxID); err != nil {
		return false, err
	}
	raw, err := ctx.GetStub().GetState(keyPrefix + fabricTxID)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	return raw != nil, nil
}

func validateTxID(txID string) error {
	if len(txID) != 64 {
		return fmt.Errorf("fabric tx id must be 64 hex chars, got %d", len(txID))
	}
	if _, err := hex.DecodeString(txID); err != nil {
		return fmt.Errorf("fabric tx id is not hex: %w", err)
	}
	return nil
}

func validateAnchorRef(ref string) error {
	if !strings.HasPrefix(ref, "0x") || len(ref) != 66 {
		return fmt.Errorf("anchor ref must be 0x + 64 hex chars")
	}
	if _, err := hex.DecodeString(ref[2:]); err != nil {
		return fmt.Errorf("anchor ref is not hex: %w", err)
	}
	return nil
}
