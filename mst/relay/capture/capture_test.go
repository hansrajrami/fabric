package capture

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/relay/internal/blocktest"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
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

// run drains a finite StaticSource through the service; the "stream ended
// unexpectedly" error is the expected terminal state for a finite source.
func run(t *testing.T, svc *Service, blocks ...*common.Block) {
	t.Helper()
	src := &StaticSource{BlocksList: blocks}
	svc.source = src
	err := svc.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stream ended") {
		t.Fatalf("finite source must end with stream-ended, got %v", err)
	}
}

func TestCaptureEndToEnd(t *testing.T) {
	store := openStore(t)
	payload := validPayload(t)

	blocks := []*common.Block{
		blocktest.Build(t, 0,
			// opted-in, valid -> captured
			blocktest.TxSpec{TxID: txID("a1"), ChannelID: "mychannel", Timestamp: 1720000001, Valid: true,
				ChaincodeID: "mst-example", EventName: "MSTProofRequest", EventPayload: payload},
			// opted-in but invalid tx -> skipped
			blocktest.TxSpec{TxID: txID("a2"), ChannelID: "mychannel", Timestamp: 1720000002, Valid: false,
				ChaincodeID: "mst-example", EventName: "MSTProofRequest", EventPayload: payload},
		),
		blocktest.Build(t, 1,
			// not opted in -> skipped
			blocktest.TxSpec{TxID: txID("b1"), ChannelID: "mychannel", Timestamp: 1720000003, Valid: true,
				ChaincodeID: "mst-example", EventName: "SomethingElse", EventPayload: payload},
			// excluded chaincode (echo-loop guard) -> skipped
			blocktest.TxSpec{TxID: txID("b2"), ChannelID: "mychannel", Timestamp: 1720000004, Valid: true,
				ChaincodeID: "mst-anchor-status", EventName: "MSTProofRequest", EventPayload: payload},
		),
		blocktest.Build(t, 2,
			// malformed payload -> quarantined, stream continues
			blocktest.TxSpec{TxID: txID("c1"), ChannelID: "mychannel", Timestamp: 1720000005, Valid: true,
				ChaincodeID: "mst-example", EventName: "MSTProofRequest", EventPayload: []byte{0xFF, 0xFF}},
			// opted-in, valid -> captured
			blocktest.TxSpec{TxID: txID("c2"), ChannelID: "mychannel", Timestamp: 1720000006, Valid: true,
				ChaincodeID: "mst-example", EventName: "MSTProofRequest", EventPayload: payload},
		),
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

	mkBlock := func(n uint64, seed string) *common.Block {
		return blocktest.Build(t, n,
			blocktest.TxSpec{TxID: txID(seed), ChannelID: "ch", Timestamp: 1720000000 + int64(n), Valid: true,
				ChaincodeID: "mst-example", EventName: "MSTProofRequest", EventPayload: payload},
		)
	}
	all := []*common.Block{mkBlock(0, "f0"), mkBlock(1, "f1"), mkBlock(2, "f2"), mkBlock(3, "f3"), mkBlock(4, "f4")}

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
	payload := validPayload(t)
	block := blocktest.Build(t, 9,
		blocktest.TxSpec{TxID: txID("aa"), ChannelID: "ch", Timestamp: 1720000009, Valid: true,
			ChaincodeID: "cc", EventName: "MSTProofRequest", EventPayload: payload},
	)

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

func TestCaptureContextCancelIsCleanShutdown(t *testing.T) {
	store := openStore(t)
	svc := New(&StaticSource{}, store, Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("cancelled run must return nil, got %v", err)
	}
}
