// mst-verify recomputes a transaction's commitment from original data and
// checks it against the MSTAnchor contract: the independent verification
// that makes anchoring worth anything.
//
// Exit codes: 0 = MATCH, 1 = NO-MATCH / not anchored, 2 = usage or error.
//
// Example:
//
//	mst-verify \
//	  --tx-id 4e5b...64hex \
//	  --channel mychannel --chaincode mst-example \
//	  --block 12 --timestamp 1720000000 \
//	  --payload payload.json \
//	  --rpc http://127.0.0.1:8545 --contract 0x5FbD...
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/verifylib"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		txID        = flag.String("tx-id", "", "Fabric transaction id (64 hex chars)")
		channel     = flag.String("channel", "", "channel id")
		chaincode   = flag.String("chaincode", "", "chaincode id")
		block       = flag.Uint64("block", 0, "Fabric block number")
		timestamp   = flag.Uint64("timestamp", 0, "tx timestamp (unix seconds, from the tx ChannelHeader)")
		payloadFile = flag.String("payload", "", "JSON file with the declared payload fields")
		payloadHex  = flag.String("payload-hex", "", "exact canonical event payload bytes (hex) instead of --payload")
		rpcURL      = flag.String("rpc", "", "MST JSON-RPC endpoint")
		contract    = flag.String("contract", "", "MSTAnchor contract address")
		printOnly   = flag.Bool("print-only", false, "only print the recomputed commitment; no chain access")
	)
	flag.Parse()

	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
		return 2
	}

	if *txID == "" {
		return fail("--tx-id is required")
	}
	in := verifylib.Input{
		FabricTxID:  *txID,
		ChannelID:   *channel,
		ChaincodeID: *chaincode,
		BlockNumber: *block,
		Timestamp:   *timestamp,
	}
	switch {
	case *payloadFile != "" && *payloadHex != "":
		return fail("--payload and --payload-hex are mutually exclusive")
	case *payloadFile != "":
		raw, err := os.ReadFile(*payloadFile)
		if err != nil {
			return fail("read payload: %v", err)
		}
		p, err := verifylib.ParsePayloadJSON(raw)
		if err != nil {
			return fail("%v", err)
		}
		in.Payload = p
	case *payloadHex != "":
		in.PayloadCanonicalHex = *payloadHex
	default:
		return fail("one of --payload or --payload-hex is required")
	}

	if *printOnly {
		commitment, err := verifylib.Recompute(in)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Printf("commitment: 0x%s\n", hex.EncodeToString(commitment[:]))
		return 0
	}

	if *rpcURL == "" || *contract == "" {
		return fail("--rpc and --contract are required (or use --print-only)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Read-only access: a dummy key satisfies the client constructor; no
	// transaction is ever signed or sent by this tool.
	client, err := evm.Dial(ctx, evm.Config{
		RPCURL:          *rpcURL,
		ContractAddress: *contract,
		PrivateKeyHex:   "0000000000000000000000000000000000000000000000000000000000000001",
	})
	if err != nil {
		return fail("%v", err)
	}
	defer client.Close()

	result, err := verifylib.Verify(ctx, client, in)
	if err != nil {
		return fail("%v", err)
	}

	fmt.Printf("recomputed commitment: 0x%s\n", hex.EncodeToString(result.Commitment[:]))
	if result.OnChain == nil {
		fmt.Println("on-chain anchor:       (none)")
		fmt.Println("NO-MATCH: transaction is not anchored")
		return 1
	}
	fmt.Printf("on-chain commitment:   0x%s\n", hex.EncodeToString(result.OnChain.Commitment[:]))
	fmt.Printf("anchored at:           MST block %d, evm timestamp %d\n",
		result.OnChain.BlockNumber, result.OnChain.EVMTimestamp)
	if result.Match {
		fmt.Println("MATCH: the anchored commitment equals the recomputation from the original data")
		return 0
	}
	fmt.Println("NO-MATCH: the anchored commitment differs — the data does not correspond to what was anchored")
	return 1
}
