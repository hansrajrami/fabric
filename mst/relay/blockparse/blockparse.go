// Package blockparse extracts the transaction facts the capture service
// needs from committed Fabric blocks delivered by the Fabric Gateway
// (fabric-protos-go-apiv2): tx id, channel, client-asserted timestamp,
// validity, and chaincode events.
//
// It is a self-contained port of the peer's own gateway event extraction
// (internal/pkg/gateway/event/{block,transaction}.go in fabric release-2.5).
// Importing github.com/hyperledger/fabric itself is deliberately avoided:
// its protoutil works on the old proto module while the fabric-gateway
// client delivers apiv2 blocks, and the module drags a very large dependency
// graph into the relayer. The output is the proto-free txmodel, shared with
// the in-peer embedded parser.
package blockparse

import (
	"fmt"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"google.golang.org/protobuf/proto"

	"github.com/hansrajrami/fabric/mst/relay/txmodel"
)

// Parse extracts all endorser transactions from a committed block. Non-endorser
// entries (config transactions etc.) are ignored. Unparseable envelopes are
// reported in Bad rather than failing the block.
func Parse(block *common.Block) (*txmodel.Block, error) {
	if block == nil {
		return nil, fmt.Errorf("blockparse: nil block")
	}
	out := &txmodel.Block{Number: block.GetHeader().GetNumber()}

	statusCodes := transactionsFilter(block)
	for i, envelopeBytes := range block.GetData().GetData() {
		tx, isEndorser, err := parseEnvelope(envelopeBytes)
		if err != nil {
			out.Bad = append(out.Bad, txmodel.BadEnvelope{Index: i, Err: err})
			continue
		}
		if !isEndorser {
			continue
		}
		tx.Valid = statusCode(statusCodes, i) == peer.TxValidationCode_VALID
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

func parseEnvelope(envelopeBytes []byte) (*txmodel.Tx, bool, error) {
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

	tx := &txmodel.Tx{
		TxID:      channelHeader.GetTxId(),
		ChannelID: channelHeader.GetChannelId(),
	}
	if ts := channelHeader.GetTimestamp(); ts != nil && ts.GetSeconds() > 0 {
		tx.TimestampUnix = uint64(ts.GetSeconds())
	}

	chaincodeID, events, err := readActions(payload.GetData())
	if err != nil {
		return nil, false, err
	}
	tx.ChaincodeID = chaincodeID
	tx.Events = events
	return tx, true, nil
}

// readActions walks Transaction -> ChaincodeActionPayload ->
// ProposalResponsePayload -> ChaincodeAction, collecting the invoked
// chaincode id (first action) and any chaincode events. Individual
// undecodable actions are skipped, matching the peer's gateway behavior
// (they are not endorser chaincode actions).
func readActions(payloadData []byte) (string, []txmodel.Event, error) {
	transaction := &peer.Transaction{}
	if err := proto.Unmarshal(payloadData, transaction); err != nil {
		return "", nil, fmt.Errorf("unmarshal transaction: %w", err)
	}

	var chaincodeID string
	var events []txmodel.Event
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
		if chaincodeID == "" {
			chaincodeID = chaincodeAction.GetChaincodeId().GetName()
		}
		event := &peer.ChaincodeEvent{}
		if err := proto.Unmarshal(chaincodeAction.GetEvents(), event); err != nil {
			continue
		}
		if event.GetChaincodeId() == "" || event.GetEventName() == "" {
			continue
		}
		events = append(events, txmodel.Event{
			ChaincodeID: event.GetChaincodeId(),
			EventName:   event.GetEventName(),
			Payload:     event.GetPayload(),
		})
	}
	return chaincodeID, events, nil
}
