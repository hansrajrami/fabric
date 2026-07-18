/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/hyperledger/fabric/bccsp"
	"github.com/spf13/cobra"

	"github.com/hansrajrami/fabric/mst/relay/evm"
)

// envOwnerKey holds the contract owner's EVM private key for relayer-allowlist
// changes. Falls back to the relayer key when the relayer is also the owner.
const (
	envOwnerKey   = "MST_OWNER_KEY"
	envRelayerKey = "MST_RELAYER_KEY"
)

func relayerCmd(cryptoProvider bccsp.BCCSP) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relayer add|remove <wallet-address>",
		Short: "Manage the channel contract's relayer allowlist (contract owner only)",
		Long: "Adds or removes a wallet from the per-channel MST contract's relayer allowlist by " +
			"calling setRelayer as the contract owner. Requires the owner's EVM key in " +
			"MST_OWNER_KEY (or MST_RELAYER_KEY). Only has effect if the contract was deployed with the allowlist enabled.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			var allowed bool
			switch args[0] {
			case "add":
				allowed = true
			case "remove":
				allowed = false
			default:
				return fmt.Errorf("first argument must be 'add' or 'remove', got %q", args[0])
			}
			wallet := args[1]

			if err := requireChannel(); err != nil {
				return err
			}
			ownerKey := os.Getenv(envOwnerKey)
			if ownerKey == "" {
				ownerKey = os.Getenv(envRelayerKey)
			}
			if ownerKey == "" {
				return fmt.Errorf("no owner key: set %s (or %s) to the contract owner's EVM private key", envOwnerKey, envRelayerKey)
			}

			cl, err := initClients()
			if err != nil {
				return err
			}
			mstCfg, err := cl.fetchMSTConfig(cryptoProvider)
			if err != nil {
				return err
			}
			cfg, err := evmConfigFor(mstCfg)
			if err != nil {
				return err
			}
			cfg.PrivateKeyHex = ownerKey // owner key signs the setRelayer tx

			ctx := context.Background()
			client, err := evm.Dial(ctx, cfg)
			if err != nil {
				return err
			}
			defer client.Close()

			txHash, err := client.SetRelayer(ctx, wallet, allowed)
			if err != nil {
				return err
			}
			verb := "added to"
			if !allowed {
				verb = "removed from"
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s %s the relayer allowlist on %s\n", wallet, verb, mstCfg.ContractAddress)
			fmt.Fprintf(w, "evm tx: 0x%s\n", hex.EncodeToString(txHash[:]))
			return nil
		},
	}
	attachFlags(cmd, []string{"channelID", "peerAddresses", "tlsRootCertFiles", "rpc"})
	return cmd
}
