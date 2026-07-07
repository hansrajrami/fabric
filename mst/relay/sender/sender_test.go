package sender

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

// fakeClient is a scriptable in-memory MST chain.
type fakeClient struct {
	mu sync.Mutex

	anchored map[[32]byte]*evm.AnchorRecord // fabricTxID -> record
	receipts map[[32]byte]bool              // evm tx hash -> included

	submitFailures int  // fail this many submits before succeeding
	submitBlackout bool // submits succeed but tx never lands (mempool drop)
	confirmRevert  bool // confirmations report revert
	getAnchorErr   error

	submits  int
	nextHash byte
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		anchored: map[[32]byte]*evm.AnchorRecord{},
		receipts: map[[32]byte]bool{},
	}
}

func (f *fakeClient) GetAnchor(_ context.Context, txID [32]byte) (*evm.AnchorRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getAnchorErr != nil {
		return nil, f.getAnchorErr
	}
	return f.anchored[txID], nil
}

func (f *fakeClient) SubmitAnchor(_ context.Context, txID, commitment [32]byte, blockNumber uint64) ([32]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	if f.submitFailures > 0 {
		f.submitFailures--
		return [32]byte{}, errors.New("rpc unreachable")
	}
	f.nextHash++
	var hash [32]byte
	hash[0] = 0xE0
	hash[1] = f.nextHash
	if !f.submitBlackout {
		f.receipts[hash] = true
		if _, exists := f.anchored[txID]; !exists { // contract idempotency
			f.anchored[txID] = &evm.AnchorRecord{
				Commitment: commitment, BlockNumber: blockNumber, EVMTimestamp: 1720001111, Exists: true,
			}
		}
	}
	return hash, nil
}

func (f *fakeClient) TxIncluded(_ context.Context, txHash [32]byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.receipts[txHash], nil
}

func (f *fakeClient) WaitConfirmed(ctx context.Context, txHash [32]byte, _ uint64) error {
	f.mu.Lock()
	included := f.receipts[txHash]
	revert := f.confirmRevert
	f.mu.Unlock()
	if revert {
		return evm.ErrReverted
	}
	if !included {
		<-ctx.Done() // never confirms; honor the timeout
		return ctx.Err()
	}
	return nil
}

// recordingWriteBack captures write-back calls; optionally fails first N.
type recordingWriteBack struct {
	mu       sync.Mutex
	recorded [][32]byte
	failures int
}

func (r *recordingWriteBack) Record(_ context.Context, e *outbox.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures > 0 {
		r.failures--
		return errors.New("fabric unavailable")
	}
	r.recorded = append(r.recorded, e.FabricTxID)
	return nil
}

func openStore(t *testing.T) outbox.Store {
	t.Helper()
	s, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedPending(t *testing.T, s outbox.Store, ids ...byte) {
	t.Helper()
	var entries []*outbox.Entry
	for _, b := range ids {
		var id, commitment [32]byte
		id[0] = b
		commitment[0] = b ^ 0xFF
		entries = append(entries, &outbox.Entry{
			FabricTxID: id, Commitment: commitment,
			ChannelID: "ch", ChaincodeID: "cc", BlockNumber: uint64(b), Timestamp: 1720000000,
		})
	}
	if _, err := s.PutBlock(entries, uint64(len(ids))); err != nil {
		t.Fatal(err)
	}
}

func newSender(t *testing.T, s outbox.Store, c AnchorClient, wb WriteBack, cfg Config) *Sender {
	t.Helper()
	if cfg.Backoff == (Backoff{}) {
		cfg.Backoff = Backoff{Min: time.Millisecond, Max: 2 * time.Millisecond}
	}
	if cfg.ConfirmTimeout == 0 {
		cfg.ConfirmTimeout = 50 * time.Millisecond
	}
	snd, err := New(s, c, wb, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return snd
}

func txKey(b byte) [32]byte {
	var id [32]byte
	id[0] = b
	return id
}

func statusOf(t *testing.T, s outbox.Store, b byte) outbox.Status {
	t.Helper()
	e, err := s.Get(txKey(b))
	if err != nil || e == nil {
		t.Fatalf("get %d: %v %v", b, e, err)
	}
	return e.Status
}

func TestHappyPathPendingToDone(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	wb := &recordingWriteBack{}
	seedPending(t, store, 1, 2, 3)

	snd := newSender(t, store, client, wb, Config{})
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, b := range []byte{1, 2, 3} {
		if got := statusOf(t, store, b); got != outbox.StatusDone {
			t.Fatalf("entry %d: want DONE, got %s", b, got)
		}
	}
	if len(wb.recorded) != 3 {
		t.Fatalf("write-backs: %d", len(wb.recorded))
	}
	if client.submits != 3 {
		t.Fatalf("submits: %d", client.submits)
	}
}

func TestAlreadyAnchoredShortCircuitsWithoutGas(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	seedPending(t, store, 7)
	// Another relayer already anchored it with OUR commitment.
	e, _ := store.Get(txKey(7))
	client.anchored[txKey(7)] = &evm.AnchorRecord{Commitment: e.Commitment, Exists: true}

	snd := newSender(t, store, client, &recordingWriteBack{}, Config{})
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, store, 7); got != outbox.StatusDone {
		t.Fatalf("want DONE, got %s", got)
	}
	if client.submits != 0 {
		t.Fatalf("short-circuit must not submit; submits=%d", client.submits)
	}
}

func TestCommitmentMismatchIsNotRetried(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	seedPending(t, store, 7)
	client.anchored[txKey(7)] = &evm.AnchorRecord{Commitment: txKey(0x66), Exists: true}

	snd := newSender(t, store, client, &recordingWriteBack{}, Config{})
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Entry must remain PENDING (a human decision), not thrash or advance.
	if got := statusOf(t, store, 7); got != outbox.StatusPending {
		t.Fatalf("want PENDING, got %s", got)
	}
	e, _ := store.Get(txKey(7))
	if e.Attempts != 0 {
		t.Fatal("mismatch must not burn retry attempts")
	}
}

func TestSubmitFailureRetriesWithBackoff(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	client.submitFailures = 2
	seedPending(t, store, 1)

	// Big backoff so NextRetryAt (unix seconds) lands measurably in the
	// future; the test forces due-ness explicitly.
	snd := newSender(t, store, client, &recordingWriteBack{}, Config{
		Backoff: Backoff{Min: time.Hour, Max: 2 * time.Hour},
	})

	// Round 1: submit fails, entry re-queued with NextRetryAt in the future.
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := store.Get(txKey(1))
	if e.Status != outbox.StatusPending || e.Attempts != 1 || e.NextRetryAt == 0 {
		t.Fatalf("after failure: %+v", e)
	}

	// While NextRetryAt is in the future the entry is not due.
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e2, _ := store.Get(txKey(1)); e2.Attempts != 1 {
		t.Fatal("not-due entry must not be retried yet")
	}

	// Force due and drain the second failure, then succeed.
	for i := 0; i < 2; i++ {
		if _, err := store.Update(txKey(1), outbox.StatusPending, func(en *outbox.Entry) { en.NextRetryAt = 0 }); err != nil {
			t.Fatal(err)
		}
		if err := snd.FlushOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := statusOf(t, store, 1); got != outbox.StatusDone {
		t.Fatalf("want DONE after retries, got %s", got)
	}
}

func TestConfirmTimeoutLeavesSubmittedThenRecoveryConfirms(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	client.submitBlackout = true // tx vanishes into the void
	seedPending(t, store, 4)

	snd := newSender(t, store, client, &recordingWriteBack{}, Config{ConfirmTimeout: 30 * time.Millisecond})
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, store, 4); got != outbox.StatusSubmitted {
		t.Fatalf("want SUBMITTED after confirm timeout, got %s", got)
	}

	// Recovery: tx not on chain and not in mempool -> back to PENDING.
	if err := snd.RecoverInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := store.Get(txKey(4))
	if e.Status != outbox.StatusPending || e.EVMTxHash != txKey(0) {
		t.Fatalf("after recovery: %+v", e)
	}

	// Chain heals; a due retry completes the pipeline.
	client.mu.Lock()
	client.submitBlackout = false
	client.mu.Unlock()
	if _, err := store.Update(txKey(4), outbox.StatusPending, func(en *outbox.Entry) { en.NextRetryAt = 0 }); err != nil {
		t.Fatal(err)
	}
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, store, 4); got != outbox.StatusDone {
		t.Fatalf("want DONE, got %s", got)
	}
}

func TestCrashRecoveryFromSubmittedThatLanded(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	wb := &recordingWriteBack{}
	seedPending(t, store, 9)

	// Simulate: previous process submitted (anchor landed) and crashed
	// before recording anything past SUBMITTED.
	e, _ := store.Get(txKey(9))
	client.anchored[txKey(9)] = &evm.AnchorRecord{Commitment: e.Commitment, Exists: true}
	if _, err := store.Transition(txKey(9), outbox.StatusPending, outbox.StatusSubmitted, func(en *outbox.Entry) {
		en.EVMTxHash = txKey(0xEE)
	}); err != nil {
		t.Fatal(err)
	}

	snd := newSender(t, store, client, wb, Config{})
	if err := snd.RecoverInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, store, 9); got != outbox.StatusDone {
		t.Fatalf("want DONE after recovery, got %s", got)
	}
	if client.submits != 0 {
		t.Fatal("recovery must never re-submit an anchored tx")
	}
	if len(wb.recorded) != 1 {
		t.Fatalf("write-backs: %d", len(wb.recorded))
	}
}

func TestWriteBackFailureRetriesWithoutReanchoring(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	wb := &recordingWriteBack{failures: 1}
	seedPending(t, store, 5)

	snd := newSender(t, store, client, wb, Config{})
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Write-back failed once: entry parks at CONFIRMED.
	if got := statusOf(t, store, 5); got != outbox.StatusConfirmed {
		t.Fatalf("want CONFIRMED, got %s", got)
	}

	// Recovery finishes write-back; no new submission happens.
	submitsBefore := client.submits
	if err := snd.RecoverInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, store, 5); got != outbox.StatusDone {
		t.Fatalf("want DONE, got %s", got)
	}
	if client.submits != submitsBefore {
		t.Fatal("write-back retry must not re-anchor")
	}
}

func TestRevertedAnchorRequeues(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	client.confirmRevert = true
	seedPending(t, store, 6)

	snd := newSender(t, store, client, &recordingWriteBack{}, Config{})
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := store.Get(txKey(6))
	if e.Status != outbox.StatusPending || e.Attempts == 0 {
		t.Fatalf("reverted anchor must re-queue with attempts: %+v", e)
	}
}

func TestBatchCadenceWaitsForThreshold(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	seedPending(t, store, 1, 2)

	snd := newSender(t, store, client, &recordingWriteBack{}, Config{
		Cadence: Cadence{Mode: CadenceBatch, N: 3, MaxWait: time.Hour},
	})
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.submits != 0 {
		t.Fatalf("below threshold must not flush; submits=%d", client.submits)
	}

	seedPending(t, store, 3)
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.submits != 3 {
		t.Fatalf("at threshold must flush all; submits=%d", client.submits)
	}
}

func TestBatchCadenceMaxWaitForcesFlush(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	seedPending(t, store, 1)

	snd := newSender(t, store, client, &recordingWriteBack{}, Config{
		Cadence: Cadence{Mode: CadenceBatch, N: 100, MaxWait: time.Nanosecond},
	})
	snd.nowFn = func() time.Time { return time.Now().Add(2 * time.Second) }
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.submits != 1 {
		t.Fatalf("aged entry must flush despite threshold; submits=%d", client.submits)
	}
}

func TestRunDrainsAndStopsOnCancel(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	seedPending(t, store, 1, 2)

	snd := newSender(t, store, client, &recordingWriteBack{}, Config{PollInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- snd.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for {
		if statusOf(t, store, 1) == outbox.StatusDone && statusOf(t, store, 2) == outbox.StatusDone {
			break
		}
		select {
		case <-deadline:
			t.Fatal("entries did not reach DONE in time")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run on cancel: %v", err)
	}
}

func TestMetricsHandler(t *testing.T) {
	store := openStore(t)
	seedPending(t, store, 1, 2)
	rec := httptest.NewRecorder()
	MetricsHandler(store).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `mst_outbox_entries{status="PENDING"} 2`) {
		t.Fatalf("metrics body:\n%s", body)
	}
	if !strings.Contains(body, "mst_outbox_quarantined_total 0") {
		t.Fatalf("metrics body:\n%s", body)
	}
}

func TestBackoffBoundsAndJitter(t *testing.T) {
	b := Backoff{Min: 100 * time.Millisecond, Max: time.Second}
	for attempt := uint32(1); attempt < 30; attempt++ {
		d := b.Delay(attempt)
		if d <= 0 || d > time.Second {
			t.Fatalf("attempt %d: delay %v out of bounds", attempt, d)
		}
	}
}
