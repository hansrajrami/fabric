package canonical

import (
	"bytes"
	"errors"
	"math/big"
	"math/rand"
	"testing"
)

func encodeOK(t *testing.T, fields ...Field) []byte {
	t.Helper()
	enc, err := Encode(Payload{Fields: fields})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return enc
}

func hashOK(t *testing.T, fields ...Field) [32]byte {
	t.Helper()
	h, err := PayloadHash(Payload{Fields: fields})
	if err != nil {
		t.Fatalf("PayloadHash: %v", err)
	}
	return h
}

func TestEmptyPayload(t *testing.T) {
	enc := encodeOK(t)
	if !bytes.Equal(enc, []byte{0, 0, 0, 0}) {
		t.Fatalf("empty payload must encode to 4 zero bytes, got %x", enc)
	}
}

func TestEncodingLayout(t *testing.T) {
	// { "a": bool(true) } — verify the exact byte layout of rule 3.
	enc := encodeOK(t, mustBool(t, "a", true))
	want := []byte{
		0, 0, 0, 1, // field count
		0, 0, 0, 1, // len(name)
		'a',        // name
		0x02,       // type tag bool
		0, 0, 0, 1, // len(value)
		0x01, // value
	}
	if !bytes.Equal(enc, want) {
		t.Fatalf("layout mismatch:\n got %x\nwant %x", enc, want)
	}
}

func TestOrderIndependenceStress(t *testing.T) {
	// Vector B1, stress-run to defeat any map/ordering nondeterminism.
	fields := []Field{
		mustInt(t, "x", big.NewInt(1)),
		mustInt(t, "y", big.NewInt(2)),
		mustString(t, "name", "hello"),
		mustBytes(t, "blob", []byte{9, 9}),
		mustBool(t, "flag", true),
	}
	ref := hashOK(t, fields...)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 1000; i++ {
		shuffled := append([]Field(nil), fields...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if h := hashOK(t, shuffled...); h != ref {
			t.Fatalf("iteration %d: hash differs under reordering", i)
		}
	}
}

func TestByteWiseSortNotLocale(t *testing.T) {
	// Vector B2: 'Z'(0x5A) < '_'(0x5F) < 'a'(0x61) — byte order, not locale order.
	enc := encodeOK(t,
		mustInt(t, "a", big.NewInt(2)),
		mustInt(t, "Z", big.NewInt(1)),
		mustInt(t, "_", big.NewInt(3)),
	)
	zPos := bytes.Index(enc, []byte("Z"))
	uPos := bytes.Index(enc, []byte("_"))
	aPos := bytes.Index(enc, []byte("a"))
	if !(zPos < uPos && uPos < aPos) {
		t.Fatalf("sort order wrong: Z@%d _@%d a@%d", zPos, uPos, aPos)
	}
}

func TestMultiByteNameSort(t *testing.T) {
	// Vector B3: names sorted by encoded UTF-8 bytes:
	// "e"(65) < "é"(C3 A9) < "ф"(D1 84) < "😀"(F0 9F 98 80).
	names := []string{"\U0001F600", "ф", "é", "e"}
	var fields []Field
	for i, n := range names {
		fields = append(fields, mustInt(t, n, big.NewInt(int64(i))))
	}
	enc := encodeOK(t, fields...)
	positions := []int{
		bytes.Index(enc, []byte("e")),
		bytes.Index(enc, []byte("é")),
		bytes.Index(enc, []byte("ф")),
		bytes.Index(enc, []byte("\U0001F600")),
	}
	for i := 1; i < len(positions); i++ {
		if positions[i-1] >= positions[i] {
			t.Fatalf("multi-byte sort broken: positions %v", positions)
		}
	}
}

func TestDuplicateNameRejected(t *testing.T) {
	_, err := Encode(Payload{Fields: []Field{
		mustInt(t, "a", big.NewInt(1)),
		mustInt(t, "a", big.NewInt(2)),
	}})
	if !errors.Is(err, ErrDuplicateField) {
		t.Fatalf("want ErrDuplicateField, got %v", err)
	}
}

func TestFramingDistinctness(t *testing.T) {
	// Vector C1: {"k1":"ab","k2":"c"} != {"k1":"a","k2":"bc"}.
	h1 := hashOK(t, mustString(t, "k1", "ab"), mustString(t, "k2", "c"))
	h2 := hashOK(t, mustString(t, "k1", "a"), mustString(t, "k2", "bc"))
	if h1 == h2 {
		t.Fatal("framing collision: shifted value bytes hash equal")
	}
}

func TestFieldCountGuard(t *testing.T) {
	// Vector C2: adding/removing a field always changes the hash.
	h1 := hashOK(t, mustInt(t, "a", big.NewInt(1)))
	h2 := hashOK(t, mustInt(t, "a", big.NewInt(1)), mustInt(t, "b", big.NewInt(2)))
	if h1 == h2 {
		t.Fatal("field-count collision")
	}
}

func TestTypeTagDistinctness(t *testing.T) {
	// Vector C3: same name, same-looking value, different types.
	hInt := hashOK(t, mustInt(t, "v", big.NewInt(100)))
	hStr := hashOK(t, mustString(t, "v", "100"))
	var raw [32]byte
	raw[31] = 100
	hBytes := hashOK(t, mustBytes(t, "v", raw[:]))
	if hInt == hStr || hInt == hBytes || hStr == hBytes {
		t.Fatalf("type-tag collision: int=%x str=%x bytes=%x", hInt, hStr, hBytes)
	}
}

func TestPresentEmptyVsAbsent(t *testing.T) {
	// Vector C5: { "a": "" } != {}.
	hEmpty := hashOK(t)
	hPresent := hashOK(t, mustString(t, "a", ""))
	if hEmpty == hPresent {
		t.Fatal("present-but-empty collides with absent")
	}
}

func TestEncodeRejectsHandBuiltBadFields(t *testing.T) {
	cases := []struct {
		desc  string
		field Field
		want  error
	}{
		{"int wrong width", Field{Name: "v", Type: TypeInt, Value: []byte{1}}, ErrInvalidValue},
		{"bool bad byte", Field{Name: "v", Type: TypeBool, Value: []byte{0x02}}, ErrInvalidValue},
		{"bool wrong width", Field{Name: "v", Type: TypeBool, Value: []byte{0, 1}}, ErrInvalidValue},
		{"unknown tag", Field{Name: "v", Type: FieldType(0x09), Value: nil}, ErrInvalidType},
		{"float-ish tag zero", Field{Name: "v", Type: FieldType(0x00), Value: nil}, ErrInvalidType},
		{"non-NFC string value", Field{Name: "v", Type: TypeString, Value: []byte("é")}, ErrNotNFC},
		{"invalid UTF-8 string value", Field{Name: "v", Type: TypeString, Value: []byte{0xff}}, ErrInvalidUTF8},
	}
	for _, tc := range cases {
		_, err := Encode(Payload{Fields: []Field{tc.field}})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", tc.desc, tc.want, err)
		}
	}
}

func TestEncodeDoesNotMutateInput(t *testing.T) {
	fields := []Field{
		mustInt(t, "z", big.NewInt(1)),
		mustInt(t, "a", big.NewInt(2)),
	}
	if _, err := Encode(Payload{Fields: fields}); err != nil {
		t.Fatal(err)
	}
	if fields[0].Name != "z" || fields[1].Name != "a" {
		t.Fatal("Encode reordered the caller's slice")
	}
}

func TestTooManyFields(t *testing.T) {
	fields := make([]Field, MaxFields+1)
	for i := range fields {
		fields[i] = Field{Name: "f", Type: TypeBool, Value: []byte{0}}
	}
	if _, err := Encode(Payload{Fields: fields}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}
