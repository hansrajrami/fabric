/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/spf13/cobra"

	"github.com/hansrajrami/fabric/mst/relay/evm"
)

// preflightInput carries everything the (pure) evaluator needs: the channel's
// MST config plus the results of the read-only chain probes the command already
// performed. Separating gathering (I/O) from evaluation (decision) keeps the
// decision logic unit-testable without a live chain.
type preflightInput struct {
	channelID string
	cfg       *channelconfig.MSTAnchorConfig
	// dialErr is non-nil when the RPC endpoint could not be reached (or its
	// chain id could not be fetched); nodeChainID/contractProbeErr are then moot.
	dialErr error
	// nodeChainID is the chain id the node actually reports (valid when
	// dialErr == nil).
	nodeChainID uint64
	// contractProbeErr is the error from reading the contract (nil = the
	// contract responded as an MSTAnchor).
	contractProbeErr error
}

// checkResult is one line of the preflight checklist.
type checkResult struct {
	Status string `json:"status"` // PASS | WARN | FAIL
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// preflightResult is the full outcome; OK is false if any check FAILed (WARNs
// do not fail the run).
type preflightResult struct {
	Channel string        `json:"channel"`
	OK      bool          `json:"ok"`
	Checks  []checkResult `json:"checks"`
}

// evaluatePreflight is the pure decision core: given the config and the gathered
// probe results, it produces the checklist and the overall pass/fail. It makes
// no network calls, so it is exhaustively unit-testable.
func evaluatePreflight(in preflightInput) preflightResult {
	r := preflightResult{Channel: in.channelID, OK: true}
	add := func(status, label, detail string) {
		r.Checks = append(r.Checks, checkResult{Status: status, Label: label, Detail: detail})
		if status == "FAIL" {
			r.OK = false
		}
	}

	if !in.cfg.Enabled {
		add("PASS", "enabled", "MST anchoring is disabled on this channel — nothing to preflight")
		return r
	}
	add("PASS", "enabled", "MST anchoring is enabled")

	if in.dialErr != nil {
		add("FAIL", "rpc endpoint", "cannot reach MST node: "+in.dialErr.Error())
		add("FAIL", "chain id", "not checked (no connection to the MST node)")
		add("FAIL", "contract", "not checked (no connection to the MST node)")
		return r
	}
	add("PASS", "rpc endpoint", fmt.Sprintf("connected; node reports chain id %d", in.nodeChainID))

	switch {
	case in.cfg.ChainID == 0:
		add("WARN", "chain id", fmt.Sprintf("config chainID is 0 (unpinned); the peer will trust whatever chain it connects to (node reports %d)", in.nodeChainID))
	case in.cfg.ChainID == in.nodeChainID:
		add("PASS", "chain id", fmt.Sprintf("config chainID %d matches the node", in.cfg.ChainID))
	default:
		add("FAIL", "chain id", fmt.Sprintf("config chainID %d does not match the node's chain id %d", in.cfg.ChainID, in.nodeChainID))
	}

	if in.contractProbeErr != nil {
		add("FAIL", "contract", fmt.Sprintf("%s: no MSTAnchor contract deployed there, or incompatible ABI (%s)", in.cfg.ContractAddress, in.contractProbeErr.Error()))
	} else {
		add("PASS", "contract", fmt.Sprintf("%s responds as an MSTAnchor contract", in.cfg.ContractAddress))
	}
	return r
}

// renderPreflightText writes the checklist in human-readable form.
func renderPreflightText(w io.Writer, r preflightResult) {
	fmt.Fprintf(w, "channel: %s\n", r.Channel)
	for _, c := range r.Checks {
		fmt.Fprintf(w, "[%-4s] %-13s %s\n", c.Status, c.Label, c.Detail)
	}
	if r.OK {
		fmt.Fprintln(w, "\npreflight: OK")
	} else {
		fmt.Fprintln(w, "\npreflight: FAILED")
	}
}

func preflightCmd(cryptoProvider bccsp.BCCSP) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Validate a channel's MST config against the live chain before a config update",
		Long: "Reads the channel's current MST anchoring configuration and checks it against the " +
			"MST node: that the node is reachable, its chain id matches the configured ChainID, and " +
			"the configured contract address actually hosts a compatible MSTAnchor contract. Intended " +
			"to be run before applying an MSTAnchor config update. Exits non-zero if any check fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if err := requireChannel(); err != nil {
				return err
			}
			cl, err := initClients()
			if err != nil {
				return err
			}
			mstCfg, err := cl.fetchMSTConfig(cryptoProvider)
			if err != nil {
				return err
			}

			in := preflightInput{channelID: channelID, cfg: mstCfg}
			if mstCfg.Enabled {
				cfg, err := evmConfigFor(mstCfg)
				if err != nil {
					return err
				}
				// Force a live chain-id fetch: evm.Dial only queries the node
				// when ChainID is 0, otherwise Client.ChainID() would just echo
				// the configured value and the match check would be vacuous.
				cfg.ChainID = 0
				ctx := context.Background()
				client, derr := evm.Dial(ctx, cfg)
				if derr != nil {
					in.dialErr = derr
				} else {
					defer client.Close()
					in.nodeChainID = client.ChainID()
					// Probe the contract with a zero tx id: a real MSTAnchor
					// returns cleanly (Exists=false); a bare/incompatible
					// address makes the ABI read error.
					_, in.contractProbeErr = client.GetAnchor(ctx, [32]byte{})
				}
			}

			result := evaluatePreflight(in)
			w := cmd.OutOrStdout()
			if jsonOutput {
				out, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(w, string(out))
			} else {
				renderPreflightText(w, result)
			}
			if !result.OK {
				return fmt.Errorf("preflight failed: one or more checks did not pass")
			}
			return nil
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles", "rpc", "json"})
	return cmd
}
