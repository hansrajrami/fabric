package evm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hansrajrami/fabric/mst/canonical"
)

// abiArtifact mirrors the checked-in Hardhat ABI export.
type abiArtifact struct {
	ContractName string `json:"contractName"`
	ABI          []struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Inputs  []abiParam `json:"inputs"`
		Outputs []abiParam `json:"outputs"`
	} `json:"abi"`
}

type abiParam struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TestSelectorsMatchCompiledABI derives every function selector from the
// compiled contract ABI and asserts our hand-pinned selectors agree — the
// guard against signature drift between MSTAnchor.sol and this package.
func TestSelectorsMatchCompiledABI(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash("../../anchor-contracts/abi/MSTAnchor.json"))
	if err != nil {
		t.Fatalf("read ABI artifact: %v", err)
	}
	var artifact abiArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}

	abiSelectors := map[string][4]byte{}
	for _, entry := range artifact.ABI {
		if entry.Type != "function" {
			continue
		}
		types := make([]string, len(entry.Inputs))
		for i, in := range entry.Inputs {
			types[i] = in.Type
		}
		sig := fmt.Sprintf("%s(%s)", entry.Name, strings.Join(types, ","))
		h := canonical.Keccak256([]byte(sig))
		var sel [4]byte
		copy(sel[:], h[:4])
		abiSelectors[entry.Name] = sel
	}

	for name, want := range map[string][4]byte{
		"anchor":      selAnchor,
		"anchorBatch": selAnchorBatch,
		"getAnchor":   selGetAnchor,
	} {
		got, ok := abiSelectors[name]
		if !ok {
			t.Fatalf("function %q missing from compiled ABI", name)
		}
		if got != want {
			t.Fatalf("selector drift for %q: abi %x, packed %x", name, got, want)
		}
	}
}

func TestPackAnchorLayout(t *testing.T) {
	var id, commitment [32]byte
	for i := range id {
		id[i] = byte(i)
		commitment[i] = byte(0xA0 | i&0x0F)
	}
	data := packAnchor(id, commitment, 0xDEADBEEF)
	if len(data) != 4+96 {
		t.Fatalf("length: %d", len(data))
	}
	if [4]byte(data[:4]) != selAnchor {
		t.Fatal("selector wrong")
	}
	if !equal(data[4:36], id[:]) || !equal(data[36:68], commitment[:]) {
		t.Fatal("static args wrong")
	}
	if binary.BigEndian.Uint64(data[92:100]) != 0xDEADBEEF {
		t.Fatal("block number wrong")
	}
	for _, b := range data[68:92] {
		if b != 0 {
			t.Fatal("uint64 padding not zero")
		}
	}
}

func TestPackAnchorBatchLayout(t *testing.T) {
	ids := [][32]byte{{1}, {2}}
	commitments := [][32]byte{{0xAA}, {0xBB}}
	blocks := []uint64{7, 8}
	data, err := packAnchorBatch(ids, commitments, blocks)
	if err != nil {
		t.Fatal(err)
	}
	body := data[4:]
	// Head: offsets 0x60, 0xC0, 0x120 for three 2-element arrays.
	if got := binary.BigEndian.Uint64(body[24:32]); got != 0x60 {
		t.Fatalf("offset1: %#x", got)
	}
	if got := binary.BigEndian.Uint64(body[56:64]); got != 0x60+0x60 {
		t.Fatalf("offset2: %#x", got)
	}
	if got := binary.BigEndian.Uint64(body[88:96]); got != 0x60+0xC0 {
		t.Fatalf("offset3: %#x", got)
	}
	// Array 1: length + elements.
	if got := binary.BigEndian.Uint64(body[0x60+24 : 0x60+32]); got != 2 {
		t.Fatalf("len1: %d", got)
	}
	if body[0x60+32] != 1 || body[0x60+64] != 2 {
		t.Fatal("ids wrong")
	}
	if body[0xC0+32] != 0xAA || body[0xC0+64] != 0xBB {
		t.Fatal("commitments wrong")
	}
	if got := binary.BigEndian.Uint64(body[0x120+56 : 0x120+64]); got != 7 {
		t.Fatalf("block[0]: %d", got)
	}

	if _, err := packAnchorBatch(ids, commitments[:1], blocks); err == nil {
		t.Fatal("length mismatch must error")
	}
}

func TestUnpackGetAnchor(t *testing.T) {
	ret := make([]byte, 128)
	for i := 0; i < 32; i++ {
		ret[i] = byte(i)
	}
	binary.BigEndian.PutUint64(ret[56:64], 42)      // blockNumber
	binary.BigEndian.PutUint64(ret[88:96], 1720000) // evmTimestamp
	ret[127] = 1                                    // exists

	rec, err := unpackGetAnchor(ret)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BlockNumber != 42 || rec.EVMTimestamp != 1720000 || !rec.Exists {
		t.Fatalf("record: %+v", rec)
	}
	if rec.Commitment[5] != 5 {
		t.Fatal("commitment bytes wrong")
	}

	if _, err := unpackGetAnchor(ret[:100]); err == nil {
		t.Fatal("short return must error")
	}
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
