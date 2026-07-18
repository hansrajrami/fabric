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
	"github.com/hyperledger/fabric/core/scc/mstscc"
	"github.com/hyperledger/fabric/core/scc/qscc"
	"github.com/hyperledger/fabric/internal/pkg/mstanchor"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/spf13/cobra"

	"github.com/hansrajrami/fabric/mst/relay/verifylib"
)

// proofRequestEvent is the opt-in event name; kept in sync with
// mst/fabric-chaincode/proofhelper.EventName.
const proofRequestEvent = "MSTProofRequest"

// emptyPayloadHex is the canonical empty payload used for anchor-all mode
// transactions that declared no payload.
const emptyPayloadHex = "00000000"

func verifyCmd(cryptoProvider bccsp.BCCSP) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify <txid>",
		Short: "Verify a transaction end-to-end: recompute its commitment and compare it against the MST chain",
		Long: "Fetches the transaction from the ledger, recomputes its commitment, reads the anchor from " +
			"the channel's MST contract, and reports MATCH/NO-MATCH. Also reports whether the anchor status " +
			"is recorded on Fabric (the mstscc ledger fact). Exit code: 0 = MATCH, 1 = NO-MATCH/not anchored.",
		Args: cobra.ExactArgs(1),
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

			// (a) The Fabric ledger fact (mstscc), if present.
			recordedRef := ""
			if statusPayload, qerr := cl.queryMSTSCC(mstscc.QueryAnchorStatus, txID); qerr == nil {
				var rec mstscc.AnchorStatus
				if json.Unmarshal(statusPayload, &rec) == nil {
					recordedRef = rec.AnchorRef
				}
			}

			// (b) The original transaction (for the commitment inputs).
			in, err := cl.buildVerifyInput(txID)
			if err != nil {
				return err
			}

			// (c) The on-chain anchor, via the channel's contract.
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

			result, err := verifylib.Verify(ctx, client, *in)
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "commitment       : 0x%s\n", hex.EncodeToString(result.Commitment[:]))
			if recordedRef != "" {
				fmt.Fprintf(w, "fabric ledger fact: recorded (ref %s)\n", recordedRef)
			} else {
				fmt.Fprintf(w, "fabric ledger fact: NOT recorded on this channel\n")
			}
			if result.OnChain != nil && result.OnChain.Exists {
				fmt.Fprintf(w, "on-chain anchor  : commitment 0x%s, block %d\n",
					hex.EncodeToString(result.OnChain.Commitment[:]), result.OnChain.BlockNumber)
			} else {
				fmt.Fprintf(w, "on-chain anchor  : NOT found on contract %s\n", mstCfg.ContractAddress)
			}
			if result.Match {
				fmt.Fprintln(w, "MATCH")
				return nil
			}
			fmt.Fprintln(w, "NO-MATCH")
			return fmt.Errorf("verification failed: the recomputed commitment does not match the on-chain anchor")
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles", "rpc"})
	return cmd
}

// buildVerifyInput fetches the transaction's block via qscc, parses it, and
// assembles the verifylib.Input (channel, chaincode, block, timestamp, payload)
// needed to recompute the commitment. Opted-in transactions use their declared
// event payload; anchor-all transactions use the empty canonical payload.
func (c *clients) buildVerifyInput(txID string) (*verifylib.Input, error) {
	blockBytes, err := c.queryCC("qscc", qscc.GetBlockByTxID, channelID, txID)
	if err != nil {
		return nil, fmt.Errorf("fetch block for tx: %w", err)
	}
	block, err := protoutil.UnmarshalBlock(blockBytes)
	if err != nil {
		return nil, fmt.Errorf("unmarshal block: %w", err)
	}
	parsed, err := mstanchor.ParseBlock(block)
	if err != nil {
		return nil, fmt.Errorf("parse block: %w", err)
	}
	for i := range parsed.Txs {
		tx := &parsed.Txs[i]
		if tx.TxID != txID {
			continue
		}
		in := &verifylib.Input{
			FabricTxID:  txID,
			ChannelID:   tx.ChannelID,
			ChaincodeID: tx.ChaincodeID,
			BlockNumber: parsed.Number,
			Timestamp:   tx.TimestampUnix,
		}
		// Prefer the declared MSTProofRequest payload; fall back to the empty
		// canonical payload (anchor-all).
		in.PayloadCanonicalHex = emptyPayloadHex
		for j := range tx.Events {
			if tx.Events[j].EventName == proofRequestEvent {
				in.ChaincodeID = tx.Events[j].ChaincodeID
				in.PayloadCanonicalHex = hex.EncodeToString(tx.Events[j].Payload)
				break
			}
		}
		return in, nil
	}
	return nil, fmt.Errorf("transaction %s not found in its block", txID)
}
