package sender

import (
	"context"
	"testing"
	"time"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

func merkleSender(t *testing.T, s outbox.Store, c AnchorClient, wb WriteBack) *Sender {
	t.Helper()
	return newSender(t, s, c, wb, Config{Strategy: StrategyMerkle})
}

func TestMerkleBatchHappyPath(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	wb := &recordingWriteBack{}
	seedPending(t, store, 1, 2, 3, 4, 5)

	snd := merkleSender(t, store, client, wb)
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// ONE root submission for the whole batch, zero per-tx submissions.
	if client.rootSubmits != 1 || client.submits != 0 || client.batchSubmits != 0 {
		t.Fatalf("submissions: root=%d single=%d batch=%d", client.rootSubmits, client.submits, client.batchSubmits)
	}

	// Every entry DONE, carrying the shared batch root; membership recorded.
	var root [32]byte
	for _, b := range []byte{1, 2, 3, 4, 5} {
		if got := statusOf(t, store, b); got != outbox.StatusDone {
			t.Fatalf("entry %d: want DONE, got %s", b, got)
		}
		e, _ := store.Get(txKey(b))
		if e.BatchRoot == ([32]byte{}) {
			t.Fatalf("entry %d: batch root not recorded", b)
		}
		root = e.BatchRoot
	}
	members, err := store.GetBatch(root)
	if err != nil || len(members) != 5 {
		t.Fatalf("batch membership: %d %v", len(members), err)
	}
	if len(wb.recorded) != 5 {
		t.Fatalf("write-backs: %d", len(wb.recorded))
	}

	// The root the sender anchored must be reproducible from the entries'
	// commitments — that is what makes inclusion proofs verifiable later.
	var leaves [][32]byte
	for _, b := range []byte{1, 2, 3, 4, 5} {
		e, _ := store.Get(txKey(b))
		leaves = append(leaves, e.Commitment)
	}
	want, err := canonical.MerkleRoot(leaves)
	if err != nil {
		t.Fatal(err)
	}
	if root != want {
		t.Fatalf("anchored root %x != recomputed %x", root, want)
	}
	if client.roots[root] == nil || client.roots[root].LeafCount != 5 {
		t.Fatalf("on-chain root record: %+v", client.roots[root])
	}

	// Inclusion proof round-trip for one member.
	e1, _ := store.Get(txKey(1))
	proof, err := canonical.MerkleProof(leaves, e1.Commitment)
	if err != nil {
		t.Fatal(err)
	}
	if !canonical.VerifyMerkleProof(root, e1.Commitment, proof) {
		t.Fatal("inclusion proof must verify against the anchored root")
	}
}

func TestMerkleBatchRootAlreadyAnchoredShortCircuits(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	seedPending(t, store, 1, 2, 3)

	// Precompute the root the sender will build and pre-anchor it.
	var leaves [][32]byte
	for _, b := range []byte{1, 2, 3} {
		e, _ := store.Get(txKey(b))
		leaves = append(leaves, e.Commitment)
	}
	root, err := canonical.MerkleRoot(leaves)
	if err != nil {
		t.Fatal(err)
	}
	client.roots[root] = &evm.RootRecord{LeafCount: 3, Exists: true}

	snd := merkleSender(t, store, client, &recordingWriteBack{})
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.rootSubmits != 0 {
		t.Fatalf("already-anchored root must not be resubmitted; rootSubmits=%d", client.rootSubmits)
	}
	for _, b := range []byte{1, 2, 3} {
		if got := statusOf(t, store, b); got != outbox.StatusDone {
			t.Fatalf("entry %d: want DONE, got %s", b, got)
		}
	}
}

func TestMerkleBatchRecoveryFromSubmitted(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	client.submitBlackout = true // root tx vanishes
	seedPending(t, store, 1, 2)

	snd := merkleSender(t, store, client, &recordingWriteBack{})
	snd.cfg.ConfirmTimeout = 30 * time.Millisecond
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte{1, 2} {
		if got := statusOf(t, store, b); got != outbox.StatusSubmitted {
			t.Fatalf("entry %d: want SUBMITTED after timeout, got %s", b, got)
		}
	}

	// Recovery: root not on chain, tx not in mempool -> re-queue with the
	// batch root cleared (the next flush may compose a different batch).
	if err := snd.RecoverInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte{1, 2} {
		e, _ := store.Get(txKey(b))
		if e.Status != outbox.StatusPending || e.BatchRoot != ([32]byte{}) {
			t.Fatalf("entry %d after recovery: %+v", b, e)
		}
	}

	// Chain heals: a due retry completes via a fresh root.
	client.mu.Lock()
	client.submitBlackout = false
	client.mu.Unlock()
	for _, b := range []byte{1, 2} {
		if _, err := store.Update(txKey(b), outbox.StatusPending, func(en *outbox.Entry) { en.NextRetryAt = 0 }); err != nil {
			t.Fatal(err)
		}
	}
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte{1, 2} {
		if got := statusOf(t, store, b); got != outbox.StatusDone {
			t.Fatalf("entry %d: want DONE, got %s", b, got)
		}
	}
}

func TestMerkleBatchRecoveryRootLanded(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	wb := &recordingWriteBack{}
	seedPending(t, store, 7, 8)

	// Simulate: previous process anchored the root and crashed after
	// recording SUBMITTED.
	var leaves [][32]byte
	for _, b := range []byte{7, 8} {
		e, _ := store.Get(txKey(b))
		leaves = append(leaves, e.Commitment)
	}
	root, err := canonical.MerkleRoot(leaves)
	if err != nil {
		t.Fatal(err)
	}
	client.roots[root] = &evm.RootRecord{LeafCount: 2, Exists: true}
	for _, b := range []byte{7, 8} {
		if _, err := store.Transition(txKey(b), outbox.StatusPending, outbox.StatusSubmitted, func(en *outbox.Entry) {
			en.EVMTxHash = txKey(0xEE)
			en.BatchRoot = root
		}); err != nil {
			t.Fatal(err)
		}
	}

	snd := merkleSender(t, store, client, wb)
	if err := snd.RecoverInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte{7, 8} {
		if got := statusOf(t, store, b); got != outbox.StatusDone {
			t.Fatalf("entry %d: want DONE after recovery, got %s", b, got)
		}
	}
	if client.rootSubmits != 0 {
		t.Fatal("recovery must never re-anchor a landed root")
	}
}

func TestCronCadenceFiresOnSchedule(t *testing.T) {
	store := openStore(t)
	client := newFakeClient()
	seedPending(t, store, 1)

	base := time.Date(2026, 7, 7, 10, 0, 30, 0, time.UTC)
	now := base
	snd := newSender(t, store, client, &recordingWriteBack{}, Config{
		Cadence: Cadence{Mode: CadenceCron, Cron: "*/5 * * * *"}, // every 5 minutes
	})
	snd.nowFn = func() time.Time { return now }

	// First examination only arms the schedule (next fire 10:05).
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.submits+client.batchSubmits != 0 {
		t.Fatal("cron must not flush before its first boundary")
	}

	// Still before the boundary.
	now = base.Add(3 * time.Minute)
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.submits+client.batchSubmits != 0 {
		t.Fatal("cron must not flush mid-window")
	}

	// Past the boundary: flush happens.
	now = base.Add(5 * time.Minute)
	if err := snd.FlushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, store, 1); got != outbox.StatusDone {
		t.Fatalf("want DONE after cron fire, got %s", got)
	}
}

func TestCronCadenceRejectsBadExpression(t *testing.T) {
	_, err := New(openStore(t), newFakeClient(), nil, Config{
		Cadence: Cadence{Mode: CadenceCron, Cron: "not-a-cron"},
	}, nil)
	if err == nil {
		t.Fatal("invalid cron expression must be rejected at construction")
	}
}

func TestBatchStrategyValidation(t *testing.T) {
	if _, err := New(openStore(t), newFakeClient(), nil, Config{Strategy: "zip"}, nil); err == nil {
		t.Fatal("unknown strategy must be rejected")
	}
	s := BatchStrategy("")
	if err := s.Validate(); err != nil || s != StrategyIndividual {
		t.Fatalf("empty strategy must default to individual: %v %q", err, s)
	}
}
