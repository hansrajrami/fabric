// Package proofhelper is the opt-in API for business chaincodes (Part 1 spec,
// section 7): a chaincode declares that the current transaction wants an MST
// proof — and which fields the proof covers — by emitting a single
// MSTProofRequest chaincode event whose payload is the canonical encoding of
// the declared fields.
//
// The event payload IS the canonical encoding: there is no second
// serialization format between the chaincode and the capture service. The
// helper validates the payload at endorsement time, so a malformed
// declaration fails the business transaction synchronously instead of
// surfacing later in the relay pipeline.
//
// Usage:
//
//	err := proofhelper.New().
//		AddString("asset_id", id).
//		AddInt64("value", value).
//		AddBool("active", true).
//		Emit(ctx.GetStub())
//
// Emit must be called at most once per transaction: Fabric records only one
// chaincode event per transaction, and a second SetEvent would silently
// replace the first. The helper cannot see other SetEvent calls in the same
// transaction, so this is a contract with the caller.
package proofhelper

import (
	"fmt"
	"math/big"

	"github.com/hansrajrami/fabric/mst/canonical"
)

// EventName is the well-known chaincode event name that marks a transaction
// as proof-enabled. Only transactions emitting this event are captured.
const EventName = "MSTProofRequest"

// EventSetter is the single stub capability the helper needs. It is satisfied
// by shim.ChaincodeStubInterface (and by contract-api's stub), keeping this
// module free of any Fabric dependency.
type EventSetter interface {
	SetEvent(name string, payload []byte) error
}

// Builder accumulates declared fields with fluent chaining. The first error
// sticks: subsequent Adds are no-ops and Encode/Emit return it, so call sites
// only need a single error check.
type Builder struct {
	fields []canonical.Field
	err    error
}

// New returns an empty payload builder.
func New() *Builder {
	return &Builder{}
}

func (b *Builder) add(f canonical.Field, err error) *Builder {
	if b.err != nil {
		return b
	}
	if err != nil {
		b.err = err
		return b
	}
	b.fields = append(b.fields, f)
	return b
}

// AddInt declares an unsigned integer field (smallest unit only; negative or
// >256-bit values are rejected).
func (b *Builder) AddInt(name string, v *big.Int) *Builder {
	if b.err != nil {
		return b
	}
	f, err := canonical.IntField(name, v)
	return b.add(f, err)
}

// AddInt64 declares a non-negative int64 field.
func (b *Builder) AddInt64(name string, v int64) *Builder {
	if b.err != nil {
		return b
	}
	if v < 0 {
		b.err = fmt.Errorf("proofhelper: field %q: negative values are not allowed", name)
		return b
	}
	return b.AddInt(name, big.NewInt(v))
}

// AddUint64 declares a uint64 field.
func (b *Builder) AddUint64(name string, v uint64) *Builder {
	if b.err != nil {
		return b
	}
	return b.AddInt(name, new(big.Int).SetUint64(v))
}

// AddBool declares a boolean field.
func (b *Builder) AddBool(name string, v bool) *Builder {
	if b.err != nil {
		return b
	}
	f, err := canonical.BoolField(name, v)
	return b.add(f, err)
}

// AddString declares a string field (must be valid, NFC-normalized UTF-8).
func (b *Builder) AddString(name, v string) *Builder {
	if b.err != nil {
		return b
	}
	f, err := canonical.StringField(name, v)
	return b.add(f, err)
}

// AddBytes declares a raw-bytes field (the value is copied).
func (b *Builder) AddBytes(name string, v []byte) *Builder {
	if b.err != nil {
		return b
	}
	f, err := canonical.BytesField(name, v)
	return b.add(f, err)
}

// Err returns the first construction error, if any.
func (b *Builder) Err() error { return b.err }

// Payload returns the declared payload after full validation.
func (b *Builder) Payload() (canonical.Payload, error) {
	if b.err != nil {
		return canonical.Payload{}, b.err
	}
	p := canonical.Payload{Fields: b.fields}
	// Encode validates everything (duplicates included) even though we do
	// not need the bytes here.
	if _, err := canonical.Encode(p); err != nil {
		return canonical.Payload{}, err
	}
	return p, nil
}

// Encode returns the canonical encoding of the declared payload — the exact
// bytes that will travel in the MSTProofRequest event and be hashed into the
// commitment.
func (b *Builder) Encode() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	return canonical.Encode(canonical.Payload{Fields: b.fields})
}

// Emit validates the payload and emits the MSTProofRequest event on the
// stub, opting the current transaction into proof anchoring. Call at most
// once per transaction.
func (b *Builder) Emit(stub EventSetter) error {
	enc, err := b.Encode()
	if err != nil {
		return fmt.Errorf("proofhelper: invalid declared payload: %w", err)
	}
	if err := stub.SetEvent(EventName, enc); err != nil {
		return fmt.Errorf("proofhelper: set event: %w", err)
	}
	return nil
}
