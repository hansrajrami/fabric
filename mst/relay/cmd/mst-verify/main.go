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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/verifylib"
)

// loadBatchProof parses the JSON emitted by mst-proof.
func loadBatchProof(path string) ([32]byte, [][32]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}, nil, fmt.Errorf("read batch proof: %w", err)
	}
	var file struct {
		Root  string   `json:"root"`
		Proof []string `json:"proof"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return [32]byte{}, nil, fmt.Errorf("parse batch proof: %w", err)
	}
	root, err := parse32(file.Root)
	if err != nil {
		return [32]byte{}, nil, fmt.Errorf("batch proof root: %w", err)
	}
	proof := make([][32]byte, len(file.Proof))
	for i, p := range file.Proof {
		if proof[i], err = parse32(p); err != nil {
			return [32]byte{}, nil, fmt.Errorf("batch proof sibling %d: %w", i, err)
		}
	}
	return root, proof, nil
}

func parse32(s string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return out, err
	}
	if len(raw) != 32 {
		return out, fmt.Errorf("want 32 bytes, got %d", len(raw))
	}
	copy(out[:], raw)
	return out, nil
}

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
		batchProof  = flag.String("batch-proof", "", "inclusion-proof JSON from mst-proof: verify against the batch root instead of a per-tx anchor")
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

	if *batchProof != "" {
		root, proof, err := loadBatchProof(*batchProof)
		if err != nil {
			return fail("%v", err)
		}
		result, err := verifylib.VerifyInBatch(ctx, client, in, root, proof)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Printf("recomputed commitment: 0x%s\n", hex.EncodeToString(result.Leaf[:]))
		fmt.Printf("batch root:            0x%s\n", hex.EncodeToString(root[:]))
		fmt.Printf("inclusion proof:       %v (%d siblings)\n", result.ProofValid, len(proof))
		if result.OnChain == nil {
			fmt.Println("on-chain root:         (none)")
		} else {
			fmt.Printf("on-chain root:         anchored, %d leaves, evm timestamp %d\n",
				result.OnChain.LeafCount, result.OnChain.EVMTimestamp)
		}
		if result.Match {
			fmt.Println("MATCH: the transaction is included in an anchored batch")
			return 0
		}
		fmt.Println("NO-MATCH: inclusion proof invalid or batch root not anchored")
		return 1
	}

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
