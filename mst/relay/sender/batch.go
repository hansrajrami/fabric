package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

// processIndividualBatch anchors several entries in ONE EVM transaction via
// the contract's anchorBatch, keeping one on-chain record per transaction
// (verification unchanged). The contract skips already-anchored ids quietly,
// so at-least-once delivery stays exactly-once in effect.
func (s *Sender) processIndividualBatch(ctx context.Context, due []*outbox.Entry) error {
	toSubmit := s.shortCircuitAnchored(ctx, due)
	if len(toSubmit) == 0 {
		return nil
	}

	ids := make([][32]byte, len(toSubmit))
	commitments := make([][32]byte, len(toSubmit))
	blocks := make([]uint64, len(toSubmit))
	for i, e := range toSubmit {
		ids[i] = e.FabricTxID
		commitments[i] = e.Commitment
		blocks[i] = e.BlockNumber
	}

	txHash, err := s.client.SubmitAnchorBatch(ctx, ids, commitments, blocks)
	if err != nil {
		for _, e := range toSubmit {
			s.requeue(e, outbox.StatusPending, fmt.Errorf("batch submit: %w", err), s.entryLogger(e))
		}
		return nil
	}
	s.log.Info("anchor batch submitted", "entries", len(toSubmit), "evmTx", fmt.Sprintf("%x", txHash[:8]))

	submitted := s.markSubmitted(toSubmit, txHash, [32]byte{})
	s.confirmBatch(ctx, submitted, txHash)
	return nil
}

// processMerkleBatch aggregates the entries' commitments under one Merkle
// root and anchors ONLY the root (the proof of combination). Batch
// membership is durably recorded BEFORE submission so inclusion proofs can
// always be produced, even after a crash.
func (s *Sender) processMerkleBatch(ctx context.Context, due []*outbox.Entry) error {
	leaves := make([][32]byte, len(due))
	txIDs := make([][32]byte, len(due))
	for i, e := range due {
		leaves[i] = e.Commitment
		txIDs[i] = e.FabricTxID
	}
	root, err := canonical.MerkleRoot(leaves)
	if err != nil {
		// Duplicate commitments across entries would be a capture bug;
		// surface loudly and let per-entry processing untangle it.
		s.log.Error("merkle batch construction failed; falling back to per-entry", "err", err)
		return s.processIndividualBatch(ctx, due)
	}

	// Membership FIRST: the proof material must survive any crash between
	// here and confirmation.
	if err := s.store.PutBatch(root, txIDs); err != nil {
		return fmt.Errorf("sender: record batch membership: %w", err)
	}

	// Pre-submit short-circuit at ROOT level: a previous crash after submit
	// (or a hot-standby relayer with the same batch composition) already
	// anchored this exact root.
	rec, err := s.client.GetRoot(ctx, root)
	if err != nil {
		for _, e := range due {
			s.requeue(e, outbox.StatusPending, fmt.Errorf("pre-submit getRoot: %w", err), s.entryLogger(e))
		}
		return nil
	}
	if rec != nil {
		s.log.Info("batch root already anchored; short-circuiting", "root", fmt.Sprintf("%x", root[:8]))
		for _, e := range due {
			if _, err := s.store.Transition(e.FabricTxID, outbox.StatusPending, outbox.StatusConfirmed, func(en *outbox.Entry) {
				en.BatchRoot = root
			}); err == nil {
				s.finish(ctx, e.FabricTxID, s.entryLogger(e))
			}
		}
		return nil
	}

	txHash, err := s.client.SubmitAnchorRoot(ctx, root, uint64(len(leaves)))
	if err != nil {
		for _, e := range due {
			s.requeue(e, outbox.StatusPending, fmt.Errorf("root submit: %w", err), s.entryLogger(e))
		}
		return nil
	}
	s.log.Info("merkle batch root submitted", "entries", len(due),
		"root", fmt.Sprintf("%x", root[:8]), "evmTx", fmt.Sprintf("%x", txHash[:8]))

	submitted := s.markSubmitted(due, txHash, root)
	s.confirmBatch(ctx, submitted, txHash)
	return nil
}

// shortCircuitAnchored runs the per-entry getAnchor pre-check with bounded
// parallelism and returns the entries that still need submission. Entries
// already anchored jump to CONFIRMED and finish; mismatches freeze (loud
// log, no retry burn); lookup failures re-queue with backoff.
func (s *Sender) shortCircuitAnchored(ctx context.Context, due []*outbox.Entry) []*outbox.Entry {
	type verdict struct {
		entry  *outbox.Entry
		submit bool
	}
	verdicts := make([]verdict, len(due))

	sem := make(chan struct{}, s.cfg.Workers)
	var wg sync.WaitGroup
	for i, e := range due {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, e *outbox.Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			logger := s.entryLogger(e)
			rec, err := s.client.GetAnchor(ctx, e.FabricTxID)
			switch {
			case err != nil:
				s.requeue(e, outbox.StatusPending, fmt.Errorf("pre-submit getAnchor: %w", err), logger)
			case rec != nil && rec.Commitment != e.Commitment:
				logger.Error("anchor exists with mismatched commitment — investigate immediately",
					"onchain", fmt.Sprintf("%x", rec.Commitment), "local", fmt.Sprintf("%x", e.Commitment))
			case rec != nil:
				if _, err := s.store.Transition(e.FabricTxID, outbox.StatusPending, outbox.StatusConfirmed, nil); err == nil {
					s.finish(ctx, e.FabricTxID, logger)
				}
			default:
				verdicts[i] = verdict{entry: e, submit: true}
			}
		}(i, e)
	}
	wg.Wait()

	var out []*outbox.Entry
	for _, v := range verdicts {
		if v.submit {
			out = append(out, v.entry)
		}
	}
	return out
}

// markSubmitted transitions entries to SUBMITTED with the shared EVM tx
// hash (and batch root, when merkle). Entries whose transition fails are
// dropped from the confirmation set; recovery reconciles them.
func (s *Sender) markSubmitted(entries []*outbox.Entry, txHash, batchRoot [32]byte) []*outbox.Entry {
	var submitted []*outbox.Entry
	for _, e := range entries {
		if _, err := s.store.Transition(e.FabricTxID, outbox.StatusPending, outbox.StatusSubmitted, func(en *outbox.Entry) {
			en.EVMTxHash = txHash
			en.BatchRoot = batchRoot
			en.Attempts++
		}); err != nil {
			s.entryLogger(e).Error("record SUBMITTED failed", "err", err)
			continue
		}
		submitted = append(submitted, e)
	}
	return submitted
}

// confirmBatch waits once for the shared transaction, then advances every
// entry of the batch. Revert re-queues everything (the batch shares one
// fate); timeout leaves entries SUBMITTED for recovery.
func (s *Sender) confirmBatch(ctx context.Context, entries []*outbox.Entry, txHash [32]byte) {
	if len(entries) == 0 {
		return
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.ConfirmTimeout)
	err := s.client.WaitConfirmed(waitCtx, txHash, s.cfg.Confirmations)
	cancel()
	if err != nil {
		if errors.Is(err, evm.ErrReverted) {
			for _, e := range entries {
				s.requeueFrom(e.FabricTxID, outbox.StatusSubmitted, e.Attempts, err, s.entryLogger(e))
			}
			return
		}
		s.log.Warn("batch confirmation wait ended without confirmation", "err", err, "entries", len(entries))
		return
	}
	s.log.Info("batch confirmed", "entries", len(entries), "confirmations", s.cfg.Confirmations)

	sem := make(chan struct{}, s.cfg.Workers)
	var wg sync.WaitGroup
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(e *outbox.Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			logger := s.entryLogger(e)
			if _, err := s.store.Transition(e.FabricTxID, outbox.StatusSubmitted, outbox.StatusConfirmed, nil); err != nil {
				logger.Error("record CONFIRMED failed", "err", err)
				return
			}
			s.finish(ctx, e.FabricTxID, logger)
		}(e)
	}
	wg.Wait()
}

func (s *Sender) entryLogger(e *outbox.Entry) *slog.Logger {
	return s.log.With("txID", fmt.Sprintf("%x", e.FabricTxID[:8]), "block", e.BlockNumber)
}
