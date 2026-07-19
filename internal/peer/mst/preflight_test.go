/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"errors"
	"strings"
	"testing"

	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/stretchr/testify/require"
)

const preflightContract = "0x1234567890abcdef1234567890abcdef12345678"

// checkFor returns the check with the given label (or fails the test).
func checkFor(t *testing.T, r preflightResult, label string) checkResult {
	t.Helper()
	for _, c := range r.Checks {
		if c.Label == label {
			return c
		}
	}
	t.Fatalf("no %q check in result: %+v", label, r.Checks)
	return checkResult{}
}

func TestEvaluatePreflightDisabled(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID: "ch",
		cfg:       &channelconfig.MSTAnchorConfig{Enabled: false},
	})
	require.True(t, r.OK, "a disabled channel has nothing to fail")
	require.Equal(t, "PASS", checkFor(t, r, "enabled").Status)
	require.Contains(t, checkFor(t, r, "enabled").Detail, "disabled")
	// No chain/contract checks are attempted for a disabled channel.
	require.Len(t, r.Checks, 1)
}

func TestEvaluatePreflightChainMatch(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID:   "ch",
		cfg:         &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 1337},
		nodeChainID: 1337,
	})
	require.True(t, r.OK)
	require.Equal(t, "PASS", checkFor(t, r, "chain id").Status)
	require.Equal(t, "PASS", checkFor(t, r, "contract").Status)
	require.Equal(t, "PASS", checkFor(t, r, "rpc endpoint").Status)
}

func TestEvaluatePreflightChainMismatch(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID:   "ch",
		cfg:         &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 1337},
		nodeChainID: 42,
	})
	require.False(t, r.OK, "a chain-id mismatch must fail preflight")
	c := checkFor(t, r, "chain id")
	require.Equal(t, "FAIL", c.Status)
	require.Contains(t, c.Detail, "1337")
	require.Contains(t, c.Detail, "42")
}

func TestEvaluatePreflightChainUnpinnedWarns(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID:   "ch",
		cfg:         &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 0},
		nodeChainID: 99,
	})
	require.True(t, r.OK, "an unpinned chain id is a WARN, not a FAIL")
	c := checkFor(t, r, "chain id")
	require.Equal(t, "WARN", c.Status)
	require.Contains(t, c.Detail, "99")
}

func TestEvaluatePreflightContractProbeFails(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID:        "ch",
		cfg:              &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 1337},
		nodeChainID:      1337,
		contractProbeErr: errors.New("abi: attempting to unmarshall an empty string"),
	})
	require.False(t, r.OK, "a contract that does not respond as MSTAnchor must fail")
	c := checkFor(t, r, "contract")
	require.Equal(t, "FAIL", c.Status)
	require.Contains(t, c.Detail, preflightContract)
}

func TestEvaluatePreflightDialFails(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID: "ch",
		cfg:       &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 1337},
		dialErr:   errors.New("dial tcp: connection refused"),
	})
	require.False(t, r.OK, "an unreachable node must fail")
	require.Equal(t, "FAIL", checkFor(t, r, "rpc endpoint").Status)
	// Downstream checks are reported as not-checked failures, not silently skipped.
	require.Equal(t, "FAIL", checkFor(t, r, "chain id").Status)
	require.Equal(t, "FAIL", checkFor(t, r, "contract").Status)
}

func TestEvaluatePreflightNodeOUsAllPresent(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID:    "ch",
		cfg:          &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 1337},
		nodeChainID:  1337,
		nodeOUsTotal: 2,
	})
	require.True(t, r.OK)
	require.Equal(t, "PASS", checkFor(t, r, "nodeous").Status)
}

func TestEvaluatePreflightNodeOUsSomeMissingWarns(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID:      "ch",
		cfg:            &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 1337},
		nodeChainID:    1337,
		nodeOUsMissing: []string{"Org2"},
		nodeOUsTotal:   2,
	})
	require.True(t, r.OK, "a partial NodeOUs gap is a WARN, not a FAIL")
	c := checkFor(t, r, "nodeous")
	require.Equal(t, "WARN", c.Status)
	require.Contains(t, c.Detail, "Org2")
}

func TestEvaluatePreflightNodeOUsAllMissingFails(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID:      "ch",
		cfg:            &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 1337},
		nodeChainID:    1337,
		nodeOUsMissing: []string{"Org1", "Org2"},
		nodeOUsTotal:   2,
	})
	require.False(t, r.OK, "no org with NodeOUs means write-back can never land")
	require.Equal(t, "FAIL", checkFor(t, r, "nodeous").Status)
}

func TestRenderPreflightText(t *testing.T) {
	r := evaluatePreflight(preflightInput{
		channelID:   "mychannel",
		cfg:         &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: preflightContract, ChainID: 1337},
		nodeChainID: 1337,
	})
	var sb strings.Builder
	renderPreflightText(&sb, r)
	out := sb.String()
	require.Contains(t, out, "channel: mychannel")
	require.Contains(t, out, "[PASS]")
	require.Contains(t, out, "preflight: OK")
}

// TestPreflightProposalAndError exercises the command wiring: with a failing
// config fetch the command errors before any EVM dial, and we can still assert
// the cscc GetChannelConfig proposal it sent (the live-chain path is
// integration/manual-covered).
func TestPreflightProposalAndError(t *testing.T) {
	fake := configFetchFails()
	_, err := runCmd(t, preflightCmd(nil), fake, "-C", "mychannel")
	require.Error(t, err)
	assertCSCCConfigProposal(t, fake, "mychannel")
}

func TestPreflightRequiresChannel(t *testing.T) {
	_, err := runCmd(t, preflightCmd(nil), cannedFake(okResponse(nil)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "channelID")
}
