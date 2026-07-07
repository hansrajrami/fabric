package main

import (
	"strings"
	"testing"

	"github.com/hyperledger/fabric-chaincode-go/v2/pkg/cid"
	"github.com/hyperledger/fabric-chaincode-go/v2/shim"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type mockStub struct {
	shim.ChaincodeStubInterface
	state      map[string][]byte
	eventCalls int
}

func (m *mockStub) GetState(key string) ([]byte, error) { return m.state[key], nil }
func (m *mockStub) PutState(key string, value []byte) error {
	m.state[key] = value
	return nil
}
func (m *mockStub) SetEvent(string, []byte) error {
	m.eventCalls++
	return nil
}
func (m *mockStub) GetTxTimestamp() (*timestamppb.Timestamp, error) {
	return &timestamppb.Timestamp{Seconds: 1720009999}, nil
}

type mockIdentity struct {
	cid.ClientIdentity
	mspID string
}

func (m *mockIdentity) GetMSPID() (string, error) { return m.mspID, nil }

type mockCtx struct {
	contractapi.TransactionContextInterface
	stub *mockStub
	id   *mockIdentity
}

func (m *mockCtx) GetStub() shim.ChaincodeStubInterface  { return m.stub }
func (m *mockCtx) GetClientIdentity() cid.ClientIdentity { return m.id }

func newCtx(msp string) (*mockCtx, *mockStub) {
	stub := &mockStub{state: map[string][]byte{}}
	return &mockCtx{stub: stub, id: &mockIdentity{mspID: msp}}, stub
}

func contract(t *testing.T, allowedMSP string) *SmartContract {
	t.Helper()
	t.Setenv(EnvRelayerMSPID, allowedMSP)
	return &SmartContract{}
}

var (
	goodTxID = strings.Repeat("ab", 32)
	goodRef  = "0x" + strings.Repeat("cd", 32)
)

func TestRecordAndQuery(t *testing.T) {
	sc := contract(t, "RelayerMSP")
	ctx, stub := newCtx("RelayerMSP")

	if err := sc.RecordAnchor(ctx, goodTxID, goodRef, StatusConfirmed); err != nil {
		t.Fatalf("RecordAnchor: %v", err)
	}
	rec, err := sc.QueryAnchorStatus(ctx, goodTxID)
	if err != nil {
		t.Fatalf("QueryAnchorStatus: %v", err)
	}
	if rec.AnchorRef != goodRef || rec.Status != StatusConfirmed || rec.RecordedAt != 1720009999 {
		t.Fatalf("record: %+v", rec)
	}
	ok, err := sc.IsAnchored(ctx, goodTxID)
	if err != nil || !ok {
		t.Fatalf("IsAnchored: %v %v", ok, err)
	}
	if stub.eventCalls != 0 {
		t.Fatal("anchor-status must NEVER emit events (echo-loop guard)")
	}
}

func TestRecordIsIdempotent(t *testing.T) {
	sc := contract(t, "")
	ctx, _ := newCtx("AnyMSP")

	if err := sc.RecordAnchor(ctx, goodTxID, goodRef, StatusConfirmed); err != nil {
		t.Fatal(err)
	}
	otherRef := "0x" + strings.Repeat("99", 32)
	// Re-record with a different ref: quiet success, no overwrite.
	if err := sc.RecordAnchor(ctx, goodTxID, otherRef, StatusConfirmed); err != nil {
		t.Fatalf("idempotent re-record must not error: %v", err)
	}
	rec, err := sc.QueryAnchorStatus(ctx, goodTxID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.AnchorRef != goodRef {
		t.Fatal("re-record overwrote the original pointer")
	}
}

func TestUnauthorizedMSPRejected(t *testing.T) {
	sc := contract(t, "RelayerMSP")
	ctx, stub := newCtx("IntruderMSP")

	if err := sc.RecordAnchor(ctx, goodTxID, goodRef, StatusConfirmed); err == nil {
		t.Fatal("write from wrong MSP must fail")
	}
	if len(stub.state) != 0 {
		t.Fatal("unauthorized write must not touch state")
	}

	// Reads remain open to everyone.
	if _, err := sc.QueryAnchorStatus(ctx, goodTxID); err == nil {
		t.Fatal("expected not-anchored error")
	} else if !strings.Contains(err.Error(), "not anchored") {
		t.Fatalf("query must be permitted (got %v)", err)
	}
}

func TestInputValidation(t *testing.T) {
	sc := contract(t, "")
	ctx, _ := newCtx("AnyMSP")

	cases := []struct {
		desc                 string
		txID, ref, status    string
	}{
		{"short tx id", "abcd", goodRef, StatusConfirmed},
		{"non-hex tx id", strings.Repeat("zz", 32), goodRef, StatusConfirmed},
		{"ref without 0x", strings.Repeat("cd", 33), goodRef[:64], StatusConfirmed},
		{"short ref", goodTxID, "0xabcd", StatusConfirmed},
		{"non-hex ref", goodTxID, "0x" + strings.Repeat("zz", 32), StatusConfirmed},
		{"unsupported status", goodTxID, goodRef, "MAYBE"},
	}
	for _, tc := range cases {
		if err := sc.RecordAnchor(ctx, tc.txID, tc.ref, tc.status); err == nil {
			t.Errorf("%s: expected error", tc.desc)
		}
	}

	if _, err := sc.QueryAnchorStatus(ctx, "nope"); err == nil {
		t.Error("query with bad id must fail")
	}
}

func TestTxIDNormalizedToLowercase(t *testing.T) {
	sc := contract(t, "")
	ctx, _ := newCtx("AnyMSP")
	upper := strings.ToUpper(goodTxID)
	if err := sc.RecordAnchor(ctx, upper, goodRef, StatusConfirmed); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.QueryAnchorStatus(ctx, goodTxID); err != nil {
		t.Fatalf("case-insensitive lookup: %v", err)
	}
}
