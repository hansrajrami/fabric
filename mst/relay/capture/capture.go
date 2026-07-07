// Package capture turns committed, opted-in Fabric transactions into
// commitments in the durable outbox (spec section 8). It consumes blocks
// that are already committed, so it is structurally off the commit path:
// nothing here can slow, block, or endanger Fabric's commit.
//
// The package is deliberately proto-free (it consumes the parsed txmodel),
// so the same capture core runs in both deployments: the standalone relayer
// (blocks parsed from the Fabric Gateway by relay/gwsource) and embedded
// inside the peer binary (blocks parsed from the peer's own ledger).
package capture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/fabric-chaincode/proofhelper"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
	"github.com/hansrajrami/fabric/mst/relay/txmodel"
)

// Source delivers parsed committed blocks starting at a given block number.
// The returned channel closes when ctx ends or the underlying stream
// terminates.
type Source interface {
	Blocks(ctx context.Context, startBlock uint64) (<-chan *txmodel.Block, error)
}

// Mode selects which transactions are anchored.
type Mode string

const (
	// ModeOptIn (default) anchors only transactions that emitted the
	// MSTProofRequest event — the chaincode chooses, per transaction, and
	// declares which fields the proof covers.
	ModeOptIn Mode = "opt-in"
	// ModeAll anchors EVERY valid endorser transaction. Opted-in
	// transactions keep their declared payloads; all others are anchored
	// with the well-known empty payload hash — an existence-and-timing
	// proof over the transaction tuple, verifiable with
	// `mst-verify --payload-hex 0x00000000`. Gas cost scales with channel
	// traffic: batch cadence is strongly advisable.
	ModeAll Mode = "all"
)

// Validate normalizes and checks the mode.
func (m *Mode) Validate() error {
	switch *m {
	case "", ModeOptIn:
		*m = ModeOptIn
	case ModeAll:
	default:
		return fmt.Errorf("capture: unknown mode %q (want %q or %q)", *m, ModeOptIn, ModeAll)
	}
	return nil
}

// systemChaincodes are never anchored in ModeAll: lifecycle/system
// transactions are Fabric plumbing, not business facts, and anchoring them
// by surprise would waste gas (and, for the write-back path, loop).
var systemChaincodes = map[string]bool{
	"_lifecycle": true,
	"lscc":       true,
	"cscc":       true,
	"qscc":       true,
	"vscc":       true,
}

// Config tunes the capture service.
type Config struct {
	// Mode selects opt-in (default) or anchor-all capture.
	Mode Mode

	// DefaultStartBlock is used when the outbox holds no checkpoint yet
	// (i.e. the very first run). Zero replays the chain from genesis.
	DefaultStartBlock uint64

	// ExcludeChaincodes are never captured. The anchor-status chaincode MUST
	// be listed here (belt) in addition to never emitting MSTProofRequest
	// (braces), so write-backs can never echo-loop into new anchors — in
	// ModeAll this guard is load-bearing, since every write-back is itself
	// a valid transaction.
	ExcludeChaincodes []string

	// IncludeChaincodes, when non-empty, restricts ModeAll to these
	// chaincodes ("anchor every transaction of chaincode X"). Ignored in
	// ModeOptIn. Exclusions win over inclusions.
	IncludeChaincodes []string
}

// Service is the capture loop.
type Service struct {
	source  Source
	store   outbox.Store
	cfg     Config
	exclude map[string]bool
	include map[string]bool
	log     *slog.Logger

	// processed counts blocks handled in this process lifetime (metrics/tests).
	processed uint64
}

// New builds a capture service over the given block source and outbox.
// An invalid Mode falls back to opt-in with an error log rather than
// panicking (callers should Validate config beforehand).
func New(source Source, store outbox.Store, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Mode.Validate(); err != nil {
		log.Error("invalid capture mode; falling back to opt-in", "err", err)
		cfg.Mode = ModeOptIn
	}
	exclude := make(map[string]bool, len(cfg.ExcludeChaincodes))
	for _, cc := range cfg.ExcludeChaincodes {
		exclude[cc] = true
	}
	include := make(map[string]bool, len(cfg.IncludeChaincodes))
	for _, cc := range cfg.IncludeChaincodes {
		include[cc] = true
	}
	return &Service{source: source, store: store, cfg: cfg, exclude: exclude, include: include, log: log}
}

// Run consumes blocks from the last checkpoint until ctx is cancelled or the
// source stream ends. It returns nil on context cancellation and an error if
// the stream terminated for any other reason (callers reconnect with backoff).
func (s *Service) Run(ctx context.Context) error {
	start, ok, err := s.store.Checkpoint()
	if err != nil {
		return fmt.Errorf("capture: read checkpoint: %w", err)
	}
	if !ok {
		start = s.cfg.DefaultStartBlock
	}
	s.log.Info("capture starting", "startBlock", start)

	blocks, err := s.source.Blocks(ctx, start)
	if err != nil {
		return fmt.Errorf("capture: open block stream at %d: %w", start, err)
	}

	for block := range blocks {
		if err := s.ProcessBlock(block); err != nil {
			// Only storage failures land here; malformed payloads are
			// quarantined inside ProcessBlock. A storage failure is not
			// survivable — stop and let the operator/supervisor intervene.
			return fmt.Errorf("capture: block %d: %w", block.Number, err)
		}
	}
	if ctx.Err() != nil {
		return nil // clean shutdown
	}
	return errors.New("capture: block stream ended unexpectedly")
}

// ProcessBlock extracts opted-in transactions from one committed block and
// writes their commitments plus the advanced checkpoint atomically. It is
// idempotent: redelivered blocks insert nothing and never regress the
// checkpoint.
func (s *Service) ProcessBlock(parsed *txmodel.Block) error {
	var entries []*outbox.Entry
	for i := range parsed.Txs {
		tx := &parsed.Txs[i]
		if !tx.Valid {
			continue // endorsement-failed / MVCC-conflict txs are not real commits
		}
		entry, reason, err := s.buildEntry(tx, parsed.Number)
		if err != nil {
			// Poison pill: record and continue; never wedge the stream.
			s.log.Warn("quarantining malformed proof request",
				"block", parsed.Number, "txID", tx.TxID, "reason", reason, "err", err)
			s.quarantine(tx, parsed.Number, fmt.Sprintf("%s: %v", reason, err))
			continue
		}
		if entry != nil {
			entries = append(entries, entry)
		}
	}
	for _, bad := range parsed.Bad {
		s.log.Warn("unparseable block entry", "block", parsed.Number, "index", bad.Index, "err", bad.Err)
	}

	inserted, err := s.store.PutBlock(entries, parsed.Number+1)
	if err != nil {
		return err
	}
	s.processed++
	if inserted > 0 || len(entries) > 0 {
		s.log.Info("captured block", "block", parsed.Number, "optedIn", len(entries), "inserted", inserted)
	}
	return nil
}

// emptyPayloadHash is keccak256 of the canonical empty payload — the
// payload_hash used in ModeAll for transactions that declared nothing.
var emptyPayloadHash = canonical.Keccak256([]byte{0, 0, 0, 0})

// buildEntry returns the outbox entry for a transaction, (nil, "", nil) when
// the tx is not captured under the current mode, or an error with a
// quarantine reason.
func (s *Service) buildEntry(tx *txmodel.Tx, blockNumber uint64) (*outbox.Entry, string, error) {
	var event *txmodel.Event
	for i := range tx.Events {
		ev := &tx.Events[i]
		if ev.EventName != proofhelper.EventName {
			continue
		}
		if s.exclude[ev.ChaincodeID] {
			s.log.Warn("ignoring proof request from excluded chaincode (echo-loop guard)",
				"chaincode", ev.ChaincodeID, "txID", tx.TxID)
			continue
		}
		event = ev
		break // Fabric records at most one event per tx
	}

	var chaincodeID string
	var payloadHash [32]byte
	switch {
	case event != nil:
		// Opted-in (either mode): the event payload must BE the canonical
		// encoding; strict Decode guarantees re-encode == input, so hashing
		// the received bytes hashes exactly what the chaincode declared,
		// with no alternative serialization accepted.
		if _, err := canonical.Decode(event.Payload); err != nil {
			return nil, "non-canonical payload", err
		}
		chaincodeID = event.ChaincodeID
		payloadHash = canonical.Keccak256(event.Payload)

	case s.cfg.Mode == ModeAll:
		// Anchor-all: no declared payload — commit to the transaction tuple
		// with the empty payload hash (existence-and-timing proof).
		chaincodeID = tx.ChaincodeID
		if chaincodeID == "" {
			// Endorser tx without a decodable chaincode action; nothing
			// meaningful to attribute the anchor to.
			s.log.Warn("skipping tx without chaincode id in anchor-all mode", "txID", tx.TxID)
			return nil, "", nil
		}
		if s.exclude[chaincodeID] || systemChaincodes[chaincodeID] {
			return nil, "", nil
		}
		if len(s.include) > 0 && !s.include[chaincodeID] {
			return nil, "", nil
		}
		payloadHash = emptyPayloadHash

	default:
		return nil, "", nil // opt-in mode, no event: not captured
	}

	txID, err := canonical.ParseFabricTxID(tx.TxID)
	if err != nil {
		return nil, "bad tx id", err
	}
	if tx.TimestampUnix == 0 {
		return nil, "missing timestamp", errors.New("channel header has no timestamp")
	}

	commitment := canonical.NewCommitment(
		txID, tx.ChannelID, chaincodeID, blockNumber, tx.TimestampUnix, payloadHash)
	hash, err := commitment.Hash()
	if err != nil {
		return nil, "commitment", err
	}

	return &outbox.Entry{
		FabricTxID:  txID,
		EntryType:   outbox.EntryTypeCommitmentV1,
		Commitment:  hash,
		ChannelID:   tx.ChannelID,
		ChaincodeID: chaincodeID,
		BlockNumber: blockNumber,
		Timestamp:   tx.TimestampUnix,
	}, "", nil
}

func (s *Service) quarantine(tx *txmodel.Tx, blockNumber uint64, reason string) {
	txID, err := canonical.ParseFabricTxID(tx.TxID)
	if err != nil {
		// No usable key; synthesize one from the raw id string so the record
		// is still visible in the quarantine.
		txID = canonical.Keccak256([]byte(tx.TxID))
	}
	var raw []byte
	for i := range tx.Events {
		if tx.Events[i].EventName == proofhelper.EventName {
			raw = tx.Events[i].Payload
			break
		}
	}
	if qErr := s.store.Quarantine(txID, blockNumber, reason, raw); qErr != nil {
		s.log.Error("quarantine write failed", "txID", tx.TxID, "err", qErr)
	}
}

// Processed reports how many blocks this instance has handled (for tests and
// metrics).
func (s *Service) Processed() uint64 { return s.processed }

// StaticSource replays a fixed slice of parsed blocks; used in tests and for
// offline reprocessing.
type StaticSource struct {
	BlocksList []*txmodel.Block
}

var _ Source = (*StaticSource)(nil)

// Blocks emits every stored block with number >= startBlock, then closes.
func (s *StaticSource) Blocks(ctx context.Context, startBlock uint64) (<-chan *txmodel.Block, error) {
	ch := make(chan *txmodel.Block)
	go func() {
		defer close(ch)
		for _, b := range s.BlocksList {
			if b.Number < startBlock {
				continue
			}
			select {
			case ch <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
