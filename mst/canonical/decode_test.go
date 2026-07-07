package canonical

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/big"
	"testing"
)

func TestDecodeRoundTrip(t *testing.T) {
	original := Payload{Fields: []Field{
		mustInt(t, "amount", big.NewInt(12345)),
		mustBool(t, "approved", true),
		mustString(t, "memo", "hello é"),
		mustBytes(t, "raw", []byte{0xde, 0xad, 0xbe, 0xef}),
		mustString(t, "empty", ""),
	}}
	enc, err := Encode(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	re, err := Encode(decoded)
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if !bytes.Equal(enc, re) {
		t.Fatalf("round-trip mismatch:\n in %x\nout %x", enc, re)
	}
}

func TestDecodeEmpty(t *testing.T) {
	p, err := Decode([]byte{0, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Fields) != 0 {
		t.Fatalf("want 0 fields, got %d", len(p.Fields))
	}
}

// buildRaw hand-assembles an encoding so tests can produce byte streams the
// encoder itself would refuse to emit.
func buildRaw(count uint32, fields ...[]byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, count)
	for _, f := range fields {
		out = append(out, f...)
	}
	return out
}

func rawField(name string, tag byte, value []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(name)))
	out = append(out, name...)
	out = append(out, tag)
	out = binary.BigEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

func TestDecodeRejections(t *testing.T) {
	boolVal := []byte{0x01}
	intVal := make([]byte, 32)
	cases := []struct {
		desc string
		data []byte
		want error
	}{
		{"nil input", nil, ErrTruncated},
		{"short header", []byte{0, 0}, ErrTruncated},
		{"count without fields", buildRaw(1), ErrTruncated},
		{"trailing bytes", append(buildRaw(1, rawField("a", 0x02, boolVal)), 0xAA), ErrTrailingBytes},
		{"out-of-order fields", buildRaw(2, rawField("b", 0x02, boolVal), rawField("a", 0x02, boolVal)), ErrNotCanonical},
		{"duplicate fields", buildRaw(2, rawField("a", 0x02, boolVal), rawField("a", 0x02, boolVal)), ErrDuplicateField},
		{"unknown tag", buildRaw(1, rawField("a", 0x07, boolVal)), ErrInvalidType},
		{"bool wrong value", buildRaw(1, rawField("a", 0x02, []byte{0x05})), ErrInvalidValue},
		{"int wrong width", buildRaw(1, rawField("a", 0x01, []byte{1, 2, 3})), ErrInvalidValue},
		{"non-NFC string", buildRaw(1, rawField("a", 0x03, []byte("é"))), ErrNotNFC},
		{"invalid UTF-8 string", buildRaw(1, rawField("a", 0x03, []byte{0xff})), ErrInvalidUTF8},
		{"empty name", buildRaw(1, rawField("", 0x02, boolVal)), ErrInvalidName},
		{"non-NFC name", buildRaw(1, rawField("é", 0x02, boolVal)), ErrNotNFC},
		{"truncated value", buildRaw(1, rawField("a", 0x01, intVal)[:20]), ErrTruncated},
		{"absurd field count", buildRaw(1 << 30), ErrTooLarge},
	}
	for _, tc := range cases {
		_, err := Decode(tc.data)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", tc.desc, tc.want, err)
		}
	}
}

func TestDecodeHugeLengthNoAllocation(t *testing.T) {
	// A length prefix far beyond the buffer must fail fast on bounds, not
	// attempt a giant allocation.
	data := binary.BigEndian.AppendUint32(nil, 1)              // 1 field
	data = binary.BigEndian.AppendUint32(data, uint32(900))    // name len 900 > remaining
	data = append(data, 'a')
	if _, err := Decode(data); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}

	data = binary.BigEndian.AppendUint32(nil, 1)
	data = append(data, rawField("a", 0x04, nil)...)
	// Rewrite the value length to MaxValueLen with no data behind it.
	binary.BigEndian.PutUint32(data[len(data)-4:], MaxValueLen)
	if _, err := Decode(data); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

func TestDecodeValueIsCopied(t *testing.T) {
	enc, err := Encode(Payload{Fields: []Field{mustBytes(t, "d", []byte{1, 2, 3})}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	enc[len(enc)-1] = 0xEE // scribble on the input buffer
	if p.Fields[0].Value[2] != 3 {
		t.Fatal("Decode must copy value bytes out of the input buffer")
	}
}
