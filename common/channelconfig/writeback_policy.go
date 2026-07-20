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
)

// ApplicationOrgsWritersRejectingPeer inspects the raw channel config and
// returns the names of Application-group organizations whose Writers policy
// would NOT admit a peer-role identity, along with the total number of orgs
// inspected.
//
// This is a detection helper for a subtle MST write-back failure. The mstscc
// RecordAnchor gate authorizes only MSPRole_PEER identities, so the relayer
// signs the write-back transaction with a peer-role identity. But the orderer
// independently evaluates that transaction against the channel Writers policy,
// and the default NodeOUs Writers is OR('Org.admin','Org.client') — which
// excludes the peer role. The result is that endorsement succeeds but the
// orderer rejects the broadcast with FORBIDDEN ("requires 1 of the 'Writers'
// sub-policies"). To let peer-role write-backs through, the anchoring org's
// Writers must admit the peer role, e.g. OR('Org.admin','Org.client','Org.peer')
// or OR('Org.member').
//
// The assessment is a best-effort static analysis of the org's Writers
// signature policy: a peer-role identity can satisfy it only if the policy
// references a principal a peer satisfies — an MSPRole PEER, or MEMBER (which
// admits every role, peer included). An org whose Writers is missing,
// non-signature (e.g. ImplicitMeta), unparseable, or references no such
// principal is conservatively reported as rejecting, since peer admission
// cannot be confirmed. It does not fully evaluate composite AND rules; a policy
// that names a peer principal only inside an AND is reported as admitting.
func ApplicationOrgsWritersRejectingPeer(config *cb.Config) (rejecting []string, total int) {
	appGroup, ok := config.GetChannelGroup().GetGroups()[ApplicationGroupKey]
	if !ok {
		return nil, 0
	}
	for orgName, orgGroup := range appGroup.GetGroups() {
		// Only application orgs carry an MSP; skip anything that does not (keeps
		// the total consistent with the NodeOUs helper).
		if _, ok := orgGroup.GetValues()[MSPKey]; !ok {
			continue
		}
		total++
		if !writersAdmitsPeer(orgGroup) {
			rejecting = append(rejecting, orgName)
		}
	}
	sort.Strings(rejecting)
	return rejecting, total
}

// writersAdmitsPeer reports whether the org group's Writers signature policy
// references a principal a peer-role identity satisfies (an MSPRole PEER or
// MEMBER). It returns false when the policy is absent, not a signature policy,
// unparseable, or names no such principal.
func writersAdmitsPeer(orgGroup *cb.ConfigGroup) bool {
	cp, ok := orgGroup.GetPolicies()[WritersPolicyKey]
	if !ok {
		return false
	}
	pol := cp.GetPolicy()
	if pol.GetType() != int32(cb.Policy_SIGNATURE) {
		return false
	}
	env := &cb.SignaturePolicyEnvelope{}
	if err := proto.Unmarshal(pol.GetValue(), env); err != nil {
		return false
	}
	for _, principal := range env.GetIdentities() {
		if principal.GetPrincipalClassification() != mspprotos.MSPPrincipal_ROLE {
			continue
		}
		role := &mspprotos.MSPRole{}
		if err := proto.Unmarshal(principal.GetPrincipal(), role); err != nil {
			continue
		}
		switch role.GetRole() {
		case mspprotos.MSPRole_PEER, mspprotos.MSPRole_MEMBER:
			return true
		}
	}
	return false
}
