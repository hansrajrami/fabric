// Package txmodel holds the proto-free view of committed Fabric transactions
// that the capture pipeline consumes.
//
// It exists so the capture core can be linked into BOTH worlds: the
// standalone relayer (whose sources parse fabric-protos-go-apiv2 blocks from
// the Fabric Gateway) and the Fabric peer binary itself (which links the old
// fabric-protos-go module). The two proto modules register the same proto
// file paths and panic if linked together, so everything shared between the
// two deployments must be proto-agnostic — this package is that boundary.
package txmodel

// Event is one chaincode event of a transaction.
type Event struct {
	ChaincodeID string
	EventName   string
	Payload     []byte
}

// Tx is one endorser transaction of a committed block.
type Tx struct {
	TxID          string
	ChannelID     string
	TimestampUnix uint64 // ChannelHeader.Timestamp (client-asserted, in the signed envelope)
	// Valid reports whether the transaction committed successfully
	// (TxValidationCode == VALID in the block's TRANSACTIONS_FILTER).
	Valid  bool
	Events []Event
}

// BadEnvelope records a block entry that could not be parsed. Surfacing
// these (instead of failing the whole block or silently skipping) lets the
// capture service log poison pills without wedging the stream.
type BadEnvelope struct {
	Index int
	Err   error
}

// Block is one committed block, parsed.
type Block struct {
	Number uint64
	Txs    []Tx
	Bad    []BadEnvelope
}
