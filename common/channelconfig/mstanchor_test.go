/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package channelconfig

import (
	"testing"

	"github.com/golang/protobuf/proto"
	cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

const testContract = "0x1234567890abcdefABCDEF1234567890aBcDeF12"

// mstValueBytes marshals a valid MST config into the config-value bytes.
func mstValueBytes(t *testing.T, cfg *MSTAnchorConfig) []byte {
	t.Helper()
	scv, err := MSTAnchorValue(cfg)
	require.NoError(t, err)
	raw, err := proto.Marshal(scv.Value())
	require.NoError(t, err)
	return raw
}

// stringValueBytes marshals an arbitrary JSON string into the config-value
// bytes, bypassing MSTAnchorValue's validation (used for negative tests).
func stringValueBytes(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := proto.Marshal(structpb.NewStringValue(s))
	require.NoError(t, err)
	return raw
}

func newAppConfigWithMST(t *testing.T, mstVal []byte) (*ApplicationConfig, error) {
	t.Helper()
	capBytes, err := proto.Marshal(&cb.Capabilities{})
	require.NoError(t, err)
	group := &cb.ConfigGroup{
		Values: map[string]*cb.ConfigValue{
			CapabilitiesKey: {Value: capBytes},
		},
	}
	if mstVal != nil {
		group.Values[MSTAnchorKey] = &cb.ConfigValue{Value: mstVal}
	}
	return NewApplicationConfig(group, &MSPConfigHandler{})
}

func TestMSTAnchorConfigRoundTrip(t *testing.T) {
	want := &MSTAnchorConfig{Enabled: true, ContractAddress: testContract, ChainID: 1337}
	ac, err := newAppConfigWithMST(t, mstValueBytes(t, want))
	require.NoError(t, err)

	got, ok := ac.MSTAnchorConfig()
	require.True(t, ok, "MST config must be present")
	require.Equal(t, want.Enabled, got.Enabled)
	require.Equal(t, want.ContractAddress, got.ContractAddress)
	require.Equal(t, want.ChainID, got.ChainID)
}

func TestMSTAnchorConfigAbsent(t *testing.T) {
	ac, err := newAppConfigWithMST(t, nil)
	require.NoError(t, err)
	_, ok := ac.MSTAnchorConfig()
	require.False(t, ok, "channels without the MST value report no config")
}

func TestMSTAnchorConfigDisabledNeedsNoAddress(t *testing.T) {
	ac, err := newAppConfigWithMST(t, mstValueBytes(t, &MSTAnchorConfig{Enabled: false}))
	require.NoError(t, err)
	got, ok := ac.MSTAnchorConfig()
	require.True(t, ok)
	require.False(t, got.Enabled)
}

func TestMSTAnchorValueRejectsEnabledWithoutAddress(t *testing.T) {
	_, err := MSTAnchorValue(&MSTAnchorConfig{Enabled: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "contractAddress")
}

func TestMSTAnchorValueRejectsBadAddress(t *testing.T) {
	for _, bad := range []string{
		"1234567890abcdef1234567890abcdef12345678", // missing 0x
		"0x1234", // too short
		"0xZZ34567890abcdef1234567890abcdef12345678", // non-hex
	} {
		_, err := MSTAnchorValue(&MSTAnchorConfig{Enabled: true, ContractAddress: bad})
		require.Error(t, err, "address %q must be rejected", bad)
	}
}

func TestNewApplicationConfigRejectsBadMSTValue(t *testing.T) {
	// An enabled config with a malformed address must fail config parsing so
	// the misconfiguration surfaces when the config is applied.
	_, err := newAppConfigWithMST(t, stringValueBytes(t, `{"enabled":true,"contractAddress":"0xbad"}`))
	require.Error(t, err)
}
