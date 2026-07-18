/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"bytes"
	"context"
	"sync"
	"testing"

	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/internal/peer/common"
	msptesttools "github.com/hyperledger/fabric/msp/mgmt/testtools"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

var mspOnce sync.Once

// initMSP loads the bundled test MSP once, so common.GetDefaultSigner() returns
// a real signing identity in tests without a core.yaml.
func initMSP(t *testing.T) {
	t.Helper()
	mspOnce.Do(func() {
		if err := msptesttools.LoadMSPSetupForTesting(); err != nil {
			panic("failed to load test MSP: " + err.Error())
		}
	})
}

// fakeEndorser is a recording pb.EndorserClient: it captures every proposal and
// returns whatever its handler decides (routed by chaincode/function so one
// fake can serve mstscc, qscc, and cscc calls).
type fakeEndorser struct {
	handler  func(*pb.SignedProposal) (*pb.ProposalResponse, error)
	recorded []*pb.SignedProposal
}

func (f *fakeEndorser) ProcessProposal(_ context.Context, in *pb.SignedProposal, _ ...grpc.CallOption) (*pb.ProposalResponse, error) {
	f.recorded = append(f.recorded, in)
	return f.handler(in)
}

// invokedSpec decodes a recorded signed proposal back to its
// ChaincodeInvocationSpec so tests can assert the chaincode/function/args sent.
func invokedSpec(t *testing.T, sp *pb.SignedProposal) *pb.ChaincodeInvocationSpec {
	t.Helper()
	proposal, err := protoutil.UnmarshalProposal(sp.ProposalBytes)
	require.NoError(t, err)
	ccPropPayload, err := protoutil.UnmarshalChaincodeProposalPayload(proposal.Payload)
	require.NoError(t, err)
	cis, err := protoutil.UnmarshalChaincodeInvocationSpec(ccPropPayload.Input)
	require.NoError(t, err)
	return cis
}

// okResponse builds a 200 proposal response carrying the given payload.
func okResponse(payload []byte) *pb.ProposalResponse {
	return &pb.ProposalResponse{
		Response:    &pb.Response{Status: 200, Payload: payload},
		Endorsement: &pb.Endorsement{},
	}
}

// runCmd wires the recording fake as the endorser, runs the command with the
// given args, captures stdout, and restores the overridden globals.
func runCmd(t *testing.T, cmd *cobra.Command, fake *fakeEndorser, args ...string) (string, error) {
	t.Helper()
	initMSP(t)

	savedEndorser := common.GetEndorserClientFnc
	t.Cleanup(func() {
		common.GetEndorserClientFnc = savedEndorser
		resetFlags()
	})
	common.GetEndorserClientFnc = func(string, string) (pb.EndorserClient, error) {
		return fake, nil
	}

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}
