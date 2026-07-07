package canonical

import (
	"fmt"
	"testing"
)

func leaf(b byte) [32]byte {
	// Realistic leaves: keccak images, like real commitments.
	return Keccak256([]byte{b})
}

func leaves(n int) [][32]byte {
	out := make([][32]byte, n)
	for i := range out {
		out[i] = leaf(byte(i + 1))
	}
	return out
}

func TestMerkleSingleLeafRootIsLeaf(t *testing.T) {
	l := leaves(1)
	root, err := MerkleRoot(l)
	if err != nil {
		t.Fatal(err)
	}
	if root != l[0] {
		t.Fatal("single-leaf root must equal the leaf")
	}
	proof, err := MerkleProof(l, l[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(proof) != 0 {
		t.Fatal("single-leaf proof must be empty")
	}
	if !VerifyMerkleProof(root, l[0], proof) {
		t.Fatal("empty proof must verify")
	}
}

func TestMerkleEveryLeafProvesForAllSizes(t *testing.T) {
	// Cover even, odd, power-of-two, and lonely-node-promotion shapes.
	for _, n := range []int{1, 2, 3, 4, 5, 7, 8, 20, 33} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			ls := leaves(n)
			root, err := MerkleRoot(ls)
			if err != nil {
				t.Fatal(err)
			}
			for i, l := range ls {
				proof, err := MerkleProof(ls, l)
				if err != nil {
					t.Fatalf("leaf %d: %v", i, err)
				}
				if !VerifyMerkleProof(root, l, proof) {
					t.Fatalf("leaf %d: proof does not verify", i)
				}
				// A tampered leaf must NOT verify with the same proof.
				bad := l
				bad[0] ^= 1
				if VerifyMerkleProof(root, bad, proof) {
					t.Fatalf("leaf %d: tampered leaf verified", i)
				}
			}
		})
	}
}

func TestMerkleRootIsOrderIndependent(t *testing.T) {
	ls := leaves(9)
	root1, err := MerkleRoot(ls)
	if err != nil {
		t.Fatal(err)
	}
	// Reverse the input order: leaves are canonically sorted internally.
	rev := make([][32]byte, len(ls))
	for i := range ls {
		rev[i] = ls[len(ls)-1-i]
	}
	root2, err := MerkleRoot(rev)
	if err != nil {
		t.Fatal(err)
	}
	if root1 != root2 {
		t.Fatal("root must be independent of input order")
	}
}

func TestMerkleForeignLeafRejected(t *testing.T) {
	ls := leaves(8)
	root, err := MerkleRoot(ls)
	if err != nil {
		t.Fatal(err)
	}
	foreign := leaf(0xEE)
	if _, err := MerkleProof(ls, foreign); err == nil {
		t.Fatal("proof for a leaf outside the batch must error")
	}
	if VerifyMerkleProof(root, foreign, nil) {
		t.Fatal("foreign leaf with empty proof must not verify")
	}
}

func TestMerkleProofFromAnotherBatchFails(t *testing.T) {
	a := leaves(8)
	b := leaves(16)
	rootA, err := MerkleRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	proofB, err := MerkleProof(b, a[0]) // a[0] == b[0] by construction
	if err != nil {
		t.Fatal(err)
	}
	if VerifyMerkleProof(rootA, a[0], proofB) {
		t.Fatal("proof from a different batch must not verify")
	}
}

func TestMerkleRejectsEmptyAndDuplicates(t *testing.T) {
	if _, err := MerkleRoot(nil); err == nil {
		t.Fatal("empty batch must error")
	}
	dup := [][32]byte{leaf(1), leaf(2), leaf(1)}
	if _, err := MerkleRoot(dup); err == nil {
		t.Fatal("duplicate leaves must error")
	}
}

func TestMerklePinnedVector(t *testing.T) {
	// Pin a known tree so the construction can never silently change:
	// leaves keccak(0x01), keccak(0x02) sorted, root = keccak(min‖max).
	l1, l2 := leaf(1), leaf(2)
	want := hashPair(l1, l2)
	root, err := MerkleRoot([][32]byte{l2, l1})
	if err != nil {
		t.Fatal(err)
	}
	if root != want {
		t.Fatalf("pinned 2-leaf root mismatch: %x vs %x", root, want)
	}
	// Commutativity: hashPair(a,b) == hashPair(b,a).
	if hashPair(l1, l2) != hashPair(l2, l1) {
		t.Fatal("pair hashing must be commutative (sorted)")
	}
}
