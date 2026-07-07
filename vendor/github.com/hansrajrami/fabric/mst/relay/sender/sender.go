// Package sender drains the outbox to the MST anchor contract and records
// write-backs on Fabric (spec section 12). All slow, fallible network work
// lives here, decoupled from capture by the durable outbox. Delivery is
// at-least-once; the idempotent anchor contract and the pre-submit check
// make it exactly-once in effect. Ordering between entries is deliberately
// NOT guaranteed (spec section 11) — entries are independent and processed
// by parallel workers.
package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

// AnchorClient is the sender's view of the MST chain; *evm.Client implements
// it, and tests use a scripted fake.
type AnchorClient interface {
	GetAnchor(ctx context.Context, fabricTxID [32]byte) (*evm.AnchorRecord, error)
	SubmitAnchor(ctx context.Context, fabricTxID, commitment [32]byte, blockNumber uint64) ([32]byte, error)
	SubmitAnchorBatch(ctx context.Context, ids, commitments [][32]byte, blockNumbers []uint64) ([32]byte, error)
	SubmitAnchorRoot(ctx context.Context, root [32]byte, leafCount uint64) ([32]byte, error)
	GetRoot(ctx context.Context, root [32]byte) (*evm.RootRecord, error)
	TxIncluded(ctx context.Context, txHash [32]byte) (bool, error)
	WaitConfirmed(ctx context.Context, txHash [32]byte, confirmations uint64) error
}

// BatchStrategy selects how a flush's entries reach the chain.
type BatchStrategy string

const (
	// StrategyIndividual (default): every transaction gets its own on-chain
	// record. Multi-entry flushes share one EVM transaction via anchorBatch
	// (~40% gas saving); verification is unchanged (getAnchor per tx).
	StrategyIndividual BatchStrategy = "individual"
	// StrategyMerkle: one on-chain record per flush — the Merkle root over
	// the entries' commitments (~95% gas saving at N=20). Verifying a single
	// transaction additionally needs its inclusion proof (sibling hashes);
	// batch membership is persisted in the outbox and exportable with
	// mst-proof.
	StrategyMerkle BatchStrategy = "merkle"
)

// Validate normalizes and checks the strategy.
func (b *BatchStrategy) Validate() error {
	switch *b {
	case "", StrategyIndividual:
		*b = StrategyIndividual
	case StrategyMerkle:
	default:
		return fmt.Errorf("sender: unknown batch strategy %q (want %q or %q)", *b, StrategyIndividual, StrategyMerkle)
	}
	return nil
}

// Config tunes the sender.
type Config struct {
	// Confirmations required before an anchor counts as CONFIRMED
	// (reorg guard; 1 = included). Default 1.
	Confirmations uint64
	// Workers bounds concurrent in-flight entries. Default 4.
	Workers int
	// Cadence is the flush policy (default per-tx).
	Cadence Cadence
	// Strategy is the batching strategy (default individual).
	Strategy BatchStrategy
	// Backoff shapes per-entry retry delays.
	Backoff Backoff
	// PollInterval is the outbox re-examination period for per-tx/batch
	// cadences. Default 250ms: sub-second anchoring latency without
	// hammering the store.
	PollInterval time.Duration
	// ConfirmTimeout bounds one confirmation wait; on expiry the entry
	// stays SUBMITTED and is re-examined by recovery. Default 2 minutes.
	ConfirmTimeout time.Duration
}

func (c *Config) applyDefaults() error {
	if c.Confirmations == 0 {
		c.Confirmations = 1
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.ConfirmTimeout <= 0 {
		c.ConfirmTimeout = 2 * time.Minute
	}
	if err := c.Strategy.Validate(); err != nil {
		return err
	}
	return c.Cadence.Validate()
}

// Sender is the outbox-draining loop.
type Sender struct {
	store     outbox.Store
	client    AnchorClient
	writeback WriteBack
	cfg       Config
	log       *slog.Logger
	nowFn     func() time.Time
}

// New builds a sender. writeback may be nil (treated as NoopWriteBack).
func New(store outbox.Store, client AnchorClient, writeback WriteBack, cfg Config, log *slog.Logger) (*Sender, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if writeback == nil {
		writeback = NoopWriteBack{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Sender{store: store, client: client, writeback: writeback, cfg: cfg, log: log, nowFn: time.Now}, nil
}

// Run drives the sender until ctx is cancelled. It first recovers in-flight
// SUBMITTED entries from a previous life, then flushes on the configured
// cadence. Returns nil on cancellation.
func (s *Sender) Run(ctx context.Context) error {
	if err := s.RecoverInFlight(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("in-flight recovery incomplete; entries remain SUBMITTED and will be retried", "err", err)
	}

	ticker := time.NewTicker(s.cfg.Cadence.pollInterval(s.cfg.PollInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.FlushOnce(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("flush round failed", "err", err)
			}
		}
	}
}

// FlushOnce examines the outbox once and, if the cadence says go, pushes all
// due PENDING entries through submit → confirm → write-back with bounded
// parallelism. Individual entry failures are re-queued with backoff and do
// not fail the round; only outbox access errors are returned.
func (s *Sender) FlushOnce(ctx context.Context) error {
	pending, err := s.store.ListByStatus(outbox.StatusPending, 0)
	if err != nil {
		return err
	}
	now := s.nowFn().Unix()
	due := pending[:0]
	oldest := time.Duration(0)
	for _, e := range pending {
		if e.NextRetryAt > now {
			continue
		}
		due = append(due, e)
		if age := time.Duration(now-e.CreatedAt) * time.Second; age > oldest {
			oldest = age
		}
	}
	if !s.cfg.Cadence.shouldFlush(len(due), oldest, s.nowFn()) {
		return nil
	}

	if s.cfg.Strategy == StrategyMerkle && len(due) > 1 {
		return s.processMerkleBatch(ctx, due)
	}
	if s.cfg.Strategy == StrategyIndividual && len(due) > 1 {
		return s.processIndividualBatch(ctx, due)
	}

	sem := make(chan struct{}, s.cfg.Workers)
	var wg sync.WaitGroup
	for _, e := range due {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(e *outbox.Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			s.processPending(ctx, e)
		}(e)
	}
	wg.Wait()
	return nil
}

// processPending pushes one PENDING entry as far as possible.
func (s *Sender) processPending(ctx context.Context, e *outbox.Entry) {
	logger := s.log.With("txID", fmt.Sprintf("%x", e.FabricTxID[:8]), "block", e.BlockNumber)

	// Pre-submit short-circuit: if the tx is already anchored (previous
	// crash after submit, hot-standby relayer, duplicate capture), skip
	// spending gas and jump straight to CONFIRMED.
	rec, err := s.client.GetAnchor(ctx, e.FabricTxID)
	if err != nil {
		s.requeue(e, outbox.StatusPending, fmt.Errorf("pre-submit getAnchor: %w", err), logger)
		return
	}
	if rec != nil {
		if rec.Commitment != e.Commitment {
			// Anchored with a DIFFERENT commitment: never overwritable, so
			// retrying is pointless. Loudest possible log; needs a human.
			logger.Error("anchor exists with mismatched commitment — investigate immediately",
				"onchain", fmt.Sprintf("%x", rec.Commitment), "local", fmt.Sprintf("%x", e.Commitment))
			return
		}
		if _, err := s.store.Transition(e.FabricTxID, outbox.StatusPending, outbox.StatusConfirmed, nil); err != nil {
			logger.Error("short-circuit transition failed", "err", err)
			return
		}
		logger.Info("already anchored; short-circuited to CONFIRMED")
		s.finish(ctx, e.FabricTxID, logger)
		return
	}

	txHash, err := s.client.SubmitAnchor(ctx, e.FabricTxID, e.Commitment, e.BlockNumber)
	if err != nil {
		s.requeue(e, outbox.StatusPending, fmt.Errorf("submit: %w", err), logger)
		return
	}
	if _, err := s.store.Transition(e.FabricTxID, outbox.StatusPending, outbox.StatusSubmitted, func(en *outbox.Entry) {
		en.EVMTxHash = txHash
		en.Attempts++
	}); err != nil {
		// Storage refused (or crashed state): the submitted tx is still
		// safe — recovery's getAnchor check will reconcile.
		logger.Error("record SUBMITTED failed", "err", err)
		return
	}
	logger.Info("anchor submitted", "evmTx", fmt.Sprintf("%x", txHash[:8]))

	s.confirmSubmitted(ctx, e.FabricTxID, txHash, logger)
}

// confirmSubmitted waits for confirmations and advances SUBMITTED→CONFIRMED,
// then runs write-back.
func (s *Sender) confirmSubmitted(ctx context.Context, txID [32]byte, txHash [32]byte, logger *slog.Logger) {
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.ConfirmTimeout)
	err := s.client.WaitConfirmed(waitCtx, txHash, s.cfg.Confirmations)
	cancel()
	if err != nil {
		if errors.Is(err, evm.ErrReverted) {
			// Contract reverted: configuration problem (e.g. allowlist).
			// Re-queue with backoff so a fixed allowlist heals the backlog.
			e, gerr := s.store.Get(txID)
			if gerr == nil && e != nil {
				s.requeueFrom(txID, outbox.StatusSubmitted, e.Attempts, err, logger)
			}
			return
		}
		// Timeout/transport error: stay SUBMITTED; recovery reconciles.
		logger.Warn("confirmation wait ended without confirmation", "err", err)
		return
	}
	if _, err := s.store.Transition(txID, outbox.StatusSubmitted, outbox.StatusConfirmed, nil); err != nil {
		logger.Error("record CONFIRMED failed", "err", err)
		return
	}
	logger.Info("anchor confirmed", "confirmations", s.cfg.Confirmations)
	s.finish(ctx, txID, logger)
}

// finish runs write-back for a CONFIRMED entry and moves it to DONE.
func (s *Sender) finish(ctx context.Context, txID [32]byte, logger *slog.Logger) {
	e, err := s.store.Get(txID)
	if err != nil || e == nil || e.Status != outbox.StatusConfirmed {
		return
	}
	if err := s.writeback.Record(ctx, e); err != nil {
		// Stay CONFIRMED; the next round retries write-back only (never
		// re-anchors: submit path is behind the PENDING status).
		logger.Warn("write-back failed; will retry", "err", err)
		return
	}
	if _, err := s.store.Transition(txID, outbox.StatusConfirmed, outbox.StatusWrittenBack, nil); err != nil {
		logger.Error("record WRITTEN_BACK failed", "err", err)
		return
	}
	if _, err := s.store.Transition(txID, outbox.StatusWrittenBack, outbox.StatusDone, nil); err != nil {
		logger.Error("record DONE failed", "err", err)
		return
	}
	logger.Info("anchoring complete")
}

// RecoverInFlight reconciles SUBMITTED entries (crash recovery and
// confirmation-timeout pickup) and finishes CONFIRMED entries whose
// write-back never completed.
func (s *Sender) RecoverInFlight(ctx context.Context) error {
	submitted, err := s.store.ListByStatus(outbox.StatusSubmitted, 0)
	if err != nil {
		return err
	}
	for _, e := range submitted {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger := s.log.With("txID", fmt.Sprintf("%x", e.FabricTxID[:8]))

		// Merkle-batched entries are anchored under their batch root, not
		// under their own tx id: reconcile against getRoot instead.
		var landed bool
		var err error
		if e.BatchRoot != ([32]byte{}) {
			var root *evm.RootRecord
			root, err = s.client.GetRoot(ctx, e.BatchRoot)
			landed = root != nil
		} else {
			var rec *evm.AnchorRecord
			rec, err = s.client.GetAnchor(ctx, e.FabricTxID)
			landed = rec != nil
		}
		if err != nil {
			logger.Warn("recovery anchor lookup failed; leaving SUBMITTED", "err", err)
			continue
		}
		switch {
		case landed:
			// Landed. (Commitment mismatch is impossible for our own
			// submission and fatal-logged on the pending path.)
			if _, err := s.store.Transition(e.FabricTxID, outbox.StatusSubmitted, outbox.StatusConfirmed, nil); err == nil {
				s.finish(ctx, e.FabricTxID, logger)
			}
		default:
			included, err := s.client.TxIncluded(ctx, e.EVMTxHash)
			if err != nil {
				logger.Warn("recovery receipt check failed; leaving SUBMITTED", "err", err)
				continue
			}
			if included {
				// Included but not visible as anchored: pathological (a
				// reverted duplicate?); resolve via confirmation path.
				s.confirmSubmitted(ctx, e.FabricTxID, e.EVMTxHash, logger)
				continue
			}
			// Never made it (dropped from mempool / crashed pre-send):
			// re-queue for a fresh submission.
			s.requeueFrom(e.FabricTxID, outbox.StatusSubmitted, e.Attempts, errors.New("submitted tx not found on chain"), logger)
		}
	}

	// Write-back stragglers.
	confirmed, err := s.store.ListByStatus(outbox.StatusConfirmed, 0)
	if err != nil {
		return err
	}
	for _, e := range confirmed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.finish(ctx, e.FabricTxID, s.log.With("txID", fmt.Sprintf("%x", e.FabricTxID[:8])))
	}
	return nil
}

// requeue keeps a PENDING entry pending with bumped attempt/backoff state.
func (s *Sender) requeue(e *outbox.Entry, status outbox.Status, cause error, logger *slog.Logger) {
	attempts := e.Attempts + 1
	delay := s.cfg.Backoff.Delay(attempts)
	if _, err := s.store.Update(e.FabricTxID, status, func(en *outbox.Entry) {
		en.Attempts = attempts
		en.NextRetryAt = s.nowFn().Add(delay).Unix()
	}); err != nil {
		logger.Error("requeue update failed", "err", err)
		return
	}
	logger.Warn("entry re-queued", "attempts", attempts, "retryIn", delay.Round(time.Millisecond), "cause", cause)
}

// requeueFrom moves a SUBMITTED entry back to PENDING with backoff.
func (s *Sender) requeueFrom(txID [32]byte, from outbox.Status, attempts uint32, cause error, logger *slog.Logger) {
	attempts++
	delay := s.cfg.Backoff.Delay(attempts)
	if _, err := s.store.Transition(txID, from, outbox.StatusPending, func(en *outbox.Entry) {
		en.Attempts = attempts
		en.NextRetryAt = s.nowFn().Add(delay).Unix()
		en.EVMTxHash = [32]byte{}
		en.BatchRoot = [32]byte{}
	}); err != nil {
		logger.Error("requeue transition failed", "err", err)
		return
	}
	logger.Warn("entry re-queued for fresh submission", "attempts", attempts, "retryIn", delay.Round(time.Millisecond), "cause", cause)
}
