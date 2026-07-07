package fabricwb

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeContract struct {
	calls [][]string
	fail  error
}

func (f *fakeContract) Submit(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil, f.fail
}

func ids() (fabricTxID, evmTxHash [32]byte) {
	for i := range fabricTxID {
		fabricTxID[i] = 0xAB
		evmTxHash[i] = 0xCD
	}
	return
}

func TestRecordAnchorSubmitsExpectedArgs(t *testing.T) {
	contract := &fakeContract{}
	w := NewWriter(contract, nil)
	txID, evmHash := ids()

	if err := w.RecordAnchor(context.Background(), txID, evmHash); err != nil {
		t.Fatal(err)
	}
	if len(contract.calls) != 1 {
		t.Fatalf("calls: %d", len(contract.calls))
	}
	call := contract.calls[0]
	if call[0] != "RecordAnchor" {
		t.Fatalf("fn: %s", call[0])
	}
	if call[1] != strings.Repeat("ab", 32) {
		t.Fatalf("txID arg: %s", call[1])
	}
	if call[2] != "0x"+strings.Repeat("cd", 32) {
		t.Fatalf("anchorRef arg: %s", call[2])
	}
	if call[3] != StatusConfirmed {
		t.Fatalf("status arg: %s", call[3])
	}
}

func TestRecordAnchorPropagatesFailure(t *testing.T) {
	contract := &fakeContract{fail: errors.New("endorsement failed")}
	w := NewWriter(contract, nil)
	txID, evmHash := ids()

	err := w.RecordAnchor(context.Background(), txID, evmHash)
	if err == nil || !strings.Contains(err.Error(), "endorsement failed") {
		t.Fatalf("want propagated failure, got %v", err)
	}
}
