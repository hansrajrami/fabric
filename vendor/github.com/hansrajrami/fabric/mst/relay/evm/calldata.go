// Package evm submits anchors to the MSTAnchor contract and reads them back.
// The contract surface is three fixed-shape functions, so calldata is packed
// directly (no abigen, no reflection); the packing is unit-tested against the
// compiled Hardhat ABI artifact to rule out drift.
package evm

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/hansrajrami/fabric/mst/canonical"
)

// Function selectors: first 4 bytes of keccak256 of the canonical signature.
var (
	selAnchor      = selector("anchor(bytes32,bytes32,uint64)")
	selAnchorBatch = selector("anchorBatch(bytes32[],bytes32[],uint64[])")
	selAnchorRoot  = selector("anchorRoot(bytes32,uint64)")
	selGetAnchor   = selector("getAnchor(bytes32)")
	selGetRoot     = selector("getRoot(bytes32)")
	selSetRelayer  = selector("setRelayer(address,bool)")
)

func selector(signature string) [4]byte {
	h := canonical.Keccak256([]byte(signature))
	var s [4]byte
	copy(s[:], h[:4])
	return s
}

// packAnchor builds calldata for anchor(fabricTxId, commitment, blockNumber).
func packAnchor(fabricTxID, commitment [32]byte, blockNumber uint64) []byte {
	out := make([]byte, 4+3*32)
	copy(out[0:4], selAnchor[:])
	copy(out[4:36], fabricTxID[:])
	copy(out[36:68], commitment[:])
	binary.BigEndian.PutUint64(out[92:100], blockNumber)
	return out
}

// packAnchorRoot builds calldata for anchorRoot(root, leafCount).
func packAnchorRoot(root [32]byte, leafCount uint64) []byte {
	out := make([]byte, 4+2*32)
	copy(out[0:4], selAnchorRoot[:])
	copy(out[4:36], root[:])
	binary.BigEndian.PutUint64(out[60:68], leafCount)
	return out
}

// packGetRoot builds calldata for getRoot(root).
func packGetRoot(root [32]byte) []byte {
	out := make([]byte, 4+32)
	copy(out[0:4], selGetRoot[:])
	copy(out[4:36], root[:])
	return out
}

// packGetAnchor builds calldata for getAnchor(fabricTxId).
func packGetAnchor(fabricTxID [32]byte) []byte {
	out := make([]byte, 4+32)
	copy(out[0:4], selGetAnchor[:])
	copy(out[4:36], fabricTxID[:])
	return out
}

// packSetRelayer builds calldata for setRelayer(address relayer, bool allowed).
// The address occupies the low 20 bytes of the first ABI word; the bool is the
// last byte of the second word.
func packSetRelayer(addr common.Address, allowed bool) []byte {
	out := make([]byte, 4+2*32)
	copy(out[0:4], selSetRelayer[:])
	copy(out[16:36], addr[:]) // 4 + 12 padding .. 4 + 32
	if allowed {
		out[67] = 1 // 4 + 32 + 31
	}
	return out
}

// packAnchorBatch builds calldata for anchorBatch(ids, commitments, blocks).
// Three dynamic arrays: head = 3 offset words, tail = length + elements each.
func packAnchorBatch(ids, commitments [][32]byte, blockNumbers []uint64) ([]byte, error) {
	n := len(ids)
	if len(commitments) != n || len(blockNumbers) != n {
		return nil, fmt.Errorf("evm: batch length mismatch: %d/%d/%d", n, len(commitments), len(blockNumbers))
	}
	arrWords := 1 + n // length word + n element words
	out := make([]byte, 4+(3+3*arrWords)*32)
	copy(out[0:4], selAnchorBatch[:])
	body := out[4:]

	off1 := 3 * 32
	off2 := off1 + arrWords*32
	off3 := off2 + arrWords*32
	putWordUint(body, 0, uint64(off1))
	putWordUint(body, 1, uint64(off2))
	putWordUint(body, 2, uint64(off3))

	writeArr := func(base int, put func(i int, word []byte)) {
		putWordUint(body, base/32, uint64(n))
		for i := 0; i < n; i++ {
			put(i, body[base+(1+i)*32:base+(2+i)*32])
		}
	}
	writeArr(off1, func(i int, w []byte) { copy(w, ids[i][:]) })
	writeArr(off2, func(i int, w []byte) { copy(w, commitments[i][:]) })
	writeArr(off3, func(i int, w []byte) { binary.BigEndian.PutUint64(w[24:], blockNumbers[i]) })
	return out, nil
}

func putWordUint(body []byte, wordIndex int, v uint64) {
	binary.BigEndian.PutUint64(body[wordIndex*32+24:wordIndex*32+32], v)
}

// AnchorRecord is the decoded result of getAnchor.
type AnchorRecord struct {
	Commitment   [32]byte
	BlockNumber  uint64
	EVMTimestamp uint64
	Exists       bool
}

// RootRecord is the decoded result of getRoot.
type RootRecord struct {
	LeafCount    uint64
	EVMTimestamp uint64
	Exists       bool
}

// unpackGetRoot decodes the 3-word return of getRoot.
func unpackGetRoot(ret []byte) (*RootRecord, error) {
	if len(ret) != 3*32 {
		return nil, fmt.Errorf("evm: getRoot returned %d bytes, want %d", len(ret), 3*32)
	}
	rec := &RootRecord{}
	rec.LeafCount = binary.BigEndian.Uint64(ret[24:32])
	rec.EVMTimestamp = binary.BigEndian.Uint64(ret[32+24 : 64])
	rec.Exists = ret[95] == 1
	return rec, nil
}

// unpackGetAnchor decodes the 4-word return of getAnchor.
func unpackGetAnchor(ret []byte) (*AnchorRecord, error) {
	if len(ret) != 4*32 {
		return nil, fmt.Errorf("evm: getAnchor returned %d bytes, want %d", len(ret), 4*32)
	}
	rec := &AnchorRecord{}
	copy(rec.Commitment[:], ret[0:32])
	rec.BlockNumber = binary.BigEndian.Uint64(ret[32+24 : 64])
	rec.EVMTimestamp = binary.BigEndian.Uint64(ret[64+24 : 96])
	rec.Exists = ret[127] == 1
	return rec, nil
}
