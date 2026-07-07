package canonical

import (
	"bytes"
	"testing"
)

// FuzzDecodeReEncode asserts the core canonicity property: any byte string
// that Decode accepts must re-encode to exactly itself. If this ever fails,
// two different byte strings could describe the same logical payload and the
// "what you hash is what was emitted" guarantee breaks.
func FuzzDecodeReEncode(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0})
	seed, _ := Encode(Payload{Fields: []Field{
		{Name: "a", Type: TypeBool, Value: []byte{1}},
		{Name: "b", Type: TypeBytes, Value: []byte{0xde, 0xad}},
	}})
	f.Add(seed)
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 1, 'x', 0x02, 0, 0, 0, 1, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Decode(data)
		if err != nil {
			return // rejection is fine; acceptance must be canonical
		}
		re, err := Encode(p)
		if err != nil {
			t.Fatalf("decoded payload failed to re-encode: %v", err)
		}
		if !bytes.Equal(re, data) {
			t.Fatalf("non-canonical acceptance:\n in %x\nout %x", data, re)
		}
	})
}
