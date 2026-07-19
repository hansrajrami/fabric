/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package channelconfig

import (
	"sort"

	"github.com/golang/protobuf/proto"
	cb "github.com/hyperledger/fabric-protos-go/common"
	mspprotos "github.com/hyperledger/fabric-protos-go/msp"
	"github.com/hyperledger/fabric/msp"
)

// ApplicationOrgsMissingPeerNodeOUs inspects the raw channel config and returns
// the names of Application-group organizations whose MSP does not have NodeOUs
// peer classification enabled — i.e. FabricNodeOus is absent/disabled, or no
// peer OU identifier is configured. It also returns the total number of
// application orgs inspected.
//
// This is a detection helper for the MST write-back gate: mstscc authorizes
// only MSPRole_PEER identities, which requires the submitting org's MSP to have
// NodeOUs peer classification. An org listed here cannot have its peer node
// identity recognized, so write-backs it signs are rejected. The flag is not
// exposed by the msp.MSP interface (it is the unexported bccspmsp.ouEnforcement,
// and MSPs are cache-wrapped), so this re-parses the raw MSPConfig — the only
// externally reachable signal.
//
// Non-FABRIC MSPs (e.g. idemix) are skipped: the peer-role gate is a FABRIC-MSP
// concept. Orgs whose MSP value cannot be parsed are conservatively reported as
// missing, since peer classification cannot be confirmed for them.
func ApplicationOrgsMissingPeerNodeOUs(config *cb.Config) (missing []string, total int) {
	appGroup, ok := config.GetChannelGroup().GetGroups()[ApplicationGroupKey]
	if !ok {
		return nil, 0
	}
	for orgName, orgGroup := range appGroup.GetGroups() {
		mspValue, ok := orgGroup.GetValues()[MSPKey]
		if !ok {
			continue // no MSP for this org group; nothing to classify
		}
		total++
		mspConfig := &mspprotos.MSPConfig{}
		if err := proto.Unmarshal(mspValue.GetValue(), mspConfig); err != nil {
			missing = append(missing, orgName)
			continue
		}
		if mspConfig.GetType() != int32(msp.FABRIC) {
			// Non-FABRIC providers do not participate in the peer-role gate;
			// do not count them against the total.
			total--
			continue
		}
		fabricConfig := &mspprotos.FabricMSPConfig{}
		if err := proto.Unmarshal(mspConfig.GetConfig(), fabricConfig); err != nil {
			missing = append(missing, orgName)
			continue
		}
		nodeOUs := fabricConfig.GetFabricNodeOus()
		if !nodeOUs.GetEnable() || nodeOUs.GetPeerOuIdentifier() == nil {
			missing = append(missing, orgName)
		}
	}
	sort.Strings(missing)
	return missing, total
}
