// Package canonical implements the deterministic, byte-exact serialization of
// declared payloads and the commitment formula for MST proof anchoring
// (Phase 1 Part 1 spec, sections 6 and 10).
//
// The encoding rules make it impossible for two distinct logical payloads to
// produce the same bytes: fields are sorted by the raw UTF-8 bytes of their
// names, the field count and every name/value is length-prefixed, and each
// value carries a type tag with exactly one canonical representation per
// logical value. Anything ambiguous (floats, nesting, duplicate names,
// non-NFC or invalid UTF-8) is rejected with an error, never silently encoded.
//
// This package is deliberately dependency-light (x/crypto for keccak256 and
// x/text for NFC checks only) so chaincode can vendor it cheaply, and so the
// TypeScript reference implementation in mst/testvectors remains a genuinely
// independent cross-check.
package canonical

import (
	"errors"
	"fmt"
	"math/big"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// FieldType tags the canonical value encoding of a declared-payload field.
type FieldType uint8

const (
	// TypeInt is an unsigned integer, encoded as exactly 32 big-endian bytes
	// (smallest unit only — one canonical form per value).
	TypeInt FieldType = 0x01
	// TypeBool is a single byte, 0x00 or 0x01.
	TypeBool FieldType = 0x02
	// TypeString is NFC-normalized, valid UTF-8 bytes.
	TypeString FieldType = 0x03
	// TypeBytes is raw bytes.
	TypeBytes FieldType = 0x04
)

// Hard limits that keep encoding and decoding allocation-safe. A declared
// payload is a set of business field fingerprints, not a bulk data channel;
// anything beyond these limits is almost certainly a mistake or an attack.
const (
	MaxFields    = 4096
	MaxNameLen   = 1024              // bytes of UTF-8
	MaxValueLen  = 1 << 20           // 1 MiB per value
	intValueLen  = 32                // canonical int width
	boolValueLen = 1                 // canonical bool width
)

// Sentinel errors. All validation failures wrap one of these, so callers can
// classify rejections without string matching.
var (
	ErrDuplicateField = errors.New("canonical: duplicate field name")
	ErrInvalidName    = errors.New("canonical: invalid field name")
	ErrInvalidType    = errors.New("canonical: invalid field type tag")
	ErrInvalidValue   = errors.New("canonical: invalid field value")
	ErrInvalidUTF8    = errors.New("canonical: invalid UTF-8")
	ErrNotNFC         = errors.New("canonical: string not NFC-normalized")
	ErrTooLarge       = errors.New("canonical: size limit exceeded")
	ErrTruncated      = errors.New("canonical: truncated encoding")
	ErrTrailingBytes  = errors.New("canonical: trailing bytes after encoding")
	ErrNotCanonical   = errors.New("canonical: encoding is not in canonical form")
	ErrIntRange       = errors.New("canonical: integer out of range (must fit unsigned 256-bit)")
)

// Field is one named, typed value of a declared payload. Value always holds
// the canonical per-type value encoding (32-byte BE for ints, one byte for
// bools, NFC UTF-8 for strings, raw for bytes). Use the typed constructors to
// build well-formed fields; Encode independently re-validates every field so
// hand-built Fields cannot smuggle a non-canonical value.
type Field struct {
	Name  string
	Type  FieldType
	Value []byte
}

// Payload is a flat set of declared fields. Order does not matter: Encode
// sorts by the raw UTF-8 bytes of the field names.
type Payload struct {
	Fields []Field
}

// IntField builds an int field from v. The canonical form is exactly 32
// big-endian bytes; v must be non-nil, non-negative, and fit in 256 bits.
func IntField(name string, v *big.Int) (Field, error) {
	if err := validateName(name); err != nil {
		return Field{}, err
	}
	if v == nil || v.Sign() < 0 || v.BitLen() > 256 {
		return Field{}, fmt.Errorf("%w: field %q", ErrIntRange, name)
	}
	value := make([]byte, intValueLen)
	v.FillBytes(value)
	return Field{Name: name, Type: TypeInt, Value: value}, nil
}

// BoolField builds a bool field (canonical form: one byte, 0x00 or 0x01).
func BoolField(name string, v bool) (Field, error) {
	if err := validateName(name); err != nil {
		return Field{}, err
	}
	value := []byte{0x00}
	if v {
		value[0] = 0x01
	}
	return Field{Name: name, Type: TypeBool, Value: value}, nil
}

// StringField builds a string field. s must be valid, NFC-normalized UTF-8;
// non-normalized input is rejected, not silently normalized, so that every
// party hashes exactly the bytes the business application supplied.
func StringField(name, s string) (Field, error) {
	if err := validateName(name); err != nil {
		return Field{}, err
	}
	if err := validateStringValue([]byte(s)); err != nil {
		return Field{}, fmt.Errorf("field %q: %w", name, err)
	}
	if len(s) > MaxValueLen {
		return Field{}, fmt.Errorf("%w: field %q value %d bytes > %d", ErrTooLarge, name, len(s), MaxValueLen)
	}
	return Field{Name: name, Type: TypeString, Value: []byte(s)}, nil
}

// BytesField builds a raw-bytes field. The value is copied.
func BytesField(name string, b []byte) (Field, error) {
	if err := validateName(name); err != nil {
		return Field{}, err
	}
	if len(b) > MaxValueLen {
		return Field{}, fmt.Errorf("%w: field %q value %d bytes > %d", ErrTooLarge, name, len(b), MaxValueLen)
	}
	value := make([]byte, len(b))
	copy(value, b)
	return Field{Name: name, Type: TypeBytes, Value: value}, nil
}

func validateName(name string) error {
	if len(name) == 0 {
		return fmt.Errorf("%w: empty name", ErrInvalidName)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("%w: name %d bytes > %d", ErrTooLarge, len(name), MaxNameLen)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: name is not valid UTF-8", ErrInvalidUTF8)
	}
	if !norm.NFC.IsNormalString(name) {
		return fmt.Errorf("%w: name is not NFC-normalized", ErrNotNFC)
	}
	return nil
}

func validateStringValue(b []byte) error {
	if !utf8.Valid(b) {
		return ErrInvalidUTF8
	}
	if !norm.NFC.IsNormal(b) {
		return ErrNotNFC
	}
	return nil
}

// validateField re-checks a Field regardless of how it was constructed.
func validateField(f Field) error {
	if err := validateName(f.Name); err != nil {
		return err
	}
	switch f.Type {
	case TypeInt:
		if len(f.Value) != intValueLen {
			return fmt.Errorf("%w: field %q int value must be exactly %d bytes, got %d",
				ErrInvalidValue, f.Name, intValueLen, len(f.Value))
		}
	case TypeBool:
		if len(f.Value) != boolValueLen || f.Value[0] > 0x01 {
			return fmt.Errorf("%w: field %q bool value must be one byte 0x00/0x01",
				ErrInvalidValue, f.Name)
		}
	case TypeString:
		if err := validateStringValue(f.Value); err != nil {
			return fmt.Errorf("field %q: %w", f.Name, err)
		}
	case TypeBytes:
		// Raw bytes carry no content constraints.
	default:
		return fmt.Errorf("%w: field %q has tag 0x%02x", ErrInvalidType, f.Name, uint8(f.Type))
	}
	if len(f.Value) > MaxValueLen {
		return fmt.Errorf("%w: field %q value %d bytes > %d", ErrTooLarge, f.Name, len(f.Value), MaxValueLen)
	}
	return nil
}
