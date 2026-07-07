// Package capture turns committed, opted-in Fabric transactions into
// commitments in the durable outbox (spec section 8). It consumes blocks that
// are already committed — delivered by the Fabric Gateway block-event API —
// so it is structurally off the commit path: nothing here can slow, block,
// or endanger Fabric's commit.
package capture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/fabric-chaincode/proofhelper"
	"github.com/hansrajrami/fabric/mst/relay/blockparse"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

// Source delivers committed blocks starting at a given block number. The
// returned channel closes when ctx ends or the underlying stream terminates.
type Source interface {
	Blocks(ctx context.Context, startBlock uint64) (<-chan *common.Block, error)
}

// Config tunes the capture service.
type Config struct {
	// DefaultStartBlock is used when the outbox holds no checkpoint yet
	// (i.e. the very first run). Zero replays the chain from genesis.
	DefaultStartBlock uint64

	// ExcludeChaincodes are never captured. The anchor-status chaincode MUST
	// be listed here (belt) in addition to never emitting MSTProofRequest
	// (braces), so write-backs can never echo-loop into new anchors.
	ExcludeChaincodes []string
}

// Service is the capture loop.
type Service struct {
	source  Source
	store   outbox.Store
	cfg     Config
	exclude map[string]bool
	log     *slog.Logger

	// processed counts blocks handled in this process lifetime (metrics/tests).
	processed uint64
}

// New builds a capture service over the given block source and outbox.
func New(source Source, store outbox.Store, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	exclude := make(map[string]bool, len(cfg.ExcludeChaincodes))
	for _, cc := range cfg.ExcludeChaincodes {
		exclude[cc] = true
	}
	return &Service{source: source, store: store, cfg: cfg, exclude: exclude, log: log}
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
			// Only storage failures land here; parsing problems are
			// quarantined inside ProcessBlock. A storage failure is not
			// survivable — stop and let the operator/supervisor intervene.
			return fmt.Errorf("capture: block %d: %w", block.GetHeader().GetNumber(), err)
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
func (s *Service) ProcessBlock(block *common.Block) error {
	parsed, err := blockparse.Parse(block)
	if err != nil {
		return err
	}

	var entries []*outbox.Entry
	for i := range parsed.Txs {
		tx := &parsed.Txs[i]
		if !tx.Valid() {
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

// buildEntry returns the outbox entry for an opted-in transaction, (nil, "",
// nil) when the tx did not opt in, or an error with a quarantine reason.
func (s *Service) buildEntry(tx *blockparse.Tx, blockNumber uint64) (*outbox.Entry, string, error) {
	var event *blockparse.Event
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
	if event == nil {
		return nil, "", nil
	}

	// The event payload must BE the canonical encoding; strict Decode
	// guarantees re-encode == input, so hashing the received bytes hashes
	// exactly what the chaincode declared, with no alternative serialization
	// accepted.
	if _, err := canonical.Decode(event.Payload); err != nil {
		return nil, "non-canonical payload", err
	}
	payloadHash := canonical.Keccak256(event.Payload)

	txID, err := canonical.ParseFabricTxID(tx.TxID)
	if err != nil {
		return nil, "bad tx id", err
	}
	if tx.TimestampUnix == 0 {
		return nil, "missing timestamp", errors.New("channel header has no timestamp")
	}

	commitment := canonical.NewCommitment(
		txID, tx.ChannelID, event.ChaincodeID, blockNumber, tx.TimestampUnix, payloadHash)
	hash, err := commitment.Hash()
	if err != nil {
		return nil, "commitment", err
	}

	return &outbox.Entry{
		FabricTxID:  txID,
		EntryType:   outbox.EntryTypeCommitmentV1,
		Commitment:  hash,
		ChannelID:   tx.ChannelID,
		ChaincodeID: event.ChaincodeID,
		BlockNumber: blockNumber,
		Timestamp:   tx.TimestampUnix,
	}, "", nil
}

func (s *Service) quarantine(tx *blockparse.Tx, blockNumber uint64, reason string) {
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
