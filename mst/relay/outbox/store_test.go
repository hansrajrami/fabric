package outbox

import (
	"fmt"
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

// TestLevelDBStoreContract runs the full backend-agnostic contract suite.
func TestLevelDBStoreContract(t *testing.T) {
	runStoreSuite(t, func(t *testing.T) Store {
		s, _ := openTemp(t)
		return s
	})
}

// TestCrashRecoveryReopen is LevelDB-specific: unclean shutdown + reopen on
// the same directory (the sync'd WAL is what recovery relies on).
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
