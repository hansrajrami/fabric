/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/hansrajrami/fabric/mst/relay/sender"
)

func TestDisabledByDefault(t *testing.T) {
	v := viper.New()
	cfg, err := FromViper(v)
	require.NoError(t, err)
	require.False(t, cfg.Enabled, "mst must be off unless explicitly enabled")

	v.Set("mst.enabled", false)
	cfg, err = FromViper(v)
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
}

func TestEnabledRequiresCoreSettings(t *testing.T) {
	v := viper.New()
	v.Set("mst.enabled", true)
	_, err := FromViper(v)
	require.ErrorContains(t, err, "outboxPath")

	v.Set("mst.outboxPath", "/var/mst")
	_, err = FromViper(v)
	require.ErrorContains(t, err, "rpcURL")
}

func TestFullConfig(t *testing.T) {
	v := viper.New()
	v.Set("mst.enabled", true)
	v.Set("mst.outboxPath", "/var/mst")
	v.Set("mst.channels", []string{"mychannel"})
	v.Set("mst.defaultStartBlock", 5)
	v.Set("mst.anchorStatusChaincode", "mst-anchor-status")
	v.Set("mst.excludeChaincodes", []string{"other"})
	v.Set("mst.evm.rpcURL", "http://127.0.0.1:8545")
	v.Set("mst.evm.contractAddress", "0xabc")
	v.Set("mst.evm.confirmations", 3)
	v.Set("mst.sender.cadenceMode", "batch")
	v.Set("mst.sender.cadenceN", 10)
	v.Set("mst.sender.cadenceMaxWait", "30s")
	v.Set("mst.writeback.mspID", "Org1MSP")
	v.Set("mst.writeback.certPath", "/etc/cert.pem")
	v.Set("mst.writeback.keyPath", "/etc/key.pem")

	cfg, err := FromViper(v)
	require.NoError(t, err)
	require.True(t, cfg.Enabled)
	// echo-loop guard: anchor-status chaincode force-added to exclusions
	require.Contains(t, cfg.ExcludeChaincodes, "mst-anchor-status")
	require.Contains(t, cfg.ExcludeChaincodes, "other")

	require.True(t, cfg.channelAllowed("mychannel"))
	require.False(t, cfg.channelAllowed("otherchannel"))

	sc := cfg.SenderConfig()
	require.Equal(t, sender.CadenceMode("batch"), sc.Cadence.Mode)
	require.Equal(t, 10, sc.Cadence.N)
	require.Equal(t, 30*time.Second, sc.Cadence.MaxWait)
	require.Equal(t, uint64(3), sc.Confirmations)

	require.Equal(t, uint64(5), cfg.CaptureConfig().DefaultStartBlock)
}

func TestEVMKeyFromEnvOnly(t *testing.T) {
	v := viper.New()
	v.Set("mst.enabled", true)
	v.Set("mst.outboxPath", "/var/mst")
	v.Set("mst.evm.rpcURL", "http://127.0.0.1:8545")
	v.Set("mst.evm.contractAddress", "0xabc")
	cfg, err := FromViper(v)
	require.NoError(t, err)

	t.Setenv(EnvRelayerKey, "")
	_, err = cfg.EVMConfig()
	require.ErrorContains(t, err, EnvRelayerKey)

	t.Setenv(EnvRelayerKey, "aa")
	ec, err := cfg.EVMConfig()
	require.NoError(t, err)
	require.Equal(t, "aa", ec.PrivateKeyHex)
}

func TestChannelAllowedEmptyMeansAll(t *testing.T) {
	c := &Config{}
	require.True(t, c.channelAllowed("anything"))
}
