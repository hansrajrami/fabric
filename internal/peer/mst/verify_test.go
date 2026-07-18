/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/golang/protobuf/ptypes"
	cb "github.com/hyperledger/fabric-protos-go/common"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/internal/peer/common"
	"github.com/hyperledger/fabric/internal/pkg/txflags"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/stretchr/testify/require"
)

// buildTxBlock builds a marshaled block containing one valid endorser tx with
// the given chaincode and (optionally) an MSTProofRequest event, mirroring the
// on-ledger shape that mstanchor.ParseBlock consumes.
func buildTxBlock(t *testing.T, number uint64, txID, channel, chaincode, eventName string, payload []byte) []byte {
	t.Helper()
	ts, err := ptypes.TimestampProto(time.Unix(1720000001, 0))
	require.NoError(t, err)
	channelHeader := protoutil.MarshalOrPanic(&cb.ChannelHeader{
		Type: int32(cb.HeaderType_ENDORSER_TRANSACTION), TxId: txID, ChannelId: channel, Timestamp: ts,
	})
	action := &pb.ChaincodeAction{ChaincodeId: &pb.ChaincodeID{Name: chaincode}}
	if eventName != "" {
		action.Events = protoutil.MarshalOrPanic(&pb.ChaincodeEvent{
			ChaincodeId: chaincode, TxId: txID, EventName: eventName, Payload: payload,
		})
	}
	respPayload := protoutil.MarshalOrPanic(&pb.ProposalResponsePayload{Extension: protoutil.MarshalOrPanic(action)})
	actionPayload := protoutil.MarshalOrPanic(&pb.ChaincodeActionPayload{
		Action: &pb.ChaincodeEndorsedAction{ProposalResponsePayload: respPayload},
	})
	txBytes := protoutil.MarshalOrPanic(&pb.Transaction{Actions: []*pb.TransactionAction{{Payload: actionPayload}}})
	env := protoutil.MarshalOrPanic(&cb.Envelope{Payload: protoutil.MarshalOrPanic(&cb.Payload{
		Header: &cb.Header{ChannelHeader: channelHeader}, Data: txBytes,
	})})

	flags := txflags.New(1)
	flags.SetFlag(0, pb.TxValidationCode_VALID)
	metadata := make([][]byte, cb.BlockMetadataIndex_TRANSACTIONS_FILTER+1)
	metadata[cb.BlockMetadataIndex_TRANSACTIONS_FILTER] = flags
	return protoutil.MarshalOrPanic(&cb.Block{
		Header:   &cb.BlockHeader{Number: number},
		Data:     &cb.BlockData{Data: [][]byte{env}},
		Metadata: &cb.BlockMetadata{Metadata: metadata},
	})
}

func testClients(t *testing.T, blockBytes []byte) *clients {
	t.Helper()
	initMSP(t)
	signer, err := common.GetDefaultSigner()
	require.NoError(t, err)
	fake := cannedFake(okResponse(blockBytes))
	return &clients{signer: signer, endorser: fake}
}

func TestBuildVerifyInputOptedIn(t *testing.T) {
	t.Cleanup(resetFlags)
	channelID = "mychannel"
	payload := []byte{0, 0, 0, 0}
	block := buildTxBlock(t, 7, testTxID, "mychannel", "mst-example", "MSTProofRequest", payload)

	in, err := testClients(t, block).buildVerifyInput(testTxID)
	require.NoError(t, err)
	require.Equal(t, testTxID, in.FabricTxID)
	require.Equal(t, "mychannel", in.ChannelID)
	require.Equal(t, "mst-example", in.ChaincodeID)
	require.Equal(t, uint64(7), in.BlockNumber)
	require.Equal(t, uint64(1720000001), in.Timestamp)
	require.Equal(t, hex.EncodeToString(payload), in.PayloadCanonicalHex)
}

func TestBuildVerifyInputAnchorAll(t *testing.T) {
	t.Cleanup(resetFlags)
	channelID = "mychannel"
	// No event → anchor-all path → empty canonical payload.
	block := buildTxBlock(t, 3, testTxID, "mychannel", "assets", "", nil)

	in, err := testClients(t, block).buildVerifyInput(testTxID)
	require.NoError(t, err)
	require.Equal(t, "assets", in.ChaincodeID)
	require.Equal(t, emptyPayloadHex, in.PayloadCanonicalHex)
}

func TestBuildVerifyInputTxNotInBlock(t *testing.T) {
	t.Cleanup(resetFlags)
	channelID = "mychannel"
	other := "ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	block := buildTxBlock(t, 1, other, "mychannel", "assets", "", nil)

	_, err := testClients(t, block).buildVerifyInput(testTxID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}
