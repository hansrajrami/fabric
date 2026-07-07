package verifylib

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/relay/evm"
)

type fakeReader struct {
	anchors map[[32]byte]*evm.AnchorRecord
	roots   map[[32]byte]*evm.RootRecord
}

func (f *fakeReader) GetAnchor(_ context.Context, txID [32]byte) (*evm.AnchorRecord, error) {
	return f.anchors[txID], nil
}

func (f *fakeReader) GetRoot(_ context.Context, root [32]byte) (*evm.RootRecord, error) {
	return f.roots[root], nil
}

const payloadJSON = `[
	{"name":"amount","type":"int","value":"100"},
	{"name":"active","type":"bool","value":true},
	{"name":"memo","type":"string","value":"hello"},
	{"name":"blob","type":"bytes","valueHex":"0xdeadbeef"}
]`

func input(t *testing.T) Input {
	t.Helper()
	p, err := ParsePayloadJSON([]byte(payloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	return Input{
		FabricTxID:  strings.Repeat("ab", 32),
		ChannelID:   "mychannel",
		ChaincodeID: "mst-example",
		BlockNumber: 12,
		Timestamp:   1720000000,
		Payload:     p,
	}
}

func anchorFor(t *testing.T, in Input) (txID [32]byte, commitment [32]byte) {
	t.Helper()
	c, err := Recompute(in)
	if err != nil {
		t.Fatal(err)
	}
	id, err := canonical.ParseFabricTxID(in.FabricTxID)
	if err != nil {
		t.Fatal(err)
	}
	return id, c
}

func TestVerifyMatch(t *testing.T) {
	in := input(t)
	txID, commitment := anchorFor(t, in)
	reader := &fakeReader{anchors: map[[32]byte]*evm.AnchorRecord{
		txID: {Commitment: commitment, BlockNumber: 1, EVMTimestamp: 1720001111, Exists: true},
	}}

	res, err := Verify(context.Background(), reader, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Match {
		t.Fatal("want MATCH")
	}
}

func TestVerifyTamperedFieldNoMatch(t *testing.T) {
	in := input(t)
	txID, commitment := anchorFor(t, in)
	reader := &fakeReader{anchors: map[[32]byte]*evm.AnchorRecord{
		txID: {Commitment: commitment, Exists: true},
	}}

	// Tamper each dimension: a payload value, the block, the timestamp,
	// the channel — every one must flip the verdict.
	tampered := input(t)
	f, err := canonical.StringField("memo", "hell0")
	if err != nil {
		t.Fatal(err)
	}
	for i := range tampered.Payload.Fields {
		if tampered.Payload.Fields[i].Name == "memo" {
			tampered.Payload.Fields[i] = f
		}
	}
	cases := map[string]Input{
		"payload value": tampered,
	}
	blockIn := input(t)
	blockIn.BlockNumber++
	cases["block number"] = blockIn
	tsIn := input(t)
	tsIn.Timestamp++
	cases["timestamp"] = tsIn
	chIn := input(t)
	chIn.ChannelID = "otherchannel"
	cases["channel"] = chIn

	for desc, in := range cases {
		res, err := Verify(context.Background(), reader, in)
		if err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
		if res.Match {
			t.Errorf("%s: tampering must produce NO-MATCH", desc)
		}
	}
}

func TestVerifyNotAnchored(t *testing.T) {
	in := input(t)
	res, err := Verify(context.Background(), &fakeReader{anchors: map[[32]byte]*evm.AnchorRecord{}}, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Match || res.OnChain != nil {
		t.Fatal("absent anchor must be NO-MATCH with nil OnChain")
	}
}

func TestRecomputeFromCanonicalHexAgreesWithFields(t *testing.T) {
	in := input(t)
	fromFields, err := Recompute(in)
	if err != nil {
		t.Fatal(err)
	}

	enc, err := canonical.Encode(*in.Payload)
	if err != nil {
		t.Fatal(err)
	}
	hexIn := in
	hexIn.Payload = nil
	hexIn.PayloadCanonicalHex = "0x" + hex.EncodeToString(enc)
	fromHex, err := Recompute(hexIn)
	if err != nil {
		t.Fatal(err)
	}
	if fromFields != fromHex {
		t.Fatal("field path and canonical-hex path disagree")
	}
}

func TestRecomputeRejections(t *testing.T) {
	in := input(t)
	in.Payload = nil
	if _, err := Recompute(in); err == nil {
		t.Fatal("no payload must error")
	}

	in = input(t)
	in.PayloadCanonicalHex = "0x00000000" // both set
	if _, err := Recompute(in); err == nil {
		t.Fatal("both payload forms must error")
	}

	in = input(t)
	in.Payload = nil
	in.PayloadCanonicalHex = "0xffff" // not canonical
	if _, err := Recompute(in); err == nil {
		t.Fatal("non-canonical hex must error")
	}

	in = input(t)
	in.FabricTxID = "short"
	if _, err := Recompute(in); err == nil {
		t.Fatal("bad tx id must error")
	}
}

func TestParsePayloadJSONRejectsUnknownTypes(t *testing.T) {
	if _, err := ParsePayloadJSON([]byte(`[{"name":"f","type":"float","value":"1.5"}]`)); err == nil {
		t.Fatal("float type must be rejected")
	}
	if _, err := ParsePayloadJSON([]byte(`[{"name":"o","type":"object"}]`)); err == nil {
		t.Fatal("object type must be rejected")
	}
	if _, err := ParsePayloadJSON([]byte(`not json`)); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestVerifyInBatch(t *testing.T) {
	in := input(t)
	leaf, err := Recompute(in)
	if err != nil {
		t.Fatal(err)
	}
	// Batch of three: our leaf plus two synthetic siblings.
	leaves := [][32]byte{leaf, canonical.Keccak256([]byte{1}), canonical.Keccak256([]byte{2})}
	root, proof, err := DeriveProof(leaves, leaf)
	if err != nil {
		t.Fatal(err)
	}

	reader := &fakeReader{roots: map[[32]byte]*evm.RootRecord{
		root: {LeafCount: 3, EVMTimestamp: 1720003333, Exists: true},
	}}

	res, err := VerifyInBatch(context.Background(), reader, in, root, proof)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ProofValid || !res.Match {
		t.Fatalf("want MATCH: %+v", res)
	}

	// Tampered data: proof no longer links to the root.
	tampered := input(t)
	tampered.BlockNumber++
	res, err = VerifyInBatch(context.Background(), reader, tampered, root, proof)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProofValid || res.Match {
		t.Fatal("tampered data must fail the inclusion proof")
	}

	// Root not anchored: proof valid but no MATCH.
	res, err = VerifyInBatch(context.Background(), &fakeReader{roots: map[[32]byte]*evm.RootRecord{}}, in, root, proof)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ProofValid || res.Match || res.OnChain != nil {
		t.Fatalf("unanchored root must be NO-MATCH with valid proof: %+v", res)
	}
}
