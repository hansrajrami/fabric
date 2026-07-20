/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"sync"
	"testing"

	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/stretchr/testify/require"

	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

func TestPipelineFingerprintChangesWithGovernedFields(t *testing.T) {
	base := &channelconfig.MSTAnchorConfig{
		Enabled:         true,
		ContractAddress: "0x1234567890abcdef1234567890abcdef12345678",
		ChainID:         1337,
		CaptureMode:     "all",
		BatchStrategy:   "individual",
		Confirmations:   1,
		CadenceMode:     "batch",
		CadenceN:        10,
	}
	baseFP := pipelineFingerprint(base)

	// Same values → same fingerprint (deterministic).
	require.Equal(t, baseFP, pipelineFingerprint(&channelconfig.MSTAnchorConfig{
		Enabled: true, ContractAddress: "0x1234567890abcdef1234567890abcdef12345678",
		ChainID: 1337, CaptureMode: "all", BatchStrategy: "individual",
		Confirmations: 1, CadenceMode: "batch", CadenceN: 10,
	}))

	// Each governed field flips the fingerprint.
	for name, mutate := range map[string]func(c *channelconfig.MSTAnchorConfig){
		"captureMode": func(c *channelconfig.MSTAnchorConfig) { c.CaptureMode = "opt-in" },
		"contractAddress": func(c *channelconfig.MSTAnchorConfig) {
			c.ContractAddress = "0x0000000000000000000000000000000000000009"
		},
		"chainID":       func(c *channelconfig.MSTAnchorConfig) { c.ChainID = 9999 },
		"batchStrategy": func(c *channelconfig.MSTAnchorConfig) { c.BatchStrategy = "merkle" },
		"confirmations": func(c *channelconfig.MSTAnchorConfig) { c.Confirmations = 6 },
		"cadenceMode":   func(c *channelconfig.MSTAnchorConfig) { c.CadenceMode = "interval" },
		"cadenceN":      func(c *channelconfig.MSTAnchorConfig) { c.CadenceN = 20 },
		"include":       func(c *channelconfig.MSTAnchorConfig) { c.IncludeChaincodes = []string{"cc"} },
		"exclude":       func(c *channelconfig.MSTAnchorConfig) { c.ExcludeChaincodes = []string{"cc"} },
	} {
		cp := *base
		mutate(&cp)
		require.NotEqual(t, baseFP, pipelineFingerprint(&cp), "changing %s must change the fingerprint", name)
	}
}

func TestPlanPipelineAction(t *testing.T) {
	cases := []struct {
		name      string
		running   bool
		runningFP string
		want      bool
		desiredFP string
		expect    pipelineAction
	}{
		{"enable a new channel", false, "", true, "fpA", actionStart},
		{"steady state, no change", true, "fpA", true, "fpA", actionNone},
		{"config changed → reload", true, "fpA", true, "fpB", actionRestart},
		{"channel disabled → stop", true, "fpA", false, "", actionStop},
		{"not running, not wanted", false, "", false, "", actionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expect, planPipelineAction(tc.running, tc.runningFP, tc.want, tc.desiredFP))
		})
	}
}

// closeSpyStore embeds outbox.Store (only Close is exercised) and records the close.
type closeSpyStore struct {
	outbox.Store
	closed bool
}

func (s *closeSpyStore) Close() error {
	s.closed = true
	return nil
}

func TestStopPipeline(t *testing.T) {
	s := &Service{pipelines: map[string]*pipeline{}}

	cancelled := false
	store := &closeSpyStore{}
	// A pipeline with no live goroutines: done is empty so Wait returns at once.
	s.pipelines["ch"] = &pipeline{
		channelID: "ch",
		store:     store,
		cancel:    func() { cancelled = true },
		done:      &sync.WaitGroup{},
		fp:        "fp",
	}

	s.stopPipeline("ch")

	require.True(t, cancelled, "stopPipeline must cancel the pipeline context")
	require.True(t, store.closed, "stopPipeline must close the outbox store")
	require.NotContains(t, s.pipelines, "ch", "stopPipeline must remove the channel")

	// Idempotent: stopping an unknown/already-stopped channel is a no-op.
	require.NotPanics(t, func() { s.stopPipeline("ch") })
}
