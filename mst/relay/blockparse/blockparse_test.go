package blockparse_test

import (
	"strings"
	"testing"

	"github.com/hansrajrami/fabric/mst/relay/blockparse"
	"github.com/hansrajrami/fabric/mst/relay/internal/blocktest"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
)

func txID(seed string) string {
	return strings.Repeat("0", 64-len(seed)) + seed
}

func TestParseExtractsEndorserTransactions(t *testing.T) {
	block := blocktest.Build(t, 7,
		blocktest.TxSpec{
			TxID: txID("a1"), ChannelID: "mychannel", Timestamp: 1720000001, Valid: true,
			ChaincodeID: "mst-example", EventName: "MSTProofRequest", EventPayload: []byte{0, 0, 0, 0},
		},
		blocktest.TxSpec{
			TxID: txID("a2"), ChannelID: "mychannel", Timestamp: 1720000002, Valid: false,
			ChaincodeID: "mst-example", EventName: "MSTProofRequest", EventPayload: []byte{0, 0, 0, 0},
		},
		blocktest.TxSpec{
			TxID: txID("a3"), ChannelID: "mychannel", Timestamp: 1720000003, Valid: true,
		},
	)

	parsed, err := blockparse.Parse(block)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Number != 7 {
		t.Fatalf("number: %d", parsed.Number)
	}
	if len(parsed.Txs) != 3 || len(parsed.Bad) != 0 {
		t.Fatalf("txs=%d bad=%d", len(parsed.Txs), len(parsed.Bad))
	}

	tx0 := parsed.Txs[0]
	if tx0.TxID != txID("a1") || tx0.ChannelID != "mychannel" || tx0.TimestampUnix != 1720000001 {
		t.Fatalf("tx0: %+v", tx0)
	}
	if !tx0.Valid {
		t.Fatal("tx0 must be valid")
	}
	if len(tx0.Events) != 1 || tx0.Events[0].EventName != "MSTProofRequest" ||
		tx0.Events[0].ChaincodeID != "mst-example" {
		t.Fatalf("tx0 events: %+v", tx0.Events)
	}

	if parsed.Txs[1].Valid {
		t.Fatal("tx1 must be invalid (MVCC conflict)")
	}
	if len(parsed.Txs[2].Events) != 0 {
		t.Fatal("tx2 must have no events")
	}
}

func TestParseSkipsNonEndorserEntries(t *testing.T) {
	block := blocktest.Build(t, 1,
		blocktest.TxSpec{
			TxID: txID("c1"), ChannelID: "mychannel", Timestamp: 1, Valid: true,
			HeaderType: int32(common.HeaderType_CONFIG),
		},
		blocktest.TxSpec{
			TxID: txID("c2"), ChannelID: "mychannel", Timestamp: 1, Valid: true,
		},
	)
	parsed, err := blockparse.Parse(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Txs) != 1 || parsed.Txs[0].TxID != txID("c2") {
		t.Fatalf("txs: %+v", parsed.Txs)
	}
}

func TestParseReportsCorruptEnvelopes(t *testing.T) {
	block := blocktest.Build(t, 2,
		blocktest.TxSpec{CorruptEnvelope: true},
		blocktest.TxSpec{TxID: txID("d1"), ChannelID: "ch", Timestamp: 1, Valid: true},
	)
	parsed, err := blockparse.Parse(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Bad) != 1 || parsed.Bad[0].Index != 0 {
		t.Fatalf("bad: %+v", parsed.Bad)
	}
	if len(parsed.Txs) != 1 {
		t.Fatalf("txs: %d", len(parsed.Txs))
	}
}

func TestParseMissingFilterMetadata(t *testing.T) {
	block := blocktest.Build(t, 3,
		blocktest.TxSpec{TxID: txID("e1"), ChannelID: "ch", Timestamp: 1, Valid: true},
	)
	block.Metadata = &common.BlockMetadata{} // strip the filter
	parsed, err := blockparse.Parse(block)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Txs[0].Valid {
		t.Fatal("missing metadata must not read as VALID")
	}
}

func TestParseNilBlock(t *testing.T) {
	if _, err := blockparse.Parse(nil); err == nil {
		t.Fatal("nil block must error")
	}
}
