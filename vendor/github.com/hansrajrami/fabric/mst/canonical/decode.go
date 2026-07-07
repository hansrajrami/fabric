package canonical

import (
	"encoding/binary"
	"fmt"
)

// Decode parses a canonical payload encoding, enforcing strict canonicity:
// fields must appear in strictly ascending name-byte order (which also rules
// out duplicates), every value must be in its canonical per-type form, and no
// trailing bytes may follow. Consequently, for any payload p returned by
// Decode(data), Encode(p) == data byte-for-byte — the capture service relies
// on this to guarantee that what it hashes is exactly what the chaincode
// emitted, with no alternative serialization accepted.
func Decode(data []byte) (Payload, error) {
	r := reader{buf: data}

	count, err := r.uint32()
	if err != nil {
		return Payload{}, err
	}
	if count > MaxFields {
		return Payload{}, fmt.Errorf("%w: %d fields > %d", ErrTooLarge, count, MaxFields)
	}

	fields := make([]Field, 0, count)
	var prevName string
	for i := uint32(0); i < count; i++ {
		nameLen, err := r.uint32()
		if err != nil {
			return Payload{}, err
		}
		if nameLen > MaxNameLen {
			return Payload{}, fmt.Errorf("%w: name %d bytes > %d", ErrTooLarge, nameLen, MaxNameLen)
		}
		nameBytes, err := r.take(int(nameLen))
		if err != nil {
			return Payload{}, err
		}
		name := string(nameBytes)
		if err := validateName(name); err != nil {
			return Payload{}, err
		}
		if i > 0 {
			if name == prevName {
				return Payload{}, fmt.Errorf("%w: %q", ErrDuplicateField, name)
			}
			if name < prevName {
				return Payload{}, fmt.Errorf("%w: field %q out of order (after %q)",
					ErrNotCanonical, name, prevName)
			}
		}
		prevName = name

		tag, err := r.byte()
		if err != nil {
			return Payload{}, err
		}
		valueLen, err := r.uint32()
		if err != nil {
			return Payload{}, err
		}
		if valueLen > MaxValueLen {
			return Payload{}, fmt.Errorf("%w: value %d bytes > %d", ErrTooLarge, valueLen, MaxValueLen)
		}
		valueBytes, err := r.take(int(valueLen))
		if err != nil {
			return Payload{}, err
		}

		f := Field{Name: name, Type: FieldType(tag), Value: append([]byte(nil), valueBytes...)}
		if err := validateField(f); err != nil {
			return Payload{}, err
		}
		fields = append(fields, f)
	}

	if r.remaining() != 0 {
		return Payload{}, fmt.Errorf("%w: %d bytes", ErrTrailingBytes, r.remaining())
	}
	return Payload{Fields: fields}, nil
}

// reader is a bounds-checked cursor over the encoding. take() slices the
// input (no copy); Decode copies value bytes it retains.
type reader struct {
	buf []byte
	off int
}

func (r *reader) remaining() int { return len(r.buf) - r.off }

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || r.remaining() < n {
		return nil, fmt.Errorf("%w: need %d bytes, have %d", ErrTruncated, n, r.remaining())
	}
	b := r.buf[r.off : r.off+n]
	r.off += n
	return b, nil
}

func (r *reader) byte() (byte, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *reader) uint32() (uint32, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}
