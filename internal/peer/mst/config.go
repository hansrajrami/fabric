/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/spf13/cobra"
)

func channelConfigCmd(cryptoProvider bccsp.BCCSP) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel-config",
		Short: "Show the channel's agreed MST anchoring configuration",
		Args:  cobra.NoArgs,
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
			w := cmd.OutOrStdout()
			if jsonOutput {
				out, err := json.MarshalIndent(mstCfg, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(w, string(out))
				return nil
			}
			fmt.Fprintf(w, "enabled           : %t\n", mstCfg.Enabled)
			fmt.Fprintf(w, "contract address  : %s\n", mstCfg.ContractAddress)
			fmt.Fprintf(w, "chain id          : %d\n", mstCfg.ChainID)
			fmt.Fprintf(w, "capture mode      : %s\n", orDefault(mstCfg.CaptureMode, "opt-in"))
			if len(mstCfg.IncludeChaincodes) > 0 {
				fmt.Fprintf(w, "include chaincodes: %v\n", mstCfg.IncludeChaincodes)
			}
			if len(mstCfg.ExcludeChaincodes) > 0 {
				fmt.Fprintf(w, "exclude chaincodes: %v\n", mstCfg.ExcludeChaincodes)
			}
			fmt.Fprintf(w, "batch strategy    : %s\n", orDefault(mstCfg.BatchStrategy, "individual"))
			fmt.Fprintf(w, "confirmations     : %d\n", mstCfg.Confirmations)
			fmt.Fprintf(w, "cadence           : %s\n", cadenceSummary(mstCfg))
			return nil
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles", "json"})
	return cmd
}

func onchainCmd(cryptoProvider bccsp.BCCSP) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "onchain <txid>",
		Short: "Read a transaction's anchor directly from the channel's MST contract",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if err := requireChannel(); err != nil {
				return err
			}
			txID, err := normalizeTxID(args[0])
			if err != nil {
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
			ctx := context.Background()
			client, err := dialReadOnlyEVM(ctx, mstCfg)
			if err != nil {
				return err
			}
			defer client.Close()

			var id [32]byte
			raw, err := hex.DecodeString(txID)
			if err != nil {
				return err
			}
			copy(id[:], raw)
			rec, err := client.GetAnchor(ctx, id)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if rec == nil || !rec.Exists {
				fmt.Fprintf(w, "not anchored on contract %s\n", mstCfg.ContractAddress)
				return nil
			}
			fmt.Fprintf(w, "contract    : %s\n", mstCfg.ContractAddress)
			fmt.Fprintf(w, "commitment  : 0x%s\n", hex.EncodeToString(rec.Commitment[:]))
			fmt.Fprintf(w, "block number: %d\n", rec.BlockNumber)
			fmt.Fprintf(w, "evm time    : %d\n", rec.EVMTimestamp)
			return nil
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles", "rpc"})
	return cmd
}

func orDefault(v, def string) string {
	if v == "" {
		return def + " (default)"
	}
	return v
}

func cadenceSummary(c *channelconfig.MSTAnchorConfig) string {
	mode := c.CadenceMode
	if mode == "" {
		mode = "per-tx"
	}
	switch mode {
	case "batch":
		return fmt.Sprintf("batch (n=%d, maxWait=%s)", c.CadenceN, orNone(c.CadenceMaxWait))
	case "interval":
		return fmt.Sprintf("interval (%s)", orNone(c.CadenceInterval))
	case "cron":
		return fmt.Sprintf("cron (%q)", c.CadenceCron)
	default:
		return mode
	}
}

func orNone(v string) string {
	if v == "" {
		return "default"
	}
	return v
}
