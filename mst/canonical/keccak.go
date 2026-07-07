package canonical

import "golang.org/x/crypto/sha3"

// Keccak256 returns the legacy Keccak-256 digest (the EVM's keccak256, not
// NIST SHA3-256) of data.
func Keccak256(data []byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data) // never errors per hash.Hash contract
	var out [32]byte
	h.Sum(out[:0])
	return out
}
