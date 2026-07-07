package proofhelper

import (
	"bytes"
	"errors"
	"math/big"
	"testing"

	"github.com/hansrajrami/fabric/mst/canonical"
)

type fakeStub struct {
	name    string
	payload []byte
	calls   int
	fail    error
}

func (f *fakeStub) SetEvent(name string, payload []byte) error {
	f.calls++
	if f.fail != nil {
		return f.fail
	}
	f.name = name
	f.payload = payload
	return nil
}

func TestEmitSetsCanonicalEventPayload(t *testing.T) {
	stub := &fakeStub{}
	err := New().
		AddString("asset_id", "asset-1").
		AddInt64("value", 100).
		AddBool("active", true).
		AddBytes("digest", []byte{0xde, 0xad}).
		Emit(stub)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if stub.name != EventName {
		t.Fatalf("event name: want %q, got %q", EventName, stub.name)
	}

	// The payload must be the canonical encoding: decodable, and re-encoding
	// the decoded payload reproduces it byte-for-byte.
	p, err := canonical.Decode(stub.payload)
	if err != nil {
		t.Fatalf("event payload is not canonical: %v", err)
	}
	re, err := canonical.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(re, stub.payload) {
		t.Fatal("event payload does not round-trip canonically")
	}
	if len(p.Fields) != 4 {
		t.Fatalf("want 4 fields, got %d", len(p.Fields))
	}
}

func TestBuilderErrorSticksAndBlocksEmit(t *testing.T) {
	stub := &fakeStub{}
	b := New().
		AddString("s", "e\u0301"). // decomposed e + combining acute (NFD): invalid, sticks
		AddInt64("v", 5)           // no-op after the error
	if b.Err() == nil {
		t.Fatal("expected sticky error")
	}
	if err := b.Emit(stub); err == nil {
		t.Fatal("Emit must fail after a build error")
	}
	if stub.calls != 0 {
		t.Fatal("SetEvent must not be called for an invalid payload")
	}
	if _, err := b.Encode(); err == nil {
		t.Fatal("Encode must fail after a build error")
	}
	if _, err := b.Payload(); err == nil {
		t.Fatal("Payload must fail after a build error")
	}
}

func TestDuplicateFieldRejectedAtEmit(t *testing.T) {
	stub := &fakeStub{}
	err := New().
		AddInt64("a", 1).
		AddInt64("a", 2).
		Emit(stub)
	if !errors.Is(err, canonical.ErrDuplicateField) {
		t.Fatalf("want ErrDuplicateField, got %v", err)
	}
	if stub.calls != 0 {
		t.Fatal("SetEvent must not be called for duplicate fields")
	}
}

func TestNegativeIntRejected(t *testing.T) {
	if err := New().AddInt64("v", -1).Err(); err == nil {
		t.Fatal("AddInt64(-1) must error")
	}
	if err := New().AddInt("v", big.NewInt(-5)).Err(); !errors.Is(err, canonical.ErrIntRange) {
		t.Fatalf("AddInt(-5): want ErrIntRange, got %v", err)
	}
}

func TestSetEventFailurePropagates(t *testing.T) {
	stub := &fakeStub{fail: errors.New("boom")}
	err := New().AddBool("x", true).Emit(stub)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("boom")) {
		t.Fatalf("SetEvent failure must propagate, got %v", err)
	}
}

func TestEmptyPayloadIsValid(t *testing.T) {
	// An empty declaration is legal: it commits to the transaction tuple
	// with the well-known empty payload hash.
	stub := &fakeStub{}
	if err := New().Emit(stub); err != nil {
		t.Fatalf("empty payload Emit: %v", err)
	}
	if !bytes.Equal(stub.payload, []byte{0, 0, 0, 0}) {
		t.Fatalf("empty payload encoding: %x", stub.payload)
	}
}
