/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"strings"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/internal/pkg/txflags"
	"github.com/stretchr/testify/require"
)

func testTxID(seed string) string {
	return strings.Repeat("0", 64-len(seed)) + seed
}

type txSpec struct {
	txID       string
	channel    string
	ts         int64
	valid      bool
	chaincode  string
	eventName  string
	payload    []byte
	headerType common.HeaderType
	corrupt    bool
}

func buildBlock(t *testing.T, number uint64, specs ...txSpec) *common.Block {
	t.Helper()
	data := make([][]byte, 0, len(specs))
	flags := txflags.New(len(specs))
	for i, spec := range specs {
		if spec.corrupt {
			data = append(data, []byte{0xFF, 0xFF, 0xFF, 0xFF})
			flags.SetFlag(i, peer.TxValidationCode_VALID)
			continue
		}
		headerType := spec.headerType
		if headerType == 0 {
			headerType = common.HeaderType_ENDORSER_TRANSACTION
		}
		ts, err := ptypes.TimestampProto(time.Unix(spec.ts, 0))
		require.NoError(t, err)
		channelHeader, err := proto.Marshal(&common.ChannelHeader{
			Type: int32(headerType), TxId: spec.txID, ChannelId: spec.channel, Timestamp: ts,
		})
		require.NoError(t, err)

		var chaincodeAction peer.ChaincodeAction
		if spec.chaincode != "" {
			chaincodeAction.ChaincodeId = &peer.ChaincodeID{Name: spec.chaincode}
		}
		if spec.eventName != "" {
			eventBytes, err := proto.Marshal(&peer.ChaincodeEvent{
				ChaincodeId: spec.chaincode, TxId: spec.txID, EventName: spec.eventName, Payload: spec.payload,
			})
			require.NoError(t, err)
			chaincodeAction.Events = eventBytes
		}
		extension, err := proto.Marshal(&chaincodeAction)
		require.NoError(t, err)
		responsePayload, err := proto.Marshal(&peer.ProposalResponsePayload{Extension: extension})
		require.NoError(t, err)
		actionPayload, err := proto.Marshal(&peer.ChaincodeActionPayload{
			Action: &peer.ChaincodeEndorsedAction{ProposalResponsePayload: responsePayload},
		})
		require.NoError(t, err)
		transaction, err := proto.Marshal(&peer.Transaction{
			Actions: []*peer.TransactionAction{{Payload: actionPayload}},
		})
		require.NoError(t, err)
		payload, err := proto.Marshal(&common.Payload{
			Header: &common.Header{ChannelHeader: channelHeader},
			Data:   transaction,
		})
		require.NoError(t, err)
		envelope, err := proto.Marshal(&common.Envelope{Payload: payload})
		require.NoError(t, err)
		data = append(data, envelope)

		code := peer.TxValidationCode_VALID
		if !spec.valid {
			code = peer.TxValidationCode_MVCC_READ_CONFLICT
		}
		flags.SetFlag(i, code)
	}

	metadata := make([][]byte, common.BlockMetadataIndex_TRANSACTIONS_FILTER+1)
	metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = flags
	return &common.Block{
		Header:   &common.BlockHeader{Number: number},
		Data:     &common.BlockData{Data: data},
		Metadata: &common.BlockMetadata{Metadata: metadata},
	}
}

func TestParseBlockExtractsEndorserTransactions(t *testing.T) {
	block := buildBlock(t, 7,
		txSpec{txID: testTxID("a1"), channel: "mychannel", ts: 1720000001, valid: true,
			chaincode: "mst-example", eventName: "MSTProofRequest", payload: []byte{0, 0, 0, 0}},
		txSpec{txID: testTxID("a2"), channel: "mychannel", ts: 1720000002, valid: false,
			chaincode: "mst-example", eventName: "MSTProofRequest", payload: []byte{0, 0, 0, 0}},
		txSpec{txID: testTxID("a3"), channel: "mychannel", ts: 1720000003, valid: true},
	)

	parsed, err := ParseBlock(block)
	require.NoError(t, err)
	require.Equal(t, uint64(7), parsed.Number)
	require.Len(t, parsed.Txs, 3)
	require.Empty(t, parsed.Bad)

	tx0 := parsed.Txs[0]
	require.Equal(t, testTxID("a1"), tx0.TxID)
	require.Equal(t, "mychannel", tx0.ChannelID)
	require.Equal(t, uint64(1720000001), tx0.TimestampUnix)
	require.True(t, tx0.Valid)
	require.Len(t, tx0.Events, 1)
	require.Equal(t, "MSTProofRequest", tx0.Events[0].EventName)
	require.Equal(t, "mst-example", tx0.Events[0].ChaincodeID)

	require.False(t, parsed.Txs[1].Valid)
	require.Empty(t, parsed.Txs[2].Events)
}

func TestParseBlockSurfacesInvokedChaincodeWithoutEvent(t *testing.T) {
	block := buildBlock(t, 9,
		txSpec{txID: testTxID("f1"), channel: "ch", ts: 1, valid: true, chaincode: "assets"},
	)
	parsed, err := ParseBlock(block)
	require.NoError(t, err)
	require.Equal(t, "assets", parsed.Txs[0].ChaincodeID)
	require.Empty(t, parsed.Txs[0].Events)
}

func TestParseBlockSkipsNonEndorserEntries(t *testing.T) {
	block := buildBlock(t, 1,
		txSpec{txID: testTxID("c1"), channel: "ch", ts: 1, valid: true, headerType: common.HeaderType_CONFIG},
		txSpec{txID: testTxID("c2"), channel: "ch", ts: 1, valid: true},
	)
	parsed, err := ParseBlock(block)
	require.NoError(t, err)
	require.Len(t, parsed.Txs, 1)
	require.Equal(t, testTxID("c2"), parsed.Txs[0].TxID)
}

func TestParseBlockReportsCorruptEnvelopes(t *testing.T) {
	block := buildBlock(t, 2,
		txSpec{corrupt: true},
		txSpec{txID: testTxID("d1"), channel: "ch", ts: 1, valid: true},
	)
	parsed, err := ParseBlock(block)
	require.NoError(t, err)
	require.Len(t, parsed.Bad, 1)
	require.Equal(t, 0, parsed.Bad[0].Index)
	require.Len(t, parsed.Txs, 1)
}

func TestParseBlockMissingFilterMetadataIsInvalid(t *testing.T) {
	block := buildBlock(t, 3, txSpec{txID: testTxID("e1"), channel: "ch", ts: 1, valid: true})
	block.Metadata = &common.BlockMetadata{}
	parsed, err := ParseBlock(block)
	require.NoError(t, err)
	require.False(t, parsed.Txs[0].Valid, "missing metadata must not read as VALID")
}

func TestParseBlockNil(t *testing.T) {
	_, err := ParseBlock(nil)
	require.Error(t, err)
}
