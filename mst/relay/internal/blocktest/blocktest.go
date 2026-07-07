// Package blocktest builds synthetic committed Fabric blocks (apiv2 protos)
// so the parsing and capture pipeline is fully testable without a running
// Fabric network.
package blocktest

import (
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TxSpec describes one transaction to place in a synthetic block.
type TxSpec struct {
	TxID      string
	ChannelID string
	Timestamp int64
	Valid     bool
	// Event fields; EventName == "" means the tx emits no event.
	ChaincodeID  string
	EventName    string
	EventPayload []byte
	// HeaderType overrides the channel header type (defaults to endorser).
	HeaderType int32
	// CorruptEnvelope replaces the whole envelope with garbage bytes.
	CorruptEnvelope bool
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %T: %v", m, err)
	}
	return raw
}

// Build assembles a committed block containing the given transactions, with
// the TRANSACTIONS_FILTER metadata reflecting each spec's Valid flag.
func Build(t *testing.T, number uint64, specs ...TxSpec) *common.Block {
	t.Helper()

	data := make([][]byte, 0, len(specs))
	filter := make([]byte, len(specs))
	for i, spec := range specs {
		if spec.CorruptEnvelope {
			data = append(data, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
			filter[i] = byte(peer.TxValidationCode_VALID)
			continue
		}

		headerType := spec.HeaderType
		if headerType == 0 {
			headerType = int32(common.HeaderType_ENDORSER_TRANSACTION)
		}
		channelHeader := &common.ChannelHeader{
			Type:      headerType,
			TxId:      spec.TxID,
			ChannelId: spec.ChannelID,
			Timestamp: timestamppb.New(time.Unix(spec.Timestamp, 0)),
		}

		var event *peer.ChaincodeEvent
		if spec.EventName != "" {
			event = &peer.ChaincodeEvent{
				ChaincodeId: spec.ChaincodeID,
				TxId:        spec.TxID,
				EventName:   spec.EventName,
				Payload:     spec.EventPayload,
			}
		}
		chaincodeAction := &peer.ChaincodeAction{}
		if spec.ChaincodeID != "" {
			chaincodeAction.ChaincodeId = &peer.ChaincodeID{Name: spec.ChaincodeID}
		}
		if event != nil {
			chaincodeAction.Events = mustMarshal(t, event)
		}
		responsePayload := &peer.ProposalResponsePayload{
			Extension: mustMarshal(t, chaincodeAction),
		}
		actionPayload := &peer.ChaincodeActionPayload{
			Action: &peer.ChaincodeEndorsedAction{
				ProposalResponsePayload: mustMarshal(t, responsePayload),
			},
		}
		transaction := &peer.Transaction{
			Actions: []*peer.TransactionAction{{Payload: mustMarshal(t, actionPayload)}},
		}
		payload := &common.Payload{
			Header: &common.Header{ChannelHeader: mustMarshal(t, channelHeader)},
			Data:   mustMarshal(t, transaction),
		}
		envelope := &common.Envelope{Payload: mustMarshal(t, payload)}
		data = append(data, mustMarshal(t, envelope))

		code := peer.TxValidationCode_VALID
		if !spec.Valid {
			code = peer.TxValidationCode_MVCC_READ_CONFLICT
		}
		filter[i] = byte(code)
	}

	metadata := make([][]byte, common.BlockMetadataIndex_TRANSACTIONS_FILTER+1)
	metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = filter

	return &common.Block{
		Header:   &common.BlockHeader{Number: number},
		Data:     &common.BlockData{Data: data},
		Metadata: &common.BlockMetadata{Metadata: metadata},
	}
}
