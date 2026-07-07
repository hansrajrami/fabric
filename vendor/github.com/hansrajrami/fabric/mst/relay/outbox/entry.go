// Package outbox is the crash-safe memory of the relay (spec section 9): a
// durable, embedded LevelDB store holding one entry per captured commitment,
// the capture checkpoint, and a quarantine area for malformed events.
//
// Durability model: every write is a single LevelDB WriteBatch with
// Sync=true, so an entry insert and the checkpoint advance land atomically
// or not at all — a crash at any point leaves a state the capture service
// and sender can resume from without loss or duplication. LevelDB is the
// same storage library Fabric's own peer uses for its state database; here
// the relayer owns its private instance (the peer's cannot be shared — it is
// locked by the peer process).
package outbox

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Status is an outbox entry's position in the delivery state machine.
type Status uint8

const (
	// StatusPending: captured, waiting to be submitted to MST.
	StatusPending Status = 1
	// StatusSubmitted: anchor transaction sent; waiting for confirmations.
	StatusSubmitted Status = 2
	// StatusConfirmed: anchored with the required confirmations on MST.
	StatusConfirmed Status = 3
	// StatusWrittenBack: anchor status recorded on Fabric.
	StatusWrittenBack Status = 4
	// StatusDone: terminal.
	StatusDone Status = 5
)

var statusNames = map[Status]string{
	StatusPending:     "PENDING",
	StatusSubmitted:   "SUBMITTED",
	StatusConfirmed:   "CONFIRMED",
	StatusWrittenBack: "WRITTEN_BACK",
	StatusDone:        "DONE",
}

func (s Status) String() string {
	if n, ok := statusNames[s]; ok {
		return n
	}
	return fmt.Sprintf("Status(%d)", uint8(s))
}

// AllStatuses lists every valid status in pipeline order.
func AllStatuses() []Status {
	return []Status{StatusPending, StatusSubmitted, StatusConfirmed, StatusWrittenBack, StatusDone}
}

// allowedTransitions is the delivery state machine. Beyond the forward path,
// SUBMITTED→PENDING re-queues an entry whose submission failed, and
// PENDING→CONFIRMED short-circuits when a pre-submit getAnchor discovers the
// tx is already anchored (crash recovery, or another relayer got there).
var allowedTransitions = map[Status]map[Status]bool{
	StatusPending:     {StatusSubmitted: true, StatusConfirmed: true},
	StatusSubmitted:   {StatusConfirmed: true, StatusPending: true},
	StatusConfirmed:   {StatusWrittenBack: true},
	StatusWrittenBack: {StatusDone: true},
}

// EntryTypeCommitmentV1 is the only entry type in Part 1. Part 2 adds proof
// payload types without touching the outbox schema.
const EntryTypeCommitmentV1 = "commitment_v1"

// Entry is one captured commitment on its way to the MST chain.
type Entry struct {
	FabricTxID  [32]byte
	EntryType   string
	Commitment  [32]byte
	ChannelID   string
	ChaincodeID string
	BlockNumber uint64
	Timestamp   uint64 // Fabric tx ChannelHeader timestamp (unix seconds)

	Status Status
	// EVMTxHash is the submission transaction (zero until submitted). For
	// merkle batches it is the root-anchoring transaction, shared by every
	// entry of the batch.
	EVMTxHash [32]byte
	// BatchRoot is the Merkle batch root this entry was aggregated under
	// (zero for individually anchored entries). Recovery checks getRoot
	// instead of getAnchor when set.
	BatchRoot   [32]byte
	Attempts    uint32
	NextRetryAt int64 // unix seconds; 0 = immediately eligible
	CreatedAt   int64
	UpdatedAt   int64
}

// entryDTO is the stored JSON shape: hex for byte arrays, symbolic status.
// JSON keeps entries debuggable (leveldb tools, support dumps) and
// forward-compatible; the encode cost is irrelevant next to the fsync.
type entryDTO struct {
	FabricTxID  string `json:"fabric_tx_id"`
	EntryType   string `json:"entry_type"`
	Commitment  string `json:"commitment"`
	ChannelID   string `json:"channel_id"`
	ChaincodeID string `json:"chaincode_id"`
	BlockNumber uint64 `json:"block_number"`
	Timestamp   uint64 `json:"timestamp"`
	Status      uint8  `json:"status"`
	EVMTxHash   string `json:"evm_tx_hash,omitempty"`
	BatchRoot   string `json:"batch_root,omitempty"`
	Attempts    uint32 `json:"attempts,omitempty"`
	NextRetryAt int64  `json:"next_retry_at,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

var zero32 [32]byte

func (e *Entry) marshal() ([]byte, error) {
	dto := entryDTO{
		FabricTxID:  hex.EncodeToString(e.FabricTxID[:]),
		EntryType:   e.EntryType,
		Commitment:  hex.EncodeToString(e.Commitment[:]),
		ChannelID:   e.ChannelID,
		ChaincodeID: e.ChaincodeID,
		BlockNumber: e.BlockNumber,
		Timestamp:   e.Timestamp,
		Status:      uint8(e.Status),
		Attempts:    e.Attempts,
		NextRetryAt: e.NextRetryAt,
		CreatedAt:   e.CreatedAt,
		UpdatedAt:   e.UpdatedAt,
	}
	if e.EVMTxHash != zero32 {
		dto.EVMTxHash = hex.EncodeToString(e.EVMTxHash[:])
	}
	if e.BatchRoot != zero32 {
		dto.BatchRoot = hex.EncodeToString(e.BatchRoot[:])
	}
	return json.Marshal(&dto)
}

func unmarshalEntry(data []byte) (*Entry, error) {
	var dto entryDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, fmt.Errorf("outbox: corrupt entry: %w", err)
	}
	e := &Entry{
		EntryType:   dto.EntryType,
		ChannelID:   dto.ChannelID,
		ChaincodeID: dto.ChaincodeID,
		BlockNumber: dto.BlockNumber,
		Timestamp:   dto.Timestamp,
		Status:      Status(dto.Status),
		Attempts:    dto.Attempts,
		NextRetryAt: dto.NextRetryAt,
		CreatedAt:   dto.CreatedAt,
		UpdatedAt:   dto.UpdatedAt,
	}
	if err := decode32(dto.FabricTxID, &e.FabricTxID); err != nil {
		return nil, fmt.Errorf("outbox: corrupt fabric_tx_id: %w", err)
	}
	if err := decode32(dto.Commitment, &e.Commitment); err != nil {
		return nil, fmt.Errorf("outbox: corrupt commitment: %w", err)
	}
	if dto.EVMTxHash != "" {
		if err := decode32(dto.EVMTxHash, &e.EVMTxHash); err != nil {
			return nil, fmt.Errorf("outbox: corrupt evm_tx_hash: %w", err)
		}
	}
	if dto.BatchRoot != "" {
		if err := decode32(dto.BatchRoot, &e.BatchRoot); err != nil {
			return nil, fmt.Errorf("outbox: corrupt batch_root: %w", err)
		}
	}
	return e, nil
}

func decode32(s string, out *[32]byte) error {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return err
	}
	if len(raw) != 32 {
		return fmt.Errorf("want 32 bytes, got %d", len(raw))
	}
	copy(out[:], raw)
	return nil
}

// clone returns a private copy so callers can never mutate stored state.
func (e *Entry) clone() *Entry {
	cp := *e
	return &cp
}

func nowUnix() int64 { return time.Now().Unix() }
