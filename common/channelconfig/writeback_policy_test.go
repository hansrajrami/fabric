/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package channelconfig

import (
	"testing"

	"github.com/golang/protobuf/proto"
	cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/common/policydsl"
	"github.com/stretchr/testify/require"
)

// orgGroupWithWriters builds an application-org config group carrying an MSP
// value (so it is counted) and a Writers policy from the given policy DSL
// string. A nil dsl pointer means "no Writers policy".
func orgGroupWithWriters(t *testing.T, dsl string, withMSP bool) *cb.ConfigGroup {
	t.Helper()
	g := &cb.ConfigGroup{
		Values:   map[string]*cb.ConfigValue{},
		Policies: map[string]*cb.ConfigPolicy{},
	}
	if withMSP {
		g.Values[MSPKey] = &cb.ConfigValue{Value: []byte("msp")}
	}
	if dsl != "" {
		env, err := policydsl.FromString(dsl)
		require.NoError(t, err)
		polBytes, err := proto.Marshal(env)
		require.NoError(t, err)
		g.Policies[WritersPolicyKey] = &cb.ConfigPolicy{
			Policy: &cb.Policy{Type: int32(cb.Policy_SIGNATURE), Value: polBytes},
		}
	}
	return g
}

func configWithAppOrgs(orgs map[string]*cb.ConfigGroup) *cb.Config {
	return &cb.Config{ChannelGroup: &cb.ConfigGroup{
		Groups: map[string]*cb.ConfigGroup{
			ApplicationGroupKey: {Groups: orgs},
		},
	}}
}

func TestApplicationOrgsWritersRejectingPeer(t *testing.T) {
	// Default NodeOUs Writers (admin OR client) excludes peer → rejecting.
	// Adding peer, or using member, admits it → not rejecting.
	config := configWithAppOrgs(map[string]*cb.ConfigGroup{
		"Org1MSP": orgGroupWithWriters(t, "OR('Org1MSP.admin','Org1MSP.client')", true),
		"Org2MSP": orgGroupWithWriters(t, "OR('Org2MSP.admin','Org2MSP.client','Org2MSP.peer')", true),
		"Org3MSP": orgGroupWithWriters(t, "OR('Org3MSP.member')", true),
		"Org4MSP": orgGroupWithWriters(t, "OR('Org4MSP.peer')", true),
	})
	rejecting, total := ApplicationOrgsWritersRejectingPeer(config)
	require.Equal(t, 4, total)
	require.Equal(t, []string{"Org1MSP"}, rejecting, "only the admin/client-only org rejects peer")
}

func TestApplicationOrgsWritersRejectingPeerConservativeCases(t *testing.T) {
	// Missing Writers policy, and a non-signature (ImplicitMeta-style) policy,
	// are both conservatively reported as rejecting since peer admission cannot
	// be confirmed statically.
	noWriters := orgGroupWithWriters(t, "", true)
	nonSig := orgGroupWithWriters(t, "OR('OrgXMSP.peer')", true)
	nonSig.Policies[WritersPolicyKey].Policy.Type = int32(cb.Policy_IMPLICIT_META)

	config := configWithAppOrgs(map[string]*cb.ConfigGroup{
		"OrgNoWriters": noWriters,
		"OrgNonSig":    nonSig,
	})
	rejecting, total := ApplicationOrgsWritersRejectingPeer(config)
	require.Equal(t, 2, total)
	require.ElementsMatch(t, []string{"OrgNoWriters", "OrgNonSig"}, rejecting)
}

func TestApplicationOrgsWritersRejectingPeerSkipsNonOrgGroups(t *testing.T) {
	// A group without an MSP value is not an application org and is not counted.
	config := configWithAppOrgs(map[string]*cb.ConfigGroup{
		"Org1MSP":  orgGroupWithWriters(t, "OR('Org1MSP.peer')", true),
		"NotAnOrg": orgGroupWithWriters(t, "OR('X.admin')", false),
	})
	rejecting, total := ApplicationOrgsWritersRejectingPeer(config)
	require.Equal(t, 1, total)
	require.Empty(t, rejecting)
}

func TestApplicationOrgsWritersRejectingPeerNoAppGroup(t *testing.T) {
	rejecting, total := ApplicationOrgsWritersRejectingPeer(&cb.Config{ChannelGroup: &cb.ConfigGroup{}})
	require.Zero(t, total)
	require.Empty(t, rejecting)
}
