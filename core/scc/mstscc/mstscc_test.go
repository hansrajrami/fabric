/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstscc

import (
	"encoding/json"
	"testing"

	"github.com/hyperledger/fabric-chaincode-go/shimtest"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/stretchr/testify/require"
)

const (
	txIDA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	anchorRef = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeApp is a minimal channelconfig.Application whose only meaningful method
// is MSTAnchorConfig.
type fakeApp struct {
	cfg *channelconfig.MSTAnchorConfig
}

func (fakeApp) Organizations() map[string]channelconfig.ApplicationOrg { return nil }
func (fakeApp) APIPolicyMapper() channelconfig.PolicyMapper            { return nil }
func (fakeApp) Capabilities() channelconfig.ApplicationCapabilities    { return nil }
func (f fakeApp) MSTAnchorConfig() (*channelconfig.MSTAnchorConfig, bool) {
	if f.cfg == nil {
		return nil, false
	}
	return f.cfg, true
}

type fakeGetter struct {
	apps map[string]channelconfig.Application
}

func (g fakeGetter) GetApplicationConfig(cid string) (channelconfig.Application, bool) {
	a, ok := g.apps[cid]
	return a, ok
}

func newTestStub() *shimtest.MockStub {
	getter := fakeGetter{apps: map[string]channelconfig.Application{
		"enabled":  fakeApp{cfg: &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: "0x1234567890abcdef1234567890abcdef12345678"}},
		"disabled": fakeApp{cfg: &channelconfig.MSTAnchorConfig{Enabled: false}},
		// "unconfigured" is present with no MSTAnchorConfig value.
		"unconfigured": fakeApp{cfg: nil},
	}}
	return shimtest.NewMockStub("mstscc", New(nil, getter))
}

func record(stub *shimtest.MockStub, uuid string) pb.Response {
	return stub.MockInvoke(uuid, [][]byte{[]byte(RecordAnchor), []byte(txIDA), []byte(anchorRef), []byte(StatusConfirmed)})
}

func TestRecordAndQueryOnEnabledChannel(t *testing.T) {
	stub := newTestStub()
	stub.ChannelID = "enabled"

	res := record(stub, "tx1")
	require.Equal(t, int32(200), res.Status, res.Message)

	q := stub.MockInvoke("tx2", [][]byte{[]byte(QueryAnchorStatus), []byte(txIDA)})
	require.Equal(t, int32(200), q.Status, q.Message)
	var rec AnchorStatus
	require.NoError(t, json.Unmarshal(q.Payload, &rec))
	require.Equal(t, txIDA, rec.FabricTxID)
	require.Equal(t, anchorRef, rec.AnchorRef)
	require.Equal(t, StatusConfirmed, rec.Status)
}

func TestRecordIsIdempotent(t *testing.T) {
	stub := newTestStub()
	stub.ChannelID = "enabled"

	require.Equal(t, int32(200), record(stub, "tx1").Status)
	require.Equal(t, int32(200), record(stub, "tx2").Status) // re-record: quiet no-op

	// Exactly one record persisted.
	i := stub.MockInvoke("tx3", [][]byte{[]byte(IsAnchored), []byte(txIDA)})
	require.Equal(t, int32(200), i.Status)
	require.Equal(t, "true", string(i.Payload))
}

func TestRecordRejectedWhenChannelDisabled(t *testing.T) {
	stub := newTestStub()
	stub.ChannelID = "disabled"
	res := record(stub, "tx1")
	require.Equal(t, int32(500), res.Status)
	require.Contains(t, res.Message, "not enabled")
}

func TestRecordRejectedWhenChannelUnconfigured(t *testing.T) {
	stub := newTestStub()
	stub.ChannelID = "unconfigured"
	res := record(stub, "tx1")
	require.Equal(t, int32(500), res.Status)
	require.Contains(t, res.Message, "not enabled")
}

func TestRecordRejectedWhenNoAppConfig(t *testing.T) {
	stub := newTestStub()
	stub.ChannelID = "no-such-channel"
	res := record(stub, "tx1")
	require.Equal(t, int32(500), res.Status)
	require.Contains(t, res.Message, "no application config")
}

func TestIsAnchoredFalseForUnknownTx(t *testing.T) {
	stub := newTestStub()
	stub.ChannelID = "enabled"
	i := stub.MockInvoke("tx1", [][]byte{[]byte(IsAnchored), []byte(txIDA)})
	require.Equal(t, int32(200), i.Status)
	require.Equal(t, "false", string(i.Payload))
}

func TestRecordRejectsBadArgs(t *testing.T) {
	stub := newTestStub()
	stub.ChannelID = "enabled"

	// Bad tx id length.
	res := stub.MockInvoke("tx1", [][]byte{[]byte(RecordAnchor), []byte("short"), []byte(anchorRef), []byte(StatusConfirmed)})
	require.Equal(t, int32(500), res.Status)

	// Bad anchor ref.
	res = stub.MockInvoke("tx2", [][]byte{[]byte(RecordAnchor), []byte(txIDA), []byte("0xnothex"), []byte(StatusConfirmed)})
	require.Equal(t, int32(500), res.Status)

	// Unsupported status.
	res = stub.MockInvoke("tx3", [][]byte{[]byte(RecordAnchor), []byte(txIDA), []byte(anchorRef), []byte("PENDING")})
	require.Equal(t, int32(500), res.Status)
}

func TestUnknownFunction(t *testing.T) {
	stub := newTestStub()
	stub.ChannelID = "enabled"
	res := stub.MockInvoke("tx1", [][]byte{[]byte("Nope")})
	require.Equal(t, int32(500), res.Status)
	require.Contains(t, res.Message, "unknown function")
}
