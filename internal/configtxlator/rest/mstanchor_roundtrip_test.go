/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package rest_test

import (
	"bytes"
	"testing"

	"github.com/golang/protobuf/proto"
	cb "github.com/hyperledger/fabric-config/protolator"
	commonpb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/stretchr/testify/require"
)

// TestConfigtxlatorMSTAnchorRoundTrip guards the fabric-config fork patch that
// registers the MSTAnchor Application config value in the protolator. Without it,
// configtxlator proto_decode/proto_encode fail on any channel config that has
// MST anchoring enabled (Unknown Application ConfigValue name: MSTAnchor), which
// breaks every configtxlator-based config update on such a channel.
func TestConfigtxlatorMSTAnchorRoundTrip(t *testing.T) {
	scv, err := channelconfig.MSTAnchorValue(&channelconfig.MSTAnchorConfig{
		Enabled:         true,
		ContractAddress: "0x1234567890abcdef1234567890abcdef12345678",
		ChainID:         1337,
		CaptureMode:     "opt-in",
		BatchStrategy:   "individual",
		Confirmations:   1,
	})
	require.NoError(t, err)
	valueBytes, err := proto.Marshal(scv.Value())
	require.NoError(t, err)

	config := &commonpb.Config{ChannelGroup: &commonpb.ConfigGroup{
		Groups: map[string]*commonpb.ConfigGroup{
			"Application": {Values: map[string]*commonpb.ConfigValue{
				"MSTAnchor": {ModPolicy: "Admins", Value: valueBytes},
			}},
		},
	}}

	// decode (proto -> JSON): the operation that failed before the fork patch.
	var jsonBuf bytes.Buffer
	require.NoError(t, cb.DeepMarshalJSON(&jsonBuf, config))
	require.Contains(t, jsonBuf.String(), "MSTAnchor")

	// encode (JSON -> proto) round-trip.
	decoded := &commonpb.Config{}
	require.NoError(t, cb.DeepUnmarshalJSON(bytes.NewReader(jsonBuf.Bytes()), decoded))

	// The MST value survives the round-trip byte-for-byte.
	got := decoded.GetChannelGroup().GetGroups()["Application"].GetValues()["MSTAnchor"].GetValue()
	require.Equal(t, valueBytes, got)
}
