package canonical

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
	"testing"
)

func mustInt(t *testing.T, name string, v *big.Int) Field {
	t.Helper()
	f, err := IntField(name, v)
	if err != nil {
		t.Fatalf("IntField(%q): %v", name, err)
	}
	return f
}

func mustBool(t *testing.T, name string, v bool) Field {
	t.Helper()
	f, err := BoolField(name, v)
	if err != nil {
		t.Fatalf("BoolField(%q): %v", name, err)
	}
	return f
}

func mustString(t *testing.T, name, v string) Field {
	t.Helper()
	f, err := StringField(name, v)
	if err != nil {
		t.Fatalf("StringField(%q): %v", name, err)
	}
	return f
}

func mustBytes(t *testing.T, name string, v []byte) Field {
	t.Helper()
	f, err := BytesField(name, v)
	if err != nil {
		t.Fatalf("BytesField(%q): %v", name, err)
	}
	return f
}

func TestIntFieldCanonicalization(t *testing.T) {
	// The same logical value from differently-built big.Ints must produce
	// identical canonical bytes (vector C4).
	a := mustInt(t, "v", big.NewInt(1))
	b := mustInt(t, "v", new(big.Int).SetBytes([]byte{0, 0, 0, 1}))
	if !bytes.Equal(a.Value, b.Value) {
		t.Fatalf("int canonical form differs: %x vs %x", a.Value, b.Value)
	}
	if len(a.Value) != 32 {
		t.Fatalf("int value must be 32 bytes, got %d", len(a.Value))
	}
	if a.Value[31] != 0x01 {
		t.Fatalf("expected big-endian 1, got %x", a.Value)
	}

	one := mustInt(t, "v", big.NewInt(1))
	twoFiftySix := mustInt(t, "v", big.NewInt(256))
	if bytes.Equal(one.Value, twoFiftySix.Value) {
		t.Fatal("1 and 256 must not share a canonical form")
	}

	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	f := mustInt(t, "v", max)
	if f.Value[0] != 0xff || f.Value[31] != 0xff {
		t.Fatalf("max uint256 encoded wrong: %x", f.Value)
	}
}

func TestIntFieldRejections(t *testing.T) {
	if _, err := IntField("v", nil); !errors.Is(err, ErrIntRange) {
		t.Fatalf("nil: want ErrIntRange, got %v", err)
	}
	if _, err := IntField("v", big.NewInt(-1)); !errors.Is(err, ErrIntRange) {
		t.Fatalf("negative: want ErrIntRange, got %v", err)
	}
	tooBig := new(big.Int).Lsh(big.NewInt(1), 256)
	if _, err := IntField("v", tooBig); !errors.Is(err, ErrIntRange) {
		t.Fatalf("2^256: want ErrIntRange, got %v", err)
	}
}

func TestBoolField(t *testing.T) {
	f := mustBool(t, "b", false)
	if !bytes.Equal(f.Value, []byte{0x00}) {
		t.Fatalf("false must encode as 0x00, got %x", f.Value)
	}
	f = mustBool(t, "b", true)
	if !bytes.Equal(f.Value, []byte{0x01}) {
		t.Fatalf("true must encode as 0x01, got %x", f.Value)
	}
}

func TestStringFieldRejections(t *testing.T) {
	// "é" in decomposed (NFD) form: 'e' + COMBINING ACUTE ACCENT.
	if _, err := StringField("s", "é"); !errors.Is(err, ErrNotNFC) {
		t.Fatalf("NFD string: want ErrNotNFC, got %v", err)
	}
	if _, err := StringField("s", string([]byte{0xff, 0xfe})); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid UTF-8: want ErrInvalidUTF8, got %v", err)
	}
	// NFC form is fine.
	if _, err := StringField("s", "é"); err != nil {
		t.Fatalf("NFC string rejected: %v", err)
	}
	// Empty string value is allowed (vector C5 relies on it).
	if _, err := StringField("s", ""); err != nil {
		t.Fatalf("empty string rejected: %v", err)
	}
}

func TestBytesFieldCopies(t *testing.T) {
	src := []byte{1, 2, 3}
	f := mustBytes(t, "d", src)
	src[0] = 99
	if f.Value[0] != 1 {
		t.Fatal("BytesField must copy its input")
	}
}

func TestNameValidation(t *testing.T) {
	cases := []struct {
		name string
		want error
	}{
		{"", ErrInvalidName},
		{strings.Repeat("a", MaxNameLen+1), ErrTooLarge},
		{string([]byte{0xff}), ErrInvalidUTF8},
		{"é", ErrNotNFC},
	}
	for _, tc := range cases {
		if _, err := BoolField(tc.name, true); !errors.Is(err, tc.want) {
			t.Errorf("name %q: want %v, got %v", tc.name, tc.want, err)
		}
	}
	// Multi-byte NFC names are fine.
	if _, err := BoolField("éф", true); err != nil {
		t.Fatalf("valid multi-byte name rejected: %v", err)
	}
}
