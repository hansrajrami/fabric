package canonical

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestDomainTagPinned(t *testing.T) {
	if got := ComputeDomainTag(); got != DomainTag {
		t.Fatalf("hard-coded DomainTag stale:\n pinned %x\n actual %x", DomainTag, got)
	}
}

func testCommitment() Commitment {
	var txID [32]byte
	for i := range txID {
		txID[i] = byte(i)
	}
	var payloadHash [32]byte
	for i := range payloadHash {
		payloadHash[i] = byte(0xF0 | i&0x0F)
	}
	return NewCommitment(txID, "mychannel", "asset-transfer", 12345, 1720000000, payloadHash)
}

func TestABIEncodeLayout(t *testing.T) {
	c := testCommitment()
	enc, err := c.ABIEncode()
	if err != nil {
		t.Fatal(err)
	}

	// 8 head slots + (len slot + 32) for "mychannel" (9 bytes) +
	// (len slot + 32) for "asset-transfer" (14 bytes).
	wantLen := 8*32 + 32 + 32 + 32 + 32
	if len(enc) != wantLen {
		t.Fatalf("encoding length: want %d, got %d", wantLen, len(enc))
	}

	slot := func(i int) []byte { return enc[i*32 : (i+1)*32] }
	if binary.BigEndian.Uint64(slot(0)[24:]) != uint64(SchemaVersion) {
		t.Fatal("slot 0: schema version wrong")
	}
	if !bytes.Equal(slot(1), c.DomainTag[:]) {
		t.Fatal("slot 1: domain tag wrong")
	}
	if !bytes.Equal(slot(2), c.FabricTxID[:]) {
		t.Fatal("slot 2: fabric tx id wrong")
	}
	if off := binary.BigEndian.Uint64(slot(3)[24:]); off != 256 {
		t.Fatalf("slot 3: channel offset want 256, got %d", off)
	}
	if off := binary.BigEndian.Uint64(slot(4)[24:]); off != 256+32+32 {
		t.Fatalf("slot 4: chaincode offset want %d, got %d", 256+32+32, off)
	}
	if binary.BigEndian.Uint64(slot(5)[24:]) != c.BlockNumber {
		t.Fatal("slot 5: block number wrong")
	}
	if binary.BigEndian.Uint64(slot(6)[24:]) != c.Timestamp {
		t.Fatal("slot 6: timestamp wrong")
	}
	if !bytes.Equal(slot(7), c.PayloadHash[:]) {
		t.Fatal("slot 7: payload hash wrong")
	}

	// Tail: channel string.
	if l := binary.BigEndian.Uint64(slot(8)[24:]); l != uint64(len("mychannel")) {
		t.Fatalf("channel length slot: want %d, got %d", len("mychannel"), l)
	}
	if got := string(bytes.TrimRight(slot(9), "\x00")); got != "mychannel" {
		t.Fatalf("channel data: %q", got)
	}
	if l := binary.BigEndian.Uint64(slot(10)[24:]); l != uint64(len("asset-transfer")) {
		t.Fatalf("chaincode length slot: want %d, got %d", len("asset-transfer"), l)
	}
	if got := string(bytes.TrimRight(slot(11), "\x00")); got != "asset-transfer" {
		t.Fatalf("chaincode data: %q", got)
	}
}

func TestABIEncodeEmptyStrings(t *testing.T) {
	c := testCommitment()
	c.ChannelID = ""
	c.ChaincodeID = ""
	enc, err := c.ABIEncode()
	if err != nil {
		t.Fatal(err)
	}
	// Empty strings still occupy a zero length slot each, no data words.
	if len(enc) != 8*32+32+32 {
		t.Fatalf("empty-string encoding length: got %d", len(enc))
	}
	if off := binary.BigEndian.Uint64(enc[4*32+24 : 5*32]); off != 256+32 {
		t.Fatalf("second offset with empty first string: want %d, got %d", 256+32, off)
	}
}

func TestABIEncodeStringPadding(t *testing.T) {
	// A 32-byte-aligned string must not gain an extra padding word.
	c := testCommitment()
	c.ChannelID = strings.Repeat("a", 32)
	enc, err := c.ABIEncode()
	if err != nil {
		t.Fatal(err)
	}
	wantChaincodeOffset := uint64(256 + 32 + 32) // len slot + exactly one data word
	if off := binary.BigEndian.Uint64(enc[4*32+24 : 5*32]); off != wantChaincodeOffset {
		t.Fatalf("aligned-string offset: want %d, got %d", wantChaincodeOffset, off)
	}
}

func TestCommitmentHashDeterministic(t *testing.T) {
	c := testCommitment()
	h1, err := BuildCommitment(c)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := c.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("hash not deterministic")
	}

	// Every field must influence the hash.
	mutations := []func(*Commitment){
		func(m *Commitment) { m.SchemaVersion++ },
		func(m *Commitment) { m.DomainTag[0] ^= 1 },
		func(m *Commitment) { m.FabricTxID[0] ^= 1 },
		func(m *Commitment) { m.ChannelID += "x" },
		func(m *Commitment) { m.ChaincodeID += "x" },
		func(m *Commitment) { m.BlockNumber++ },
		func(m *Commitment) { m.Timestamp++ },
		func(m *Commitment) { m.PayloadHash[0] ^= 1 },
	}
	for i, mutate := range mutations {
		m := testCommitment()
		mutate(&m)
		h, err := m.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if h == h1 {
			t.Fatalf("mutation %d did not change the commitment", i)
		}
	}
}

func TestABIEncodeRejections(t *testing.T) {
	c := testCommitment()
	c.ChannelID = string([]byte{0xff})
	if _, err := c.ABIEncode(); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid UTF-8 channel: want ErrInvalidUTF8, got %v", err)
	}
	c = testCommitment()
	c.ChaincodeID = strings.Repeat("a", maxIDLen+1)
	if _, err := c.ABIEncode(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized chaincode id: want ErrTooLarge, got %v", err)
	}
}

func TestParseFabricTxID(t *testing.T) {
	id, err := ParseFabricTxID(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	if id[0] != 0xab || id[31] != 0xab {
		t.Fatalf("parsed wrong: %x", id)
	}
	if _, err := ParseFabricTxID("abc"); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("short id: want ErrInvalidValue, got %v", err)
	}
	if _, err := ParseFabricTxID(strings.Repeat("zz", 32)); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("non-hex id: want ErrInvalidValue, got %v", err)
	}
}
