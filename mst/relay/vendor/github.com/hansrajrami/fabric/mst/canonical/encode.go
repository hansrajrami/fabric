package canonical

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Encode serializes p using the canonical rules (spec 6.3):
//
//	uint32-BE field count, then for each field in name-byte order:
//	uint32-BE len(name) ‖ name ‖ uint8 type tag ‖ uint32-BE len(value) ‖ value
//
// The input is validated in full; any disallowed content (duplicate names,
// invalid tags, non-canonical values, oversized input) returns an error and
// no bytes. Encode never mutates p.
func Encode(p Payload) ([]byte, error) {
	n := len(p.Fields)
	if n > MaxFields {
		return nil, fmt.Errorf("%w: %d fields > %d", ErrTooLarge, n, MaxFields)
	}

	// Validate every field before doing any work, so the error/no-bytes
	// contract holds even for partially valid payloads.
	for i := range p.Fields {
		if err := validateField(p.Fields[i]); err != nil {
			return nil, err
		}
	}

	// Sort a slice of indices by the raw UTF-8 bytes of the field names.
	// Go's native string ordering is exactly byte-wise lexicographic, never
	// locale-aware, which is what the spec requires.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		return p.Fields[order[a]].Name < p.Fields[order[b]].Name
	})

	// Duplicate names are adjacent after sorting.
	size := 4
	for i := 0; i < n; i++ {
		f := &p.Fields[order[i]]
		if i > 0 && p.Fields[order[i-1]].Name == f.Name {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateField, f.Name)
		}
		size += 4 + len(f.Name) + 1 + 4 + len(f.Value)
	}

	out := make([]byte, 0, size)
	out = binary.BigEndian.AppendUint32(out, uint32(n))
	for _, idx := range order {
		f := &p.Fields[idx]
		out = binary.BigEndian.AppendUint32(out, uint32(len(f.Name)))
		out = append(out, f.Name...)
		out = append(out, byte(f.Type))
		out = binary.BigEndian.AppendUint32(out, uint32(len(f.Value)))
		out = append(out, f.Value...)
	}
	return out, nil
}

// PayloadHash returns keccak256(Encode(p)) — the fingerprint of the declared
// payload that goes into the commitment (spec 6.3).
func PayloadHash(p Payload) ([32]byte, error) {
	enc, err := Encode(p)
	if err != nil {
		return [32]byte{}, err
	}
	return Keccak256(enc), nil
}
