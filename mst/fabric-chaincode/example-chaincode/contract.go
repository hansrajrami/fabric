// Package main is the demo business chaincode for MST proof anchoring: a
// minimal asset registry whose state-changing transactions opt in to proof
// anchoring by emitting MSTProofRequest via the proofhelper (spec section 7).
package main

import (
	"encoding/json"
	"fmt"

	"github.com/hansrajrami/fabric/mst/fabric-chaincode/proofhelper"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// Asset is the demo business object.
type Asset struct {
	ID     string `json:"id"`
	Owner  string `json:"owner"`
	Value  int64  `json:"value"`
	Active bool   `json:"active"`
}

// SmartContract implements the demo asset transactions.
type SmartContract struct {
	contractapi.Contract
}

// CreateAsset stores a new asset and declares a proof over its business
// fields. The declared payload commits to what happened (asset created with
// these values) without those values ever leaving Fabric — only their
// canonical hash reaches the MST chain.
func (s *SmartContract) CreateAsset(ctx contractapi.TransactionContextInterface, id, owner string, value int64) error {
	if value < 0 {
		return fmt.Errorf("value must be non-negative")
	}
	stub := ctx.GetStub()
	existing, err := stub.GetState(id)
	if err != nil {
		return fmt.Errorf("read asset %s: %w", id, err)
	}
	if existing != nil {
		return fmt.Errorf("asset %s already exists", id)
	}

	asset := Asset{ID: id, Owner: owner, Value: value, Active: true}
	raw, err := json.Marshal(asset)
	if err != nil {
		return err
	}
	if err := stub.PutState(id, raw); err != nil {
		return fmt.Errorf("write asset %s: %w", id, err)
	}

	// Opt in: declare the fields this proof covers. Validation happens here,
	// at endorsement time — a malformed declaration fails the transaction.
	return proofhelper.New().
		AddString("action", "create").
		AddString("asset_id", id).
		AddString("owner", owner).
		AddInt64("value", value).
		AddBool("active", true).
		Emit(stub)
}

// TransferAsset moves ownership and declares a proof over the transfer.
func (s *SmartContract) TransferAsset(ctx contractapi.TransactionContextInterface, id, newOwner string) error {
	stub := ctx.GetStub()
	raw, err := stub.GetState(id)
	if err != nil {
		return fmt.Errorf("read asset %s: %w", id, err)
	}
	if raw == nil {
		return fmt.Errorf("asset %s does not exist", id)
	}
	var asset Asset
	if err := json.Unmarshal(raw, &asset); err != nil {
		return fmt.Errorf("decode asset %s: %w", id, err)
	}

	previousOwner := asset.Owner
	asset.Owner = newOwner
	updated, err := json.Marshal(asset)
	if err != nil {
		return err
	}
	if err := stub.PutState(id, updated); err != nil {
		return fmt.Errorf("write asset %s: %w", id, err)
	}

	return proofhelper.New().
		AddString("action", "transfer").
		AddString("asset_id", id).
		AddString("previous_owner", previousOwner).
		AddString("new_owner", newOwner).
		Emit(stub)
}

// ReadAsset returns an asset by id. Queries do not opt in to proofs.
func (s *SmartContract) ReadAsset(ctx contractapi.TransactionContextInterface, id string) (*Asset, error) {
	raw, err := ctx.GetStub().GetState(id)
	if err != nil {
		return nil, fmt.Errorf("read asset %s: %w", id, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("asset %s does not exist", id)
	}
	var asset Asset
	if err := json.Unmarshal(raw, &asset); err != nil {
		return nil, fmt.Errorf("decode asset %s: %w", id, err)
	}
	return &asset, nil
}
