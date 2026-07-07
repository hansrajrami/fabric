package canonical

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"unicode/utf8"
)

// SchemaVersion is the current commitment schema (spec 6.1). Any change to
// the encoding rules or the commitment tuple requires bumping it.
const SchemaVersion uint16 = 1

// domainTagPreimage is hashed once to produce DomainTag; the constant below
// is pinned and cross-checked by tests and the cross-language vectors.
const domainTagPreimage = "MST_FABRIC_TX_ANCHOR_v1"

// DomainTag = keccak256("MST_FABRIC_TX_ANCHOR_v1"). Hard-coded on every side
// of the system (spec 6.1); TestDomainTag recomputes it from the preimage.
var DomainTag = [32]byte{
	0x02, 0x82, 0x5c, 0x8b, 0x6b, 0xe7, 0xe9, 0x70,
	0x13, 0xd4, 0x60, 0x92, 0xe3, 0x95, 0x48, 0x27,
	0xbc, 0x06, 0x86, 0x0c, 0x78, 0x37, 0xe1, 0xae,
	0xd9, 0x56, 0x0d, 0xa3, 0x17, 0xb9, 0x60, 0x93,
}

// ComputeDomainTag recomputes DomainTag from its preimage.
func ComputeDomainTag() [32]byte {
	return Keccak256([]byte(domainTagPreimage))
}

// Practical caps for the two identifier strings in the commitment tuple.
// Fabric channel and chaincode names are short ASCII; these bounds only guard
// against garbage input.
const maxIDLen = 4096

// Commitment is the fixed 8-field tuple that gets ABI-encoded and hashed
// (spec 6.1/6.2). The keccak256 of the encoding is what the anchor contract
// stores for the transaction.
type Commitment struct {
	SchemaVersion uint16
	DomainTag     [32]byte
	FabricTxID    [32]byte
	ChannelID     string
	ChaincodeID   string
	BlockNumber   uint64
	Timestamp     uint64
	PayloadHash   [32]byte
}

// NewCommitment assembles a Commitment with the current schema version and
// domain tag over the caller-supplied transaction facts.
func NewCommitment(fabricTxID [32]byte, channelID, chaincodeID string, blockNumber, timestamp uint64, payloadHash [32]byte) Commitment {
	return Commitment{
		SchemaVersion: SchemaVersion,
		DomainTag:     DomainTag,
		FabricTxID:    fabricTxID,
		ChannelID:     channelID,
		ChaincodeID:   chaincodeID,
		BlockNumber:   blockNumber,
		Timestamp:     timestamp,
		PayloadHash:   payloadHash,
	}
}

// ABIEncode produces the standard EVM ABI encoding (abi.encode in Solidity)
// of the tuple (uint16, bytes32, bytes32, string, string, uint64, uint64,
// bytes32). The layout is fixed — 8 head slots with the two strings as
// dynamic tail entries — so it is implemented directly rather than through a
// generic ABI library; the cross-language vectors pin it against ethers v6's
// AbiCoder, an independent implementation of the same standard.
func (c *Commitment) ABIEncode() ([]byte, error) {
	if err := validateID("channel_id", c.ChannelID); err != nil {
		return nil, err
	}
	if err := validateID("chaincode_id", c.ChaincodeID); err != nil {
		return nil, err
	}

	channelPadded := pad32(len(c.ChannelID))
	chaincodePadded := pad32(len(c.ChaincodeID))

	const headSlots = 8
	head := headSlots * 32
	// Tail entry: 32-byte length slot + data padded to a 32-byte multiple.
	channelOffset := head
	chaincodeOffset := channelOffset + 32 + channelPadded
	total := chaincodeOffset + 32 + chaincodePadded

	out := make([]byte, total)
	putUint(out[0:32], uint64(c.SchemaVersion))    // slot 0: uint16 schema_version
	copy(out[32:64], c.DomainTag[:])               // slot 1: bytes32 domain_tag
	copy(out[64:96], c.FabricTxID[:])              // slot 2: bytes32 fabric_tx_id
	putUint(out[96:128], uint64(channelOffset))    // slot 3: offset of channel_id
	putUint(out[128:160], uint64(chaincodeOffset)) // slot 4: offset of chaincode_id
	putUint(out[160:192], c.BlockNumber)           // slot 5: uint64 block_number
	putUint(out[192:224], c.Timestamp)             // slot 6: uint64 timestamp
	copy(out[224:256], c.PayloadHash[:])           // slot 7: bytes32 payload_hash

	putUint(out[channelOffset:channelOffset+32], uint64(len(c.ChannelID)))
	copy(out[channelOffset+32:], c.ChannelID)
	putUint(out[chaincodeOffset:chaincodeOffset+32], uint64(len(c.ChaincodeID)))
	copy(out[chaincodeOffset+32:], c.ChaincodeID)
	return out, nil
}

// Hash returns keccak256(ABIEncode()) — the commitment value that is anchored
// on the MST chain (spec 6.2).
func (c *Commitment) Hash() ([32]byte, error) {
	enc, err := c.ABIEncode()
	if err != nil {
		return [32]byte{}, err
	}
	return Keccak256(enc), nil
}

// BuildCommitment is the spec-named convenience wrapper around Hash.
func BuildCommitment(c Commitment) ([32]byte, error) {
	return c.Hash()
}

// ParseFabricTxID parses a Fabric transaction ID (64 hex chars, the SHA-256
// of the tx nonce+creator) into its bytes32 form used as the anchor key.
func ParseFabricTxID(txID string) ([32]byte, error) {
	var out [32]byte
	if len(txID) != 64 {
		return out, fmt.Errorf("%w: fabric tx id must be 64 hex chars, got %d", ErrInvalidValue, len(txID))
	}
	if _, err := hex.Decode(out[:], []byte(txID)); err != nil {
		return out, fmt.Errorf("%w: fabric tx id: %v", ErrInvalidValue, err)
	}
	return out, nil
}

func validateID(what, s string) error {
	if len(s) > maxIDLen {
		return fmt.Errorf("%w: %s %d bytes > %d", ErrTooLarge, what, len(s), maxIDLen)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: %s", ErrInvalidUTF8, what)
	}
	return nil
}

func pad32(n int) int { return (n + 31) &^ 31 }

func putUint(slot []byte, v uint64) {
	binary.BigEndian.PutUint64(slot[24:32], v)
}
