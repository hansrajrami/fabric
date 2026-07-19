/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"testing"

	"github.com/golang/protobuf/proto"
	cb "github.com/hyperledger/fabric-protos-go/common"
	mspprotos "github.com/hyperledger/fabric-protos-go/msp"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/hyperledger/fabric/common/configtx"
	"github.com/hyperledger/fabric/msp"
	"github.com/stretchr/testify/require"
)

// fakeResources / fakeValidator / fakePeer embed the real interfaces so only the
// methods actually exercised by warnIfWritebackOrgLacksNodeOUs need bodies; any
// other call would nil-panic (and none happens on this path).
type fakeValidator struct {
	configtx.Validator
	cfg *cb.Config
}

func (f fakeValidator) ConfigProto() *cb.Config { return f.cfg }

type fakeResources struct {
	channelconfig.Resources
	cfg *cb.Config
}

func (f fakeResources) ConfigtxValidator() configtx.Validator { return fakeValidator{cfg: f.cfg} }

type fakePeer struct {
	PeerLedgers
	res channelconfig.Resources
}

func (f fakePeer) GetChannelConfig(string) channelconfig.Resources { return f.res }

// orgConfigMissingNodeOUs builds a channel config whose single application org
// has a FABRIC MSP with NodeOUs disabled.
func orgConfigMissingNodeOUs(t *testing.T, orgName string) *cb.Config {
	t.Helper()
	inner, err := proto.Marshal(&mspprotos.FabricMSPConfig{Name: orgName + "MSP"}) // no FabricNodeOus
	require.NoError(t, err)
	mspVal, err := proto.Marshal(&mspprotos.MSPConfig{Type: int32(msp.FABRIC), Config: inner})
	require.NoError(t, err)
	return &cb.Config{ChannelGroup: &cb.ConfigGroup{Groups: map[string]*cb.ConfigGroup{
		channelconfig.ApplicationGroupKey: {Groups: map[string]*cb.ConfigGroup{
			orgName: {Values: map[string]*cb.ConfigValue{"MSP": {Value: mspVal}}},
		}},
	}}}
}

func newServiceForNodeOUsTest(peer PeerLedgers, writebackMSPID string) *Service {
	cfg := &Config{}
	cfg.WriteBack.MSPID = writebackMSPID
	return &Service{cfg: cfg, peer: peer, warnedNodeOUs: map[string]struct{}{}}
}

func TestWarnNodeOUsFiresWhenMissing(t *testing.T) {
	peer := fakePeer{res: fakeResources{cfg: orgConfigMissingNodeOUs(t, "Org1")}}
	s := newServiceForNodeOUsTest(peer, "Org1MSP")

	s.warnIfWritebackOrgLacksNodeOUs("ch")
	_, warned := s.warnedNodeOUs["ch"]
	require.True(t, warned, "a missing-NodeOUs org must trigger (and dedupe) the warning")

	// Idempotent: a second pass does not re-fire (dedup set stays put).
	s.warnIfWritebackOrgLacksNodeOUs("ch")
	require.Len(t, s.warnedNodeOUs, 1)
}

func TestWarnNodeOUsSilentWhenPresent(t *testing.T) {
	inner, err := proto.Marshal(&mspprotos.FabricMSPConfig{
		Name: "Org1MSP",
		FabricNodeOus: &mspprotos.FabricNodeOUs{
			Enable:           true,
			PeerOuIdentifier: &mspprotos.FabricOUIdentifier{OrganizationalUnitIdentifier: "peer"},
		},
	})
	require.NoError(t, err)
	mspVal, err := proto.Marshal(&mspprotos.MSPConfig{Type: int32(msp.FABRIC), Config: inner})
	require.NoError(t, err)
	config := &cb.Config{ChannelGroup: &cb.ConfigGroup{Groups: map[string]*cb.ConfigGroup{
		channelconfig.ApplicationGroupKey: {Groups: map[string]*cb.ConfigGroup{
			"Org1": {Values: map[string]*cb.ConfigValue{"MSP": {Value: mspVal}}},
		}},
	}}}

	s := newServiceForNodeOUsTest(fakePeer{res: fakeResources{cfg: config}}, "Org1MSP")
	s.warnIfWritebackOrgLacksNodeOUs("ch")
	require.Empty(t, s.warnedNodeOUs, "no warning when the org has NodeOUs peer classification")
}

func TestWarnNodeOUsSilentWhenNoChannelConfig(t *testing.T) {
	s := newServiceForNodeOUsTest(fakePeer{res: nil}, "Org1MSP")
	s.warnIfWritebackOrgLacksNodeOUs("ch")
	require.Empty(t, s.warnedNodeOUs, "no channel config -> no warning, no panic")
}
