package outbox

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func openTemp(t *testing.T) (*LevelDB, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func txid(b byte) [32]byte {
	var id [32]byte
	for i := range id {
		id[i] = b
	}
	return id
}

func entry(b byte, block uint64) *Entry {
	return &Entry{
		FabricTxID:  txid(b),
		Commitment:  txid(b ^ 0xFF),
		ChannelID:   "mychannel",
		ChaincodeID: "mst-example",
		BlockNumber: block,
		Timestamp:   1720000000,
	}
}

func TestPutBlockInsertsPendingAndAdvancesCheckpoint(t *testing.T) {
	s, _ := openTemp(t)

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
}

func TestPutBlockIsIdempotentOnRedelivery(t *testing.T) {
	s, _ := openTemp(t)

	if _, err := s.PutBlock([]*Entry{entry(1, 7)}, 8); err != nil {
		t.Fatal(err)
	}
	// Move the entry forward, then redeliver the same block.
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
}

func TestTransitionHappyPathToDone(t *testing.T) {
	s, _ := openTemp(t)
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
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Fatalf("DONE list: %d", len(done))
	}
	pending, err := s.ListByStatus(StatusPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("PENDING index not cleaned: %d", len(pending))
	}
}

func TestTransitionCASRejectsWrongStatus(t *testing.T) {
	s, _ := openTemp(t)
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
}

func TestRecoveryTransitions(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.PutBlock([]*Entry{entry(1, 1), entry(2, 1)}, 2); err != nil {
		t.Fatal(err)
	}

	// Failed submission re-queues: SUBMITTED -> PENDING.
	if _, err := s.Transition(txid(1), StatusPending, StatusSubmitted, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(txid(1), StatusSubmitted, StatusPending, func(e *Entry) {
		e.Attempts++
		e.NextRetryAt = 12345
	}); err != nil {
		t.Fatal(err)
	}

	// Pre-submit getAnchor short-circuit: PENDING -> CONFIRMED.
	if _, err := s.Transition(txid(2), StatusPending, StatusConfirmed, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCrashRecoveryReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBlock([]*Entry{entry(1, 5), entry(2, 5)}, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(txid(1), StatusPending, StatusSubmitted, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate an unclean shutdown: close without any orderly draining (the
	// sync'd WAL is what recovery relies on).
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	re, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer re.Close()

	next, ok, err := re.Checkpoint()
	if err != nil || !ok || next != 6 {
		t.Fatalf("checkpoint after reopen: %d %v %v", next, ok, err)
	}
	e1, err := re.Get(txid(1))
	if err != nil || e1 == nil || e1.Status != StatusSubmitted {
		t.Fatalf("entry 1 after reopen: %+v %v", e1, err)
	}
	pending, err := re.ListByStatus(StatusPending, 0)
	if err != nil || len(pending) != 1 || pending[0].FabricTxID != txid(2) {
		t.Fatalf("pending after reopen: %v %v", pending, err)
	}
}

func TestCheckpointMonotonic(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.SetCheckpoint(10); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCheckpoint(9); !errors.Is(err, ErrCheckpointGoback) {
		t.Fatalf("want ErrCheckpointGoback, got %v", err)
	}
	if err := s.SetCheckpoint(10); err != nil {
		t.Fatalf("same-value checkpoint must be fine: %v", err)
	}
}

func TestListByStatusLimit(t *testing.T) {
	s, _ := openTemp(t)
	var entries []*Entry
	for i := byte(1); i <= 5; i++ {
		entries = append(entries, entry(i, 1))
	}
	if _, err := s.PutBlock(entries, 2); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListByStatus(StatusPending, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("limit: want 3, got %d", len(got))
	}
}

func TestQuarantineAndStats(t *testing.T) {
	s, _ := openTemp(t)
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
}

func TestConcurrentTransitionsSingleWinner(t *testing.T) {
	s, _ := openTemp(t)
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
}

func TestClosedStoreErrors(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBlock([]*Entry{entry(1, 1)}, 2); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func BenchmarkPutBlockSynced(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var id [32]byte
		copy(id[:], fmt.Sprintf("%032d", i))
		e := &Entry{FabricTxID: id, ChannelID: "c", ChaincodeID: "cc", BlockNumber: uint64(i)}
		if _, err := s.PutBlock([]*Entry{e}, uint64(i+1)); err != nil {
			b.Fatal(err)
		}
	}
}
