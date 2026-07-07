package outbox

import (
	"errors"
	"sync"
	"testing"
)

// runStoreSuite exercises the full Store contract against any backend. Both
// the LevelDB and CouchDB implementations must pass it identically.
func runStoreSuite(t *testing.T, open func(t *testing.T) Store) {
	t.Run("PutBlockInsertsPendingAndAdvancesCheckpoint", func(t *testing.T) {
		s := open(t)
		n, err := s.PutBlock([]*Entry{entry(1, 7), entry(2, 7)}, 8)
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("inserted: want 2, got %d", n)
		}
		got, err := s.Get(txid(1))
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Status != StatusPending {
			t.Fatalf("entry 1: %+v", got)
		}
		if got.EntryType != EntryTypeCommitmentV1 {
			t.Fatalf("entry type defaulted wrong: %q", got.EntryType)
		}
		next, ok, err := s.Checkpoint()
		if err != nil || !ok || next != 8 {
			t.Fatalf("checkpoint: %d %v %v", next, ok, err)
		}
	})

	t.Run("PutBlockIsIdempotentOnRedelivery", func(t *testing.T) {
		s := open(t)
		if _, err := s.PutBlock([]*Entry{entry(1, 7)}, 8); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Transition(txid(1), StatusPending, StatusSubmitted, nil); err != nil {
			t.Fatal(err)
		}
		n, err := s.PutBlock([]*Entry{entry(1, 7)}, 8)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("redelivery must insert nothing, inserted %d", n)
		}
		got, err := s.Get(txid(1))
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusSubmitted {
			t.Fatalf("redelivery clobbered status: %s", got.Status)
		}
	})

	t.Run("RedeliveredOlderBlockNeverRegressesCheckpoint", func(t *testing.T) {
		s := open(t)
		if _, err := s.PutBlock([]*Entry{entry(1, 9)}, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutBlock([]*Entry{entry(2, 3)}, 4); err != nil {
			t.Fatal(err)
		}
		next, _, err := s.Checkpoint()
		if err != nil || next != 10 {
			t.Fatalf("checkpoint regressed: %d %v", next, err)
		}
	})

	t.Run("TransitionHappyPathToDone", func(t *testing.T) {
		s := open(t)
		if _, err := s.PutBlock([]*Entry{entry(1, 1)}, 2); err != nil {
			t.Fatal(err)
		}
		evm := txid(0xEE)
		e, err := s.Transition(txid(1), StatusPending, StatusSubmitted, func(e *Entry) {
			e.EVMTxHash = evm
			e.Attempts++
		})
		if err != nil {
			t.Fatal(err)
		}
		if e.EVMTxHash != evm || e.Attempts != 1 {
			t.Fatalf("mutate not applied: %+v", e)
		}
		for _, step := range []struct{ from, to Status }{
			{StatusSubmitted, StatusConfirmed},
			{StatusConfirmed, StatusWrittenBack},
			{StatusWrittenBack, StatusDone},
		} {
			if _, err := s.Transition(txid(1), step.from, step.to, nil); err != nil {
				t.Fatalf("%s -> %s: %v", step.from, step.to, err)
			}
		}
		done, err := s.ListByStatus(StatusDone, 0)
		if err != nil || len(done) != 1 {
			t.Fatalf("DONE list: %d %v", len(done), err)
		}
		pending, err := s.ListByStatus(StatusPending, 0)
		if err != nil || len(pending) != 0 {
			t.Fatalf("PENDING index not cleaned: %d %v", len(pending), err)
		}
	})

	t.Run("TransitionCASRejections", func(t *testing.T) {
		s := open(t)
		if _, err := s.PutBlock([]*Entry{entry(1, 1)}, 2); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Transition(txid(1), StatusSubmitted, StatusConfirmed, nil); !errors.Is(err, ErrWrongStatus) {
			t.Fatalf("want ErrWrongStatus, got %v", err)
		}
		if _, err := s.Transition(txid(1), StatusPending, StatusDone, nil); !errors.Is(err, ErrBadTransition) {
			t.Fatalf("want ErrBadTransition, got %v", err)
		}
		if _, err := s.Transition(txid(9), StatusPending, StatusSubmitted, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("RecoveryTransitions", func(t *testing.T) {
		s := open(t)
		if _, err := s.PutBlock([]*Entry{entry(1, 1), entry(2, 1)}, 2); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Transition(txid(1), StatusPending, StatusSubmitted, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Transition(txid(1), StatusSubmitted, StatusPending, func(e *Entry) {
			e.Attempts++
			e.NextRetryAt = 12345
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Transition(txid(2), StatusPending, StatusConfirmed, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("UpdateKeepsStatus", func(t *testing.T) {
		s := open(t)
		if _, err := s.PutBlock([]*Entry{entry(1, 1)}, 2); err != nil {
			t.Fatal(err)
		}
		e, err := s.Update(txid(1), StatusPending, func(e *Entry) {
			e.Attempts = 3
			e.NextRetryAt = 999
		})
		if err != nil {
			t.Fatal(err)
		}
		if e.Attempts != 3 || e.NextRetryAt != 999 || e.Status != StatusPending {
			t.Fatalf("update: %+v", e)
		}
		if _, err := s.Update(txid(1), StatusSubmitted, nil); !errors.Is(err, ErrWrongStatus) {
			t.Fatalf("want ErrWrongStatus, got %v", err)
		}
	})

	t.Run("CheckpointMonotonic", func(t *testing.T) {
		s := open(t)
		if err := s.SetCheckpoint(10); err != nil {
			t.Fatal(err)
		}
		if err := s.SetCheckpoint(9); !errors.Is(err, ErrCheckpointGoback) {
			t.Fatalf("want ErrCheckpointGoback, got %v", err)
		}
		if err := s.SetCheckpoint(10); err != nil {
			t.Fatalf("same-value checkpoint must be fine: %v", err)
		}
	})

	t.Run("ListByStatusLimit", func(t *testing.T) {
		s := open(t)
		var entries []*Entry
		for i := byte(1); i <= 5; i++ {
			entries = append(entries, entry(i, 1))
		}
		if _, err := s.PutBlock(entries, 2); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListByStatus(StatusPending, 3)
		if err != nil || len(got) != 3 {
			t.Fatalf("limit: want 3, got %d (%v)", len(got), err)
		}
	})

	t.Run("QuarantineAndStats", func(t *testing.T) {
		s := open(t)
		if _, err := s.PutBlock([]*Entry{entry(1, 1), entry(2, 1)}, 2); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Transition(txid(1), StatusPending, StatusSubmitted, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Quarantine(txid(9), 1, "malformed payload", []byte{0xBA, 0xD1}); err != nil {
			t.Fatal(err)
		}
		stats, err := s.Stats()
		if err != nil {
			t.Fatal(err)
		}
		if stats.CountByStatus[StatusPending] != 1 || stats.CountByStatus[StatusSubmitted] != 1 {
			t.Fatalf("counts: %+v", stats.CountByStatus)
		}
		if stats.Quarantined != 1 {
			t.Fatalf("quarantined: %d", stats.Quarantined)
		}
		if stats.OldestActiveAge == 0 {
			t.Fatal("oldest active age missing")
		}
	})

	t.Run("ConcurrentTransitionsSingleWinner", func(t *testing.T) {
		s := open(t)
		if _, err := s.PutBlock([]*Entry{entry(1, 1)}, 2); err != nil {
			t.Fatal(err)
		}
		const workers = 16
		var wg sync.WaitGroup
		wins := make(chan struct{}, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Transition(txid(1), StatusPending, StatusSubmitted, nil); err == nil {
					wins <- struct{}{}
				}
			}()
		}
		wg.Wait()
		close(wins)
		count := 0
		for range wins {
			count++
		}
		if count != 1 {
			t.Fatalf("CAS must admit exactly one winner, got %d", count)
		}
	})

	t.Run("ClosedStoreErrors", func(t *testing.T) {
		s := open(t)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutBlock([]*Entry{entry(1, 1)}, 2); !errors.Is(err, ErrClosed) {
			t.Fatalf("want ErrClosed, got %v", err)
		}
	})
}
