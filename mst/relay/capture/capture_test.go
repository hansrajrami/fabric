package capture

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
	"github.com/hansrajrami/fabric/mst/relay/txmodel"
)

func txID(seed string) string {
	return strings.Repeat("0", 64-len(seed)) + seed
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

func validPayload(t *testing.T) []byte {
	t.Helper()
	amount, err := canonical.IntField("amount", big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := canonical.Encode(canonical.Payload{Fields: []canonical.Field{amount}})
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func optedTx(id string, ts uint64, valid bool, chaincode string, payload []byte) txmodel.Tx {
	return txmodel.Tx{
		TxID: txID(id), ChannelID: "mychannel", TimestampUnix: ts, Valid: valid,
		Events: []txmodel.Event{{ChaincodeID: chaincode, EventName: "MSTProofRequest", Payload: payload}},
	}
}

// run drains a finite StaticSource through the service; the "stream ended
// unexpectedly" error is the expected terminal state for a finite source.
func run(t *testing.T, svc *Service, blocks ...*txmodel.Block) {
	t.Helper()
	svc.source = &StaticSource{BlocksList: blocks}
	err := svc.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stream ended") {
		t.Fatalf("finite source must end with stream-ended, got %v", err)
	}
}

func TestCaptureEndToEnd(t *testing.T) {
	store := openStore(t)
	payload := validPayload(t)

	blocks := []*txmodel.Block{
		{Number: 0, Txs: []txmodel.Tx{
			// opted-in, valid -> captured
			optedTx("a1", 1720000001, true, "mst-example", payload),
			// opted-in but invalid tx -> skipped
			optedTx("a2", 1720000002, false, "mst-example", payload),
		}},
		{Number: 1, Txs: []txmodel.Tx{
			// not opted in -> skipped
			{TxID: txID("b1"), ChannelID: "mychannel", TimestampUnix: 1720000003, Valid: true,
				Events: []txmodel.Event{{ChaincodeID: "mst-example", EventName: "SomethingElse", Payload: payload}}},
			// excluded chaincode (echo-loop guard) -> skipped
			optedTx("b2", 1720000004, true, "mst-anchor-status", payload),
		}},
		{Number: 2, Txs: []txmodel.Tx{
			// malformed payload -> quarantined, stream continues
			optedTx("c1", 1720000005, true, "mst-example", []byte{0xFF, 0xFF}),
			// opted-in, valid -> captured
			optedTx("c2", 1720000006, true, "mst-example", payload),
		}},
	}

	svc := New(nil, store, Config{ExcludeChaincodes: []string{"mst-anchor-status"}}, nil)
	run(t, svc, blocks...)

	pending, err := store.ListByStatus(outbox.StatusPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending: want 2 (a1, c2), got %d", len(pending))
	}

	next, ok, err := store.Checkpoint()
	if err != nil || !ok || next != 3 {
		t.Fatalf("checkpoint: %d %v %v", next, ok, err)
	}

	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("quarantined: want 1, got %d", stats.Quarantined)
	}

	// The captured commitment must equal an independent recomputation.
	var wantTxID [32]byte
	rawID, _ := hex.DecodeString(txID("a1"))
	copy(wantTxID[:], rawID)
	payloadHash := canonical.Keccak256(payload)
	c := canonical.NewCommitment(wantTxID, "mychannel", "mst-example", 0, 1720000001, payloadHash)
	want, err := c.Hash()
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(wantTxID)
	if err != nil || got == nil {
		t.Fatalf("entry a1: %v %v", got, err)
	}
	if got.Commitment != want {
		t.Fatalf("commitment mismatch:\n want %x\n  got %x", want, got.Commitment)
	}
	if got.EntryType != outbox.EntryTypeCommitmentV1 {
		t.Fatalf("entry type: %s", got.EntryType)
	}
}

func TestCaptureRestartExactlyOnce(t *testing.T) {
	store := openStore(t)
	payload := validPayload(t)

	mkBlock := func(n uint64, seed string) *txmodel.Block {
		return &txmodel.Block{Number: n, Txs: []txmodel.Tx{
			optedTx(seed, 1720000000+n, true, "mst-example", payload),
		}}
	}
	all := []*txmodel.Block{mkBlock(0, "f0"), mkBlock(1, "f1"), mkBlock(2, "f2"), mkBlock(3, "f3"), mkBlock(4, "f4")}

	// First run "crashes" after block 2 (finite source with only 3 blocks).
	svc := New(nil, store, Config{}, nil)
	run(t, svc, all[:3]...)

	next, _, err := store.Checkpoint()
	if err != nil || next != 3 {
		t.Fatalf("checkpoint after partial run: %d %v", next, err)
	}

	// Restart with the full stream: Run resumes from the checkpoint (the
	// StaticSource re-serves from block 3) and redelivered earlier blocks
	// would be idempotent anyway.
	svc2 := New(nil, store, Config{}, nil)
	run(t, svc2, all...)

	pending, err := store.ListByStatus(outbox.StatusPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 5 {
		t.Fatalf("exactly-once violated: want 5 entries, got %d", len(pending))
	}
	next, _, _ = store.Checkpoint()
	if next != 5 {
		t.Fatalf("final checkpoint: %d", next)
	}
	if svc2.Processed() != 2 {
		t.Fatalf("restart must only process blocks 3..4, processed %d", svc2.Processed())
	}
}

func TestCaptureRedeliveredBlockIsIdempotent(t *testing.T) {
	store := openStore(t)
	block := &txmodel.Block{Number: 9, Txs: []txmodel.Tx{
		optedTx("aa", 1720000009, true, "cc", validPayload(t)),
	}}

	svc := New(nil, store, Config{}, nil)
	if err := svc.ProcessBlock(block); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessBlock(block); err != nil {
		t.Fatal(err)
	}

	pending, err := store.ListByStatus(outbox.StatusPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("redelivery duplicated: %d", len(pending))
	}
}

func TestCaptureMissingTimestampQuarantined(t *testing.T) {
	store := openStore(t)
	block := &txmodel.Block{Number: 1, Txs: []txmodel.Tx{
		optedTx("ee", 0, true, "cc", validPayload(t)), // no timestamp
	}}
	svc := New(nil, store, Config{}, nil)
	if err := svc.ProcessBlock(block); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CountByStatus[outbox.StatusPending] != 0 {
		t.Fatalf("stats: %+v", stats)
	}
}

func TestCaptureAllModeAnchorsEveryValidTx(t *testing.T) {
	store := openStore(t)
	payload := validPayload(t)

	block := &txmodel.Block{Number: 4, Txs: []txmodel.Tx{
		// Plain business tx, NO event -> captured with the empty payload hash.
		{TxID: txID("a1"), ChannelID: "mychannel", ChaincodeID: "assets", TimestampUnix: 1720000001, Valid: true},
		// Opted-in tx -> captured with its declared payload, as in opt-in mode.
		optedTx("a2", 1720000002, true, "mst-example", payload),
		// Invalid tx -> still skipped.
		{TxID: txID("a3"), ChannelID: "mychannel", ChaincodeID: "assets", TimestampUnix: 1720000003, Valid: false},
		// Excluded (write-back) chaincode -> skipped, even without an event.
		{TxID: txID("a4"), ChannelID: "mychannel", ChaincodeID: "mst-anchor-status", TimestampUnix: 1720000004, Valid: true},
		// System chaincode -> skipped.
		{TxID: txID("a5"), ChannelID: "mychannel", ChaincodeID: "_lifecycle", TimestampUnix: 1720000005, Valid: true},
		// No decodable chaincode id -> skipped (nothing to attribute to).
		{TxID: txID("a6"), ChannelID: "mychannel", TimestampUnix: 1720000006, Valid: true},
	}}

	svc := New(nil, store, Config{Mode: ModeAll, ExcludeChaincodes: []string{"mst-anchor-status"}}, nil)
	if err := svc.ProcessBlock(block); err != nil {
		t.Fatal(err)
	}

	pending, err := store.ListByStatus(outbox.StatusPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("want 2 captured (a1 empty-payload, a2 declared), got %d", len(pending))
	}

	// The non-opted tx commits to the tuple with the EMPTY payload hash.
	var id1 [32]byte
	raw, _ := hex.DecodeString(txID("a1"))
	copy(id1[:], raw)
	emptyEnc, err := canonical.Encode(canonical.Payload{})
	if err != nil {
		t.Fatal(err)
	}
	wantCommitment := canonical.NewCommitment(id1, "mychannel", "assets", 4, 1720000001,
		canonical.Keccak256(emptyEnc))
	want, err := wantCommitment.Hash()
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(id1)
	if err != nil || got == nil {
		t.Fatalf("a1: %v %v", got, err)
	}
	if got.Commitment != want {
		t.Fatalf("empty-payload commitment mismatch:\n want %x\n  got %x", want, got.Commitment)
	}
	if got.ChaincodeID != "assets" {
		t.Fatalf("chaincode attribution: %q", got.ChaincodeID)
	}

	// The opted-in tx keeps its declared-payload commitment.
	var id2 [32]byte
	raw, _ = hex.DecodeString(txID("a2"))
	copy(id2[:], raw)
	declaredCommitment := canonical.NewCommitment(id2, "mychannel", "mst-example", 4, 1720000002,
		canonical.Keccak256(payload))
	wantDeclared, err := declaredCommitment.Hash()
	if err != nil {
		t.Fatal(err)
	}
	got2, err := store.Get(id2)
	if err != nil || got2 == nil {
		t.Fatalf("a2: %v %v", got2, err)
	}
	if got2.Commitment != wantDeclared {
		t.Fatal("opted-in tx must keep its declared payload in all mode")
	}
}

func TestCaptureAllModeIncludeAllowlist(t *testing.T) {
	store := openStore(t)
	block := &txmodel.Block{Number: 1, Txs: []txmodel.Tx{
		{TxID: txID("b1"), ChannelID: "ch", ChaincodeID: "assets", TimestampUnix: 1720000001, Valid: true},
		{TxID: txID("b2"), ChannelID: "ch", ChaincodeID: "other", TimestampUnix: 1720000002, Valid: true},
	}}
	svc := New(nil, store, Config{Mode: ModeAll, IncludeChaincodes: []string{"assets"}}, nil)
	if err := svc.ProcessBlock(block); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListByStatus(outbox.StatusPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ChaincodeID != "assets" {
		t.Fatalf("allowlist must admit only 'assets': %+v", pending)
	}
}

func TestCaptureOptInModeIgnoresEventlessTx(t *testing.T) {
	store := openStore(t)
	block := &txmodel.Block{Number: 1, Txs: []txmodel.Tx{
		{TxID: txID("c1"), ChannelID: "ch", ChaincodeID: "assets", TimestampUnix: 1720000001, Valid: true},
	}}
	svc := New(nil, store, Config{}, nil) // default opt-in
	if err := svc.ProcessBlock(block); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListByStatus(outbox.StatusPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatal("opt-in mode must not capture eventless transactions")
	}
}

func TestCaptureModeValidation(t *testing.T) {
	for _, tc := range []struct {
		in   Mode
		ok   bool
		want Mode
	}{
		{"", true, ModeOptIn},
		{ModeOptIn, true, ModeOptIn},
		{ModeAll, true, ModeAll},
		{"everything", false, "everything"},
	} {
		m := tc.in
		err := m.Validate()
		if tc.ok && (err != nil || m != tc.want) {
			t.Fatalf("%q: err=%v mode=%q", tc.in, err, m)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q: expected validation error", tc.in)
		}
	}
}

func TestCaptureContextCancelIsCleanShutdown(t *testing.T) {
	store := openStore(t)
	svc := New(&StaticSource{}, store, Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("cancelled run must return nil, got %v", err)
	}
}
