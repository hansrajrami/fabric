/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package channelconfig

import (
	"testing"

	"github.com/golang/protobuf/proto"
	cb "github.com/hyperledger/fabric-protos-go/common"
	mspprotos "github.com/hyperledger/fabric-protos-go/msp"
	"github.com/hyperledger/fabric/msp"
	"github.com/stretchr/testify/require"
)

// mspConfigValue marshals an MSPConfig into an org group's MSP config value.
func mspConfigValue(t *testing.T, mspType msp.ProviderType, inner []byte) *cb.ConfigValue {
	t.Helper()
	raw, err := proto.Marshal(&mspprotos.MSPConfig{Type: int32(mspType), Config: inner})
	require.NoError(t, err)
	return &cb.ConfigValue{Value: raw}
}

// fabricMSPValue builds an org group's MSP config value for a FABRIC MSP with
// the given NodeOUs settings.
func fabricMSPValue(t *testing.T, mspID string, nodeOUsEnabled, withPeerOU bool) *cb.ConfigValue {
	t.Helper()
	fabricCfg := &mspprotos.FabricMSPConfig{Name: mspID}
	if nodeOUsEnabled || withPeerOU {
		fno := &mspprotos.FabricNodeOUs{Enable: nodeOUsEnabled}
		if withPeerOU {
			fno.PeerOuIdentifier = &mspprotos.FabricOUIdentifier{OrganizationalUnitIdentifier: "peer"}
		}
		fabricCfg.FabricNodeOus = fno
	}
	inner, err := proto.Marshal(fabricCfg)
	require.NoError(t, err)
	return mspConfigValue(t, msp.FABRIC, inner)
}

// configWithOrgs assembles a cb.Config whose Application group has the given
// orgs, each mapped to its MSP config value (nil = an org group with no MSP).
func configWithOrgs(orgs map[string]*cb.ConfigValue) *cb.Config {
	appGroup := &cb.ConfigGroup{Groups: map[string]*cb.ConfigGroup{}}
	for name, mspVal := range orgs {
		g := &cb.ConfigGroup{Values: map[string]*cb.ConfigValue{}}
		if mspVal != nil {
			g.Values[MSPKey] = mspVal
		}
		appGroup.Groups[name] = g
	}
	return &cb.Config{ChannelGroup: &cb.ConfigGroup{
		Groups: map[string]*cb.ConfigGroup{ApplicationGroupKey: appGroup},
	}}
}

func TestNodeOUsAllEnabled(t *testing.T) {
	cfg := configWithOrgs(map[string]*cb.ConfigValue{
		"Org1": fabricMSPValue(t, "Org1MSP", true, true),
		"Org2": fabricMSPValue(t, "Org2MSP", true, true),
	})
	missing, total := ApplicationOrgsMissingPeerNodeOUs(cfg)
	require.Equal(t, 2, total)
	require.Empty(t, missing)
}

func TestNodeOUsSomeMissing(t *testing.T) {
	cfg := configWithOrgs(map[string]*cb.ConfigValue{
		"Org1": fabricMSPValue(t, "Org1MSP", true, true),   // ok
		"Org2": fabricMSPValue(t, "Org2MSP", false, false), // disabled
		"Org3": fabricMSPValue(t, "Org3MSP", true, false),  // enabled but no peer OU
	})
	missing, total := ApplicationOrgsMissingPeerNodeOUs(cfg)
	require.Equal(t, 3, total)
	require.Equal(t, []string{"Org2", "Org3"}, missing) // sorted
}

func TestNodeOUsAllMissing(t *testing.T) {
	cfg := configWithOrgs(map[string]*cb.ConfigValue{
		"Org1": fabricMSPValue(t, "Org1MSP", false, false),
	})
	missing, total := ApplicationOrgsMissingPeerNodeOUs(cfg)
	require.Equal(t, 1, total)
	require.Equal(t, []string{"Org1"}, missing)
}

func TestNodeOUsNonFabricSkipped(t *testing.T) {
	// A non-FABRIC (e.g. idemix) MSP is not counted or flagged.
	idemix := mspConfigValue(t, msp.IDEMIX, []byte{})
	cfg := configWithOrgs(map[string]*cb.ConfigValue{
		"Org1":   fabricMSPValue(t, "Org1MSP", true, true),
		"IdxOrg": idemix,
	})
	missing, total := ApplicationOrgsMissingPeerNodeOUs(cfg)
	require.Equal(t, 1, total, "only the FABRIC org is counted")
	require.Empty(t, missing)
}

func TestNodeOUsNoApplicationGroup(t *testing.T) {
	missing, total := ApplicationOrgsMissingPeerNodeOUs(&cb.Config{ChannelGroup: &cb.ConfigGroup{}})
	require.Zero(t, total)
	require.Empty(t, missing)
}

func TestNodeOUsUnparseableMSPReportedMissing(t *testing.T) {
	cfg := configWithOrgs(map[string]*cb.ConfigValue{
		"BadOrg": {Value: []byte("not a proto")},
	})
	missing, total := ApplicationOrgsMissingPeerNodeOUs(cfg)
	require.Equal(t, 1, total)
	require.Equal(t, []string{"BadOrg"}, missing)
}
