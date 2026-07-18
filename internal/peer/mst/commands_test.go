/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"testing"

	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/core/scc/cscc"
	"github.com/stretchr/testify/require"
)

// configFetchFails returns a fake whose cscc GetChannelConfig query fails, so a
// command that needs the channel's MST config errors out after we can still
// assert the proposal it sent.
func configFetchFails() *fakeEndorser {
	return cannedFake(&pb.ProposalResponse{Response: &pb.Response{Status: 500, Message: "no such channel"}})
}

func assertCSCCConfigProposal(t *testing.T, fake *fakeEndorser, channel string) {
	t.Helper()
	require.NotEmpty(t, fake.recorded)
	spec := invokedSpec(t, fake.recorded[0])
	require.Equal(t, "cscc", spec.ChaincodeSpec.ChaincodeId.Name)
	require.Equal(t, cscc.GetChannelConfig, string(spec.ChaincodeSpec.Input.Args[0]))
	require.Equal(t, channel, string(spec.ChaincodeSpec.Input.Args[1]))
}

func TestChannelConfigProposalAndError(t *testing.T) {
	fake := configFetchFails()
	_, err := runCmd(t, channelConfigCmd(nil), fake, "-C", "mychannel")
	require.Error(t, err)
	assertCSCCConfigProposal(t, fake, "mychannel")
}

func TestOnchainProposalAndError(t *testing.T) {
	fake := configFetchFails()
	_, err := runCmd(t, onchainCmd(nil), fake, "-C", "mychannel", testTxID)
	require.Error(t, err)
	assertCSCCConfigProposal(t, fake, "mychannel")
}

func TestRelayerBadSubcommand(t *testing.T) {
	_, err := runCmd(t, relayerCmd(nil), cannedFake(okResponse(nil)), "sideways", "0xabc", "-C", "mychannel")
	require.Error(t, err)
	require.Contains(t, err.Error(), "add")
}

func TestRelayerNoOwnerKey(t *testing.T) {
	t.Setenv("MST_OWNER_KEY", "")
	t.Setenv("MST_RELAYER_KEY", "")
	_, err := runCmd(t, relayerCmd(nil), cannedFake(okResponse(nil)), "add", "0xabc", "-C", "mychannel")
	require.Error(t, err)
	require.Contains(t, err.Error(), "owner key")
}
