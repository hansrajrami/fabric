// Package blockparse extracts the transaction facts the capture service
// needs from committed Fabric blocks: tx id, channel, client-asserted
// timestamp, validation code, and chaincode events.
//
// It is a self-contained port (to fabric-protos-go-apiv2) of the peer's own
// gateway event extraction (internal/pkg/gateway/event/{block,transaction}.go
// in fabric release-2.5). Importing github.com/hyperledger/fabric itself is
// deliberately avoided: its protoutil works on the old proto module while the
// fabric-gateway client delivers apiv2 blocks, and the module drags a very
// large dependency graph into the relayer.
package blockparse

import (
	"fmt"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"google.golang.org/protobuf/proto"
)

// Event is one chaincode event of a transaction.
type Event struct {
	ChaincodeID string
	EventName   string
	Payload     []byte
}

// Tx is one endorser transaction of a committed block.
type Tx struct {
	TxID           string
	ChannelID      string
	TimestampUnix  uint64 // ChannelHeader.Timestamp (client-asserted, in the signed envelope)
	ValidationCode peer.TxValidationCode
	Events         []Event
}

// Valid reports whether the transaction committed successfully.
func (t *Tx) Valid() bool { return t.ValidationCode == peer.TxValidationCode_VALID }

// BadEnvelope records a block entry that could not be parsed. Surfacing these
// (instead of failing the whole block or silently skipping) lets the capture
// service quarantine poison pills without wedging the stream.
type BadEnvelope struct {
	Index int
	Err   error
}

// Block is the parse result for one committed block.
type Block struct {
	Number uint64
	Txs    []Tx
	Bad    []BadEnvelope
}

// Parse extracts all endorser transactions from a committed block. Non-endorser
// entries (config transactions etc.) are ignored. Unparseable envelopes are
// reported in Bad rather than failing the block.
func Parse(block *common.Block) (*Block, error) {
	if block == nil {
		return nil, fmt.Errorf("blockparse: nil block")
	}
	out := &Block{Number: block.GetHeader().GetNumber()}

	statusCodes := transactionsFilter(block)
	for i, envelopeBytes := range block.GetData().GetData() {
		tx, isEndorser, err := parseEnvelope(envelopeBytes)
		if err != nil {
			out.Bad = append(out.Bad, BadEnvelope{Index: i, Err: err})
			continue
		}
		if !isEndorser {
			continue
		}
		tx.ValidationCode = statusCode(statusCodes, i)
		out.Txs = append(out.Txs, *tx)
	}
	return out, nil
}

// transactionsFilter returns the per-transaction validation codes stored in
// block metadata by the committing peer (one byte per block entry). This is
// the same view fabric's internal/pkg/txflags.ValidationFlags provides.
func transactionsFilter(block *common.Block) []byte {
	metadata := block.GetMetadata().GetMetadata()
	if int(common.BlockMetadataIndex_TRANSACTIONS_FILTER) >= len(metadata) {
		return nil
	}
	return metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER]
}

func statusCode(filter []byte, txIndex int) peer.TxValidationCode {
	if txIndex >= len(filter) {
		// Mirrors the peer's gateway behavior for missing metadata.
		return peer.TxValidationCode_INVALID_OTHER_REASON
	}
	return peer.TxValidationCode(filter[txIndex])
}

func parseEnvelope(envelopeBytes []byte) (*Tx, bool, error) {
	envelope := &common.Envelope{}
	if err := proto.Unmarshal(envelopeBytes, envelope); err != nil {
		return nil, false, fmt.Errorf("unmarshal envelope: %w", err)
	}
	payload := &common.Payload{}
	if err := proto.Unmarshal(envelope.GetPayload(), payload); err != nil {
		return nil, false, fmt.Errorf("unmarshal payload: %w", err)
	}
	channelHeader := &common.ChannelHeader{}
	if err := proto.Unmarshal(payload.GetHeader().GetChannelHeader(), channelHeader); err != nil {
		return nil, false, fmt.Errorf("unmarshal channel header: %w", err)
	}
	if channelHeader.GetType() != int32(common.HeaderType_ENDORSER_TRANSACTION) {
		return nil, false, nil
	}

	tx := &Tx{
		TxID:      channelHeader.GetTxId(),
		ChannelID: channelHeader.GetChannelId(),
	}
	if ts := channelHeader.GetTimestamp(); ts != nil && ts.GetSeconds() > 0 {
		tx.TimestampUnix = uint64(ts.GetSeconds())
	}

	events, err := readChaincodeEvents(payload.GetData())
	if err != nil {
		return nil, false, err
	}
	tx.Events = events
	return tx, true, nil
}

// readChaincodeEvents walks Transaction -> ChaincodeActionPayload ->
// ProposalResponsePayload -> ChaincodeAction -> ChaincodeEvent. Individual
// undecodable actions are skipped, matching the peer's gateway behavior
// (they are not endorser chaincode actions).
func readChaincodeEvents(payloadData []byte) ([]Event, error) {
	transaction := &peer.Transaction{}
	if err := proto.Unmarshal(payloadData, transaction); err != nil {
		return nil, fmt.Errorf("unmarshal transaction: %w", err)
	}

	var events []Event
	for _, action := range transaction.GetActions() {
		actionPayload := &peer.ChaincodeActionPayload{}
		if err := proto.Unmarshal(action.GetPayload(), actionPayload); err != nil {
			continue
		}
		responsePayload := &peer.ProposalResponsePayload{}
		if err := proto.Unmarshal(actionPayload.GetAction().GetProposalResponsePayload(), responsePayload); err != nil {
			continue
		}
		chaincodeAction := &peer.ChaincodeAction{}
		if err := proto.Unmarshal(responsePayload.GetExtension(), chaincodeAction); err != nil {
			continue
		}
		event := &peer.ChaincodeEvent{}
		if err := proto.Unmarshal(chaincodeAction.GetEvents(), event); err != nil {
			continue
		}
		if event.GetChaincodeId() == "" || event.GetEventName() == "" {
			continue
		}
		events = append(events, Event{
			ChaincodeID: event.GetChaincodeId(),
			EventName:   event.GetEventName(),
			Payload:     event.GetPayload(),
		})
	}
	return events, nil
}
