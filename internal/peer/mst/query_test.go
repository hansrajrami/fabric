/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/core/scc/mstscc"
	"github.com/stretchr/testify/require"
)

const testTxID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func cannedFake(resp *pb.ProposalResponse) *fakeEndorser {
	return &fakeEndorser{handler: func(*pb.SignedProposal) (*pb.ProposalResponse, error) { return resp, nil }}
}

func TestStatusCommand(t *testing.T) {
	rec := mstscc.AnchorStatus{FabricTxID: testTxID, AnchorRef: "0x" + strings.Repeat("b", 64), Status: "CONFIRMED", RecordedAt: 1720000000}
	payload, _ := json.Marshal(rec)
	fake := cannedFake(okResponse(payload))

	out, err := runCmd(t, statusCmd(), fake, "-C", "mychannel", testTxID)
	require.NoError(t, err)
	require.Contains(t, out, testTxID)
	require.Contains(t, out, "CONFIRMED")
	require.Contains(t, out, rec.AnchorRef)

	// The proposal targeted mstscc.QueryAnchorStatus with the tx id.
	require.Len(t, fake.recorded, 1)
	spec := invokedSpec(t, fake.recorded[0])
	require.Equal(t, mstscc.Name, spec.ChaincodeSpec.ChaincodeId.Name)
	require.Equal(t, mstscc.QueryAnchorStatus, string(spec.ChaincodeSpec.Input.Args[0]))
	require.Equal(t, testTxID, string(spec.ChaincodeSpec.Input.Args[1]))
}

func TestStatusJSON(t *testing.T) {
	payload := []byte(`{"fabric_tx_id":"` + testTxID + `","status":"CONFIRMED"}`)
	out, err := runCmd(t, statusCmd(), cannedFake(okResponse(payload)), "-C", "mychannel", "--json", testTxID)
	require.NoError(t, err)
	require.JSONEq(t, string(payload), strings.TrimSpace(out))
}

func TestStatusMissingChannel(t *testing.T) {
	_, err := runCmd(t, statusCmd(), cannedFake(okResponse(nil)), testTxID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "channelID")
}

func TestStatusBadTxID(t *testing.T) {
	_, err := runCmd(t, statusCmd(), cannedFake(okResponse(nil)), "-C", "mychannel", "tooshort")
	require.Error(t, err)
	require.Contains(t, err.Error(), "64 hex")
}

func TestIsAnchoredCommand(t *testing.T) {
	out, err := runCmd(t, isAnchoredCmd(), cannedFake(okResponse([]byte("true"))), "-C", "mychannel", testTxID)
	require.NoError(t, err)
	require.Equal(t, "true", strings.TrimSpace(out))
}

func TestListCommand(t *testing.T) {
	recs := []mstscc.AnchorStatus{
		{FabricTxID: testTxID, Status: "CONFIRMED", AnchorRef: "0xaa", RecordedAt: 1},
		{FabricTxID: strings.Repeat("c", 64), Status: "CONFIRMED", AnchorRef: "0xbb", RecordedAt: 2},
	}
	payload, _ := json.Marshal(recs)
	out, err := runCmd(t, listCmd(), cannedFake(okResponse(payload)), "-C", "mychannel")
	require.NoError(t, err)
	require.Contains(t, out, testTxID)
	require.Contains(t, out, strings.Repeat("c", 64))
}

func TestListLimitPassedToSCC(t *testing.T) {
	fake := cannedFake(okResponse([]byte("[]")))
	_, err := runCmd(t, listCmd(), fake, "-C", "mychannel", "--limit", "5")
	require.NoError(t, err)
	spec := invokedSpec(t, fake.recorded[0])
	require.Equal(t, mstscc.ListAnchors, string(spec.ChaincodeSpec.Input.Args[0]))
	require.Equal(t, "5", string(spec.ChaincodeSpec.Input.Args[1]))
}

func TestListEmpty(t *testing.T) {
	out, err := runCmd(t, listCmd(), cannedFake(okResponse([]byte("[]"))), "-C", "mychannel")
	require.NoError(t, err)
	require.Contains(t, out, "no anchored transactions")
}

func TestCountCommand(t *testing.T) {
	out, err := runCmd(t, countCmd(), cannedFake(okResponse([]byte("7"))), "-C", "mychannel")
	require.NoError(t, err)
	require.Equal(t, "7", strings.TrimSpace(out))
	// count needs no txid arg and targets CountAnchors.
}

func TestQueryChaincodeErrorSurfaced(t *testing.T) {
	// Non-200 response from the SCC (e.g. channel not enabled) → command error.
	errResp := &pb.ProposalResponse{Response: &pb.Response{Status: 500, Message: "mstscc: MST anchoring is not enabled on channel mychannel"}}
	_, err := runCmd(t, statusCmd(), cannedFake(errResp), "-C", "mychannel", testTxID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not enabled")
}

func TestEndorserTransportError(t *testing.T) {
	fake := &fakeEndorser{handler: func(*pb.SignedProposal) (*pb.ProposalResponse, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}
	_, err := runCmd(t, statusCmd(), fake, "-C", "mychannel", testTxID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection refused")
}
