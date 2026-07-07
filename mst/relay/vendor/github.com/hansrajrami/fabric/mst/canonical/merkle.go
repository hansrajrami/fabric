package canonical

import (
	"bytes"
	"fmt"
	"sort"
)

// Merkle batching (the "proof of combination of proofs"): a batch of
// per-transaction commitments is aggregated into one 32-byte root, and only
// the root is anchored on the MST chain. Holding a transaction's original
// data plus the ~log2(N) sibling hashes of its inclusion proof, anyone can
// verify the transaction was part of the anchored batch.
//
// Canonical tree rules (all sides must agree; pinned by tests):
//
//  1. Leaves are the per-transaction commitments themselves (32-byte keccak
//     images of the ABI-encoded tuple), sorted ascending as byte strings.
//     Sorting makes the root independent of capture order, so any party
//     holding the batch's commitments reproduces it.
//  2. Parent = keccak256(min(a,b) ‖ max(a,b)) — commutative "sorted pair"
//     hashing, byte-compatible with OpenZeppelin's MerkleProof, so a Part 2
//     on-chain verifier can use the audited OZ library as-is.
//  3. An odd node at any level is promoted unchanged to the next level.
//  4. A single-leaf batch has root == leaf (proof is empty).
//  5. Empty batches are invalid.
//
// Second-preimage note: sorted-pair hashing without an explicit leaf/node
// domain tag is safe HERE because leaves are themselves keccak256 outputs of
// structured >64-byte preimages (the commitment tuple encoding) — presenting
// an internal node (keccak of 64 bytes) as a leaf would require exhibiting a
// valid tuple encoding hashing to it, i.e. a keccak preimage.

// MerkleRoot computes the batch root over the given commitments.
func MerkleRoot(leaves [][32]byte) ([32]byte, error) {
	level, err := sortedLeaves(leaves)
	if err != nil {
		return [32]byte{}, err
	}
	for len(level) > 1 {
		level = reduceLevel(level, nil, -1)
	}
	return level[0], nil
}

// MerkleProof returns the inclusion proof (bottom-up sibling hashes) for the
// given leaf commitment. The proof, the leaf, and the root are all that a
// verifier needs — plus VerifyMerkleProof.
func MerkleProof(leaves [][32]byte, leaf [32]byte) ([][32]byte, error) {
	level, err := sortedLeaves(leaves)
	if err != nil {
		return nil, err
	}
	index := -1
	for i := range level {
		if level[i] == leaf {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("canonical: leaf %x is not in the batch", leaf)
	}

	var proof [][32]byte
	for len(level) > 1 {
		var sibling *[32]byte
		level, sibling, index = reduceLevelTracking(level, index)
		if sibling != nil {
			proof = append(proof, *sibling)
		}
	}
	return proof, nil
}

// VerifyMerkleProof reports whether leaf is included in the batch with the
// given root, using OpenZeppelin-compatible sorted-pair processing.
func VerifyMerkleProof(root, leaf [32]byte, proof [][32]byte) bool {
	computed := leaf
	for _, sibling := range proof {
		computed = hashPair(computed, sibling)
	}
	return computed == root
}

func sortedLeaves(leaves [][32]byte) ([][32]byte, error) {
	if len(leaves) == 0 {
		return nil, fmt.Errorf("canonical: empty batch")
	}
	out := make([][32]byte, len(leaves))
	copy(out, leaves)
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i][:], out[j][:]) < 0
	})
	for i := 1; i < len(out); i++ {
		if out[i] == out[i-1] {
			return nil, fmt.Errorf("canonical: duplicate leaf %x", out[i])
		}
	}
	return out, nil
}

// reduceLevel builds the next tree level. (Tracking variant below is used
// when a proof path is being collected.)
func reduceLevel(level [][32]byte, _ [][32]byte, _ int) [][32]byte {
	next := make([][32]byte, 0, (len(level)+1)/2)
	for i := 0; i < len(level); i += 2 {
		if i+1 == len(level) {
			next = append(next, level[i]) // odd node promotes unchanged
			continue
		}
		next = append(next, hashPair(level[i], level[i+1]))
	}
	return next
}

// reduceLevelTracking is reduceLevel while following one node upward:
// it returns the next level, the tracked node's sibling at this level (nil
// when the node was promoted without a partner), and its index above.
func reduceLevelTracking(level [][32]byte, index int) ([][32]byte, *[32]byte, int) {
	next := make([][32]byte, 0, (len(level)+1)/2)
	var sibling *[32]byte
	nextIndex := index / 2
	for i := 0; i < len(level); i += 2 {
		if i+1 == len(level) {
			next = append(next, level[i])
			continue
		}
		if i == index || i+1 == index {
			s := level[i]
			if i == index {
				s = level[i+1]
			}
			sibling = &s
		}
		next = append(next, hashPair(level[i], level[i+1]))
	}
	return next, sibling, nextIndex
}

func hashPair(a, b [32]byte) [32]byte {
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}
	var buf [64]byte
	copy(buf[:32], a[:])
	copy(buf[32:], b[:])
	return Keccak256(buf[:])
}
