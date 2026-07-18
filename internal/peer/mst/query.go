/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/hyperledger/fabric/core/scc/mstscc"
	"github.com/spf13/cobra"
)

func statusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <txid>",
		Short: "Show the anchor status recorded on Fabric for a transaction",
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
			payload, err := cl.queryMSTSCC(mstscc.QueryAnchorStatus, txID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOutput {
				fmt.Fprintln(out, string(payload))
				return nil
			}
			var rec mstscc.AnchorStatus
			if err := json.Unmarshal(payload, &rec); err != nil {
				return fmt.Errorf("decode record: %w", err)
			}
			fmt.Fprintf(out, "fabric tx id : %s\n", rec.FabricTxID)
			fmt.Fprintf(out, "anchor ref   : %s\n", rec.AnchorRef)
			fmt.Fprintf(out, "status       : %s\n", rec.Status)
			fmt.Fprintf(out, "recorded at  : %s (%d)\n", time.Unix(rec.RecordedAt, 0).UTC().Format(time.RFC3339), rec.RecordedAt)
			return nil
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles", "json"})
	return cmd
}

func isAnchoredCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "is-anchored <txid>",
		Short: "Report whether a transaction has been anchored (true/false)",
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
			payload, err := cl.queryMSTSCC(mstscc.IsAnchored, txID)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(payload))
			return nil
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles"})
	return cmd
}

func listCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List anchor-status records on the channel",
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
			ccArgs := []string{}
			if listLimit > 0 {
				ccArgs = append(ccArgs, strconv.Itoa(listLimit))
			}
			payload, err := cl.queryMSTSCC(mstscc.ListAnchors, ccArgs...)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOutput {
				fmt.Fprintln(out, string(payload))
				return nil
			}
			var recs []mstscc.AnchorStatus
			if err := json.Unmarshal(payload, &recs); err != nil {
				return fmt.Errorf("decode records: %w", err)
			}
			if len(recs) == 0 {
				fmt.Fprintln(out, "no anchored transactions on this channel")
				return nil
			}
			for _, r := range recs {
				fmt.Fprintf(out, "%s  %s  %s  %s\n", r.FabricTxID, r.Status, r.AnchorRef,
					time.Unix(r.RecordedAt, 0).UTC().Format(time.RFC3339))
			}
			return nil
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles", "limit", "json"})
	return cmd
}

func countCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "count",
		Short: "Count anchor-status records on the channel",
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
			payload, err := cl.queryMSTSCC(mstscc.CountAnchors)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(payload))
			return nil
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles"})
	return cmd
}
