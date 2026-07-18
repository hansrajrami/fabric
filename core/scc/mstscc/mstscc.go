/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

// Package mstscc is the MST anchor-status system chaincode: the write-back
// target where the relayer records, as a consensus-backed ledger fact, that a
// Fabric transaction was anchored on the MST public blockchain. It is the
// Phase 1.5 successor to the mst-anchor-status USER chaincode — the same thin
// status pointer, but built into the peer and governed by channel config
// rather than deployed per network.
//
// Two-level gating:
//
//   - Peer level: the SCC is deployed only on peers whose core.yaml
//     chaincode.system allowlist enables "mstscc" (the peer opts in / runs the
//     MST-enabled binary).
//   - Channel level: every invoke is rejected unless the channel's own
//     configuration (channelconfig.MSTAnchorConfig) has MST anchoring enabled.
//     The SCC is therefore inert on channels that did not turn anchoring on —
//     which is what "the system chaincode is active only when the channel
//     enables MST anchoring" means in practice for a built-in SCC.
//
// Echo-loop safe by construction: this chaincode NEVER calls SetEvent, so a
// write-back transaction can never carry an MSTProofRequest and re-enter the
// capture pipeline. The capture service additionally excludes this chaincode
// by name (belt and braces).
package mstscc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hyperledger/fabric-chaincode-go/shim"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/core/aclmgmt"
)

// Name is the built-in chaincode name. It must be listed in the peer's
// chaincode.system allowlist to be deployed, and is added to the capture
// service's excluded-chaincodes set so write-backs are never re-anchored.
const Name = "mstscc"

// Invoke function names.
const (
	RecordAnchor      = "RecordAnchor"
	QueryAnchorStatus = "QueryAnchorStatus"
	IsAnchored        = "IsAnchored"
	ListAnchors       = "ListAnchors"
	CountAnchors      = "CountAnchors"
)

// StatusConfirmed is the only status Phase 1.5 records.
const StatusConfirmed = "CONFIRMED"

const (
	keyPrefix = "anchor:"
	// keyPrefixEnd is the exclusive upper bound for a range scan over every
	// "anchor:" key (';' is ':'+1), so GetStateByRange covers exactly the
	// anchor records.
	keyPrefixEnd = "anchor;"
	// defaultListLimit bounds ListAnchors when the caller does not supply one.
	defaultListLimit = 100
)

var logger = flogging.MustGetLogger("mstscc")

// ChannelConfigGetter yields a channel's parsed application config so the SCC
// can read the channel-level MST anchoring configuration. *peer.Peer
// satisfies it (GetApplicationConfig).
type ChannelConfigGetter interface {
	GetApplicationConfig(cid string) (channelconfig.Application, bool)
}

// AnchorStatus is the thin pointer stored per anchored transaction. It records
// only where the proof lives publicly; it never re-stores the commitment or
// any business data, and can never touch business state.
type AnchorStatus struct {
	FabricTxID string `json:"fabric_tx_id"`
	AnchorRef  string `json:"anchor_ref"` // MST tx hash (0x + 64 hex)
	Status     string `json:"status"`
	RecordedAt int64  `json:"recorded_at"` // recording tx's timestamp (deterministic)
}

// MSTAnchorSCC is the system-chaincode implementation.
type MSTAnchorSCC struct {
	// aclProvider is retained as the single hook for tightening who may submit
	// anchor-status writes. Phase 1.5 authorizes any valid MSP identity on the
	// channel (a write only reaches Invoke after the peer has authenticated the
	// proposer as a channel member), so no additional ACL check is performed;
	// a designated-relayer or M-of-N committee policy plugs in here later
	// without reshaping the chaincode.
	aclProvider  aclmgmt.ACLProvider
	configGetter ChannelConfigGetter
}

// New returns an MST anchor-status SCC. Typically called once per peer.
func New(aclProvider aclmgmt.ACLProvider, configGetter ChannelConfigGetter) *MSTAnchorSCC {
	return &MSTAnchorSCC{aclProvider: aclProvider, configGetter: configGetter}
}

func (s *MSTAnchorSCC) Name() string              { return Name }
func (s *MSTAnchorSCC) Chaincode() shim.Chaincode { return s }

// Init is a no-op: there is no per-channel state to seed.
func (s *MSTAnchorSCC) Init(stub shim.ChaincodeStubInterface) pb.Response {
	return shim.Success(nil)
}

// Invoke dispatches RecordAnchor / QueryAnchorStatus / IsAnchored after
// enforcing the channel-level MST enablement gate.
func (s *MSTAnchorSCC) Invoke(stub shim.ChaincodeStubInterface) pb.Response {
	channelID := stub.GetChannelID()
	if err := s.requireEnabled(channelID); err != nil {
		return shim.Error(err.Error())
	}

	args := stub.GetArgs()
	if len(args) == 0 {
		return shim.Error("mstscc: missing function name")
	}
	fname := string(args[0])
	switch fname {
	case RecordAnchor:
		return s.recordAnchor(stub, args)
	case QueryAnchorStatus:
		return s.queryAnchorStatus(stub, args)
	case IsAnchored:
		return s.isAnchored(stub, args)
	case ListAnchors:
		return s.listAnchors(stub, args)
	case CountAnchors:
		return s.countAnchors(stub)
	default:
		return shim.Error(fmt.Sprintf("mstscc: unknown function %q", fname))
	}
}

// requireEnabled rejects the invoke unless the channel's configuration has MST
// anchoring enabled. This is the channel-config governance gate: a channel
// turns MST anchoring on (all orgs agreeing through the Application group's
// modification policy) before the SCC will record anything on it.
func (s *MSTAnchorSCC) requireEnabled(channelID string) error {
	app, ok := s.configGetter.GetApplicationConfig(channelID)
	if !ok {
		return fmt.Errorf("mstscc: no application config for channel %s", channelID)
	}
	cfg, ok := app.MSTAnchorConfig()
	if !ok || !cfg.Enabled {
		return fmt.Errorf("mstscc: MST anchoring is not enabled on channel %s", channelID)
	}
	return nil
}

// recordAnchor records that fabricTxID was anchored on MST. Idempotent by
// fabricTxID: re-recording an existing id is a quiet success that changes
// nothing, so relayer retries can never duplicate or overwrite.
func (s *MSTAnchorSCC) recordAnchor(stub shim.ChaincodeStubInterface, args [][]byte) pb.Response {
	if len(args) != 4 {
		return shim.Error("mstscc: RecordAnchor(fabricTxID, anchorRef, status) requires 3 arguments")
	}
	fabricTxID := strings.ToLower(string(args[1]))
	anchorRef := strings.ToLower(string(args[2]))
	status := string(args[3])

	if err := validateTxID(fabricTxID); err != nil {
		return shim.Error("mstscc: " + err.Error())
	}
	if err := validateAnchorRef(anchorRef); err != nil {
		return shim.Error("mstscc: " + err.Error())
	}
	if status != StatusConfirmed {
		return shim.Error(fmt.Sprintf("mstscc: unsupported status %q (only %q is recorded in Phase 1.5)", status, StatusConfirmed))
	}

	key := keyPrefix + fabricTxID
	existing, err := stub.GetState(key)
	if err != nil {
		return shim.Error(fmt.Sprintf("mstscc: read %s: %s", key, err))
	}
	if existing != nil {
		return shim.Success(nil) // idempotent: already recorded, quiet no-op
	}

	ts, err := stub.GetTxTimestamp()
	if err != nil {
		return shim.Error(fmt.Sprintf("mstscc: tx timestamp: %s", err))
	}
	raw, err := json.Marshal(&AnchorStatus{
		FabricTxID: fabricTxID,
		AnchorRef:  anchorRef,
		Status:     status,
		RecordedAt: ts.GetSeconds(),
	})
	if err != nil {
		return shim.Error(fmt.Sprintf("mstscc: marshal record: %s", err))
	}
	if err := stub.PutState(key, raw); err != nil {
		return shim.Error(fmt.Sprintf("mstscc: write %s: %s", key, err))
	}
	logger.Debugw("recorded anchor", "channel", stub.GetChannelID(), "fabricTxID", fabricTxID)
	return shim.Success(nil)
}

// queryAnchorStatus returns the status pointer for fabricTxID, as marshalled
// JSON, or an error if the transaction is not anchored.
func (s *MSTAnchorSCC) queryAnchorStatus(stub shim.ChaincodeStubInterface, args [][]byte) pb.Response {
	if len(args) != 2 {
		return shim.Error("mstscc: QueryAnchorStatus(fabricTxID) requires 1 argument")
	}
	fabricTxID := strings.ToLower(string(args[1]))
	if err := validateTxID(fabricTxID); err != nil {
		return shim.Error("mstscc: " + err.Error())
	}
	raw, err := stub.GetState(keyPrefix + fabricTxID)
	if err != nil {
		return shim.Error(fmt.Sprintf("mstscc: read: %s", err))
	}
	if raw == nil {
		return shim.Error(fmt.Sprintf("mstscc: transaction %s is not anchored", fabricTxID))
	}
	return shim.Success(raw)
}

// isAnchored returns "true"/"false" for whether fabricTxID has been recorded.
func (s *MSTAnchorSCC) isAnchored(stub shim.ChaincodeStubInterface, args [][]byte) pb.Response {
	if len(args) != 2 {
		return shim.Error("mstscc: IsAnchored(fabricTxID) requires 1 argument")
	}
	fabricTxID := strings.ToLower(string(args[1]))
	if err := validateTxID(fabricTxID); err != nil {
		return shim.Error("mstscc: " + err.Error())
	}
	raw, err := stub.GetState(keyPrefix + fabricTxID)
	if err != nil {
		return shim.Error(fmt.Sprintf("mstscc: read: %s", err))
	}
	if raw != nil {
		return shim.Success([]byte("true"))
	}
	return shim.Success([]byte("false"))
}

// listAnchors returns up to `limit` anchor-status records on the channel as a
// JSON array, ordered by key. An optional args[1] overrides the limit
// (defaultListLimit otherwise); 0 or negative means defaultListLimit. Keys are
// plain (keyPrefix + txID), not composite, so a bounded range scan over the
// "anchor:" prefix is the correct iteration.
func (s *MSTAnchorSCC) listAnchors(stub shim.ChaincodeStubInterface, args [][]byte) pb.Response {
	limit := defaultListLimit
	if len(args) >= 2 {
		n, err := strconv.Atoi(string(args[1]))
		if err != nil {
			return shim.Error("mstscc: ListAnchors limit must be an integer")
		}
		if n > 0 {
			limit = n
		}
	}

	iter, err := stub.GetStateByRange(keyPrefix, keyPrefixEnd)
	if err != nil {
		return shim.Error(fmt.Sprintf("mstscc: range query: %s", err))
	}
	defer iter.Close()

	records := make([]AnchorStatus, 0, limit)
	for iter.HasNext() && len(records) < limit {
		kv, err := iter.Next()
		if err != nil {
			return shim.Error(fmt.Sprintf("mstscc: iterate: %s", err))
		}
		var rec AnchorStatus
		if err := json.Unmarshal(kv.GetValue(), &rec); err != nil {
			return shim.Error(fmt.Sprintf("mstscc: corrupt record for %s: %s", kv.GetKey(), err))
		}
		records = append(records, rec)
	}
	out, err := json.Marshal(records)
	if err != nil {
		return shim.Error(fmt.Sprintf("mstscc: marshal list: %s", err))
	}
	return shim.Success(out)
}

// countAnchors returns the number of anchor-status records on the channel as a
// decimal string.
func (s *MSTAnchorSCC) countAnchors(stub shim.ChaincodeStubInterface) pb.Response {
	iter, err := stub.GetStateByRange(keyPrefix, keyPrefixEnd)
	if err != nil {
		return shim.Error(fmt.Sprintf("mstscc: range query: %s", err))
	}
	defer iter.Close()

	count := 0
	for iter.HasNext() {
		if _, err := iter.Next(); err != nil {
			return shim.Error(fmt.Sprintf("mstscc: iterate: %s", err))
		}
		count++
	}
	return shim.Success([]byte(strconv.Itoa(count)))
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
