package main

import (
	"testing"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/fabric-chaincode/proofhelper"
	"github.com/hyperledger/fabric-chaincode-go/v2/shim"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// mockStub implements just the stub surface the contract uses; the embedded
// nil interface panics loudly if the contract starts using anything new.
type mockStub struct {
	shim.ChaincodeStubInterface
	state        map[string][]byte
	eventName    string
	eventPayload []byte
	eventCalls   int
}

func newMockStub() *mockStub {
	return &mockStub{state: map[string][]byte{}}
}

func (m *mockStub) GetState(key string) ([]byte, error) { return m.state[key], nil }

func (m *mockStub) PutState(key string, value []byte) error {
	m.state[key] = value
	return nil
}

func (m *mockStub) SetEvent(name string, payload []byte) error {
	m.eventCalls++
	m.eventName = name
	m.eventPayload = payload
	return nil
}

func testCtx(stub *mockStub) *contractapi.TransactionContext {
	ctx := &contractapi.TransactionContext{}
	ctx.SetStub(stub)
	return ctx
}

func decodeEvent(t *testing.T, stub *mockStub) map[string]canonical.Field {
	t.Helper()
	if stub.eventName != proofhelper.EventName {
		t.Fatalf("event name: want %q, got %q", proofhelper.EventName, stub.eventName)
	}
	p, err := canonical.Decode(stub.eventPayload)
	if err != nil {
		t.Fatalf("event payload not canonical: %v", err)
	}
	fields := map[string]canonical.Field{}
	for _, f := range p.Fields {
		fields[f.Name] = f
	}
	return fields
}

func TestCreateAssetEmitsProofRequest(t *testing.T) {
	stub := newMockStub()
	sc := &SmartContract{}

	if err := sc.CreateAsset(testCtx(stub), "asset-1", "alice", 100); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	if stub.state["asset-1"] == nil {
		t.Fatal("asset not stored")
	}
	if stub.eventCalls != 1 {
		t.Fatalf("want exactly 1 event, got %d", stub.eventCalls)
	}

	fields := decodeEvent(t, stub)
	if got := string(fields["action"].Value); got != "create" {
		t.Fatalf("action: %q", got)
	}
	if got := string(fields["asset_id"].Value); got != "asset-1" {
		t.Fatalf("asset_id: %q", got)
	}
	if got := string(fields["owner"].Value); got != "alice" {
		t.Fatalf("owner: %q", got)
	}
	if fields["value"].Value[31] != 100 {
		t.Fatalf("value: %x", fields["value"].Value)
	}
	if fields["active"].Value[0] != 0x01 {
		t.Fatal("active: want true")
	}
}

func TestCreateAssetRejectsDuplicateAndNegative(t *testing.T) {
	stub := newMockStub()
	sc := &SmartContract{}
	if err := sc.CreateAsset(testCtx(stub), "a", "alice", 1); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateAsset(testCtx(stub), "a", "bob", 2); err == nil {
		t.Fatal("duplicate create must fail")
	}
	if err := sc.CreateAsset(testCtx(stub), "b", "bob", -1); err == nil {
		t.Fatal("negative value must fail")
	}
}

func TestTransferAssetEmitsProofRequest(t *testing.T) {
	stub := newMockStub()
	sc := &SmartContract{}
	if err := sc.CreateAsset(testCtx(stub), "asset-1", "alice", 100); err != nil {
		t.Fatal(err)
	}
	if err := sc.TransferAsset(testCtx(stub), "asset-1", "bob"); err != nil {
		t.Fatalf("TransferAsset: %v", err)
	}

	fields := decodeEvent(t, stub)
	if got := string(fields["action"].Value); got != "transfer" {
		t.Fatalf("action: %q", got)
	}
	if got := string(fields["previous_owner"].Value); got != "alice" {
		t.Fatalf("previous_owner: %q", got)
	}
	if got := string(fields["new_owner"].Value); got != "bob" {
		t.Fatalf("new_owner: %q", got)
	}

	asset, err := sc.ReadAsset(testCtx(stub), "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if asset.Owner != "bob" {
		t.Fatalf("owner after transfer: %q", asset.Owner)
	}
}

func TestTransferMissingAssetFails(t *testing.T) {
	stub := newMockStub()
	sc := &SmartContract{}
	if err := sc.TransferAsset(testCtx(stub), "nope", "bob"); err == nil {
		t.Fatal("transfer of missing asset must fail")
	}
	if stub.eventCalls != 0 {
		t.Fatal("no event may be emitted on failure")
	}
}

func TestReadAssetDoesNotEmit(t *testing.T) {
	stub := newMockStub()
	sc := &SmartContract{}
	if err := sc.CreateAsset(testCtx(stub), "a", "alice", 5); err != nil {
		t.Fatal(err)
	}
	calls := stub.eventCalls
	if _, err := sc.ReadAsset(testCtx(stub), "a"); err != nil {
		t.Fatal(err)
	}
	if stub.eventCalls != calls {
		t.Fatal("queries must not opt in to proofs")
	}
}
