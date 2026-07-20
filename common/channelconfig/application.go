/*
Copyright IBM Corp. 2017 All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package channelconfig

import (
	cb "github.com/hyperledger/fabric-protos-go/common"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/common/capabilities"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// ApplicationGroupKey is the group name for the Application config
	ApplicationGroupKey = "Application"

	// ACLsKey is the name of the ACLs config
	ACLsKey = "ACLs"
)

// ApplicationProtos is used as the source of the ApplicationConfig. The
// MSTAnchor field carries the channel's MST anchoring configuration as an
// opaque JSON payload inside a wrapperspb.StringValue; the field name doubles as
// the config value key (see MSTAnchorKey). A well-known wrapper is used rather
// than a bespoke fabric-protos message so the fork stays additive, and a
// StringValue (not structpb.Value) so configtxlator's protolator can render it.
type ApplicationProtos struct {
	ACLs         *pb.ACLs
	Capabilities *cb.Capabilities
	MSTAnchor    *wrapperspb.StringValue
}

// ApplicationConfig implements the Application interface
type ApplicationConfig struct {
	applicationOrgs map[string]ApplicationOrg
	protos          *ApplicationProtos
	mstAnchor       *MSTAnchorConfig
}

// NewApplicationConfig creates config from an Application config group
func NewApplicationConfig(appGroup *cb.ConfigGroup, mspConfig *MSPConfigHandler) (*ApplicationConfig, error) {
	ac := &ApplicationConfig{
		applicationOrgs: make(map[string]ApplicationOrg),
		protos:          &ApplicationProtos{},
	}

	if err := DeserializeProtoValuesFromGroup(appGroup, ac.protos); err != nil {
		return nil, errors.Wrap(err, "failed to deserialize values")
	}

	if !ac.Capabilities().ACLs() {
		if _, ok := appGroup.Values[ACLsKey]; ok {
			return nil, errors.New("ACLs may not be specified without the required capability")
		}
	}

	// MST anchoring config is optional AND capability-gated: the MSTAnchor value
	// may appear only when the channel enables the MSTAnchor application
	// capability, which every participating node must support. This makes the
	// "all nodes run the MST-enabled binary" prerequisite an explicit capability
	// gate — a vanilla binary refuses the channel via Capabilities().Supported()
	// rather than silently failing to endorse the write-back. A present value
	// without the capability is a misconfiguration and is rejected here.
	if raw := ac.protos.MSTAnchor.GetValue(); raw != "" {
		if !ac.Capabilities().MSTAnchor() {
			return nil, errors.Errorf("the %s config value may not be specified without the %s application capability", MSTAnchorKey, capabilities.ApplicationMSTAnchor)
		}
		mstAnchor, err := unmarshalMSTAnchorConfig([]byte(raw))
		if err != nil {
			return nil, err
		}
		ac.mstAnchor = mstAnchor
	}

	for orgName, orgGroup := range appGroup.Groups {
		orgConfig, err := NewApplicationOrgConfig(orgName, orgGroup, mspConfig)
		if err != nil {
			return nil, err
		}
		ac.applicationOrgs[orgName] = orgConfig
	}

	return ac, nil
}

// Organizations returns a map of org ID to ApplicationOrg
func (ac *ApplicationConfig) Organizations() map[string]ApplicationOrg {
	return ac.applicationOrgs
}

// Capabilities returns a map of capability name to Capability
func (ac *ApplicationConfig) Capabilities() ApplicationCapabilities {
	return capabilities.NewApplicationProvider(ac.protos.Capabilities.Capabilities)
}

// APIPolicyMapper returns a PolicyMapper that maps API names to policies
func (ac *ApplicationConfig) APIPolicyMapper() PolicyMapper {
	pm := newAPIsProvider(ac.protos.ACLs.Acls)

	return pm
}

// MSTAnchorConfig returns the channel's MST anchoring configuration and
// whether it is present. It is absent (false) on channels that never set the
// MSTAnchor config value.
func (ac *ApplicationConfig) MSTAnchorConfig() (*MSTAnchorConfig, bool) {
	if ac.mstAnchor == nil {
		return nil, false
	}
	return ac.mstAnchor, true
}
