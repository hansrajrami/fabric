package evm_test

// Integration test against a real EVM node. Skipped unless MST_EVM_RPC is
// set, e.g.:
//
//	cd mst/anchor-contracts && npx hardhat node   # terminal 1
//	MST_EVM_RPC=http://127.0.0.1:8545 go test ./evm/ -run Integration -v
//
// It deploys MSTAnchor from the checked-in artifact bytecode, then drives
// the real client AND the full sender pipeline against it.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

// Hardhat/anvil dev account #0.
const devKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func deployAnchor(t *testing.T, rpcURL string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash("../../anchor-contracts/abi/MSTAnchor.json"))
	if err != nil {
		t.Fatalf("read artifact (run `npm run build` in mst/anchor-contracts first): %v", err)
	}
	var artifact struct {
		Bytecode string `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	bytecode, err := hex.DecodeString(strings.TrimPrefix(artifact.Bytecode, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	// Constructor args: (bool allowlistEnabled=false, address[] relayers=[]).
	ctorArgs := make([]byte, 3*32)
	ctorArgs[63] = 0x40 // offset of the empty array
	deployData := append(bytecode, ctorArgs...)

	eth, err := ethclient.Dial(rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	defer eth.Close()
	ctx := context.Background()

	key, _ := crypto.HexToECDSA(devKey)
	from := crypto.PubkeyToAddress(key.PublicKey)
	chainID, err := eth.ChainID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := eth.PendingNonceAt(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	head, err := eth.HeaderByNumber(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tip := big.NewInt(1_000_000_000)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2))),
		Gas:       1_500_000,
		Data:      deployData,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := eth.SendTransaction(ctx, signed); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		receipt, err := eth.TransactionReceipt(ctx, signed.Hash())
		if err == nil {
			if receipt.Status != types.ReceiptStatusSuccessful {
				t.Fatal("deploy reverted")
			}
			return receipt.ContractAddress.Hex()
		}
		if err != ethereum.NotFound {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("deploy not mined in time")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestIntegrationClientAndSender(t *testing.T) {
	rpcURL := os.Getenv("MST_EVM_RPC")
	if rpcURL == "" {
		t.Skip("MST_EVM_RPC not set; skipping EVM integration test")
	}
	ctx := context.Background()
	contractAddr := deployAnchor(t, rpcURL)
	t.Logf("MSTAnchor deployed at %s", contractAddr)

	client, err := evm.Dial(ctx, evm.Config{
		RPCURL:          rpcURL,
		ContractAddress: contractAddr,
		PrivateKeyHex:   devKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var txID, commitment [32]byte
	copy(txID[:], []byte("integration-test-tx-000000000001"))
	copy(commitment[:], []byte("integration-test-commitment-0001"))

	// The funded dev account must report a positive balance.
	balance, err := client.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Sign() <= 0 {
		t.Fatalf("dev account balance must be positive, got %s", balance)
	}

	// Not anchored yet.
	rec, err := client.GetAnchor(ctx, txID)
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("unexpected pre-existing anchor")
	}

	// Anchor and confirm.
	hash, err := client.SubmitAnchor(ctx, txID, commitment, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WaitConfirmed(ctx, hash, 1); err != nil {
		t.Fatal(err)
	}
	rec, err = client.GetAnchor(ctx, txID)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.Commitment != commitment || rec.BlockNumber != 42 || rec.EVMTimestamp == 0 {
		t.Fatalf("anchor record: %+v", rec)
	}

	// Duplicate submission is a quiet no-op (different commitment ignored).
	var evil [32]byte
	copy(evil[:], []byte("evil-overwrite-attempt-0000000001"))
	hash2, err := client.SubmitAnchor(ctx, txID, evil, 999)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WaitConfirmed(ctx, hash2, 1); err != nil {
		t.Fatal(err)
	}
	rec, err = client.GetAnchor(ctx, txID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Commitment != commitment {
		t.Fatal("duplicate overwrote the anchor")
	}

	// Merkle batch root: anchor once, duplicate is a quiet no-op.
	var root [32]byte
	copy(root[:], []byte("integration-merkle-root-00000001"))
	rootRec, err := client.GetRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if rootRec != nil {
		t.Fatal("unexpected pre-existing root")
	}
	rootHash, err := client.SubmitAnchorRoot(ctx, root, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WaitConfirmed(ctx, rootHash, 1); err != nil {
		t.Fatal(err)
	}
	rootRec, err = client.GetRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if rootRec == nil || rootRec.LeafCount != 20 || rootRec.EVMTimestamp == 0 {
		t.Fatalf("root record: %+v", rootRec)
	}
	dupRoot, err := client.SubmitAnchorRoot(ctx, root, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WaitConfirmed(ctx, dupRoot, 1); err != nil {
		t.Fatal(err)
	}
	rootRec, _ = client.GetRoot(ctx, root)
	if rootRec.LeafCount != 20 {
		t.Fatal("duplicate root overwrote the record")
	}

	// Full sender pipeline against the real chain.
	store, err := outbox.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var entries []*outbox.Entry
	for i := byte(1); i <= 5; i++ {
		var id, c [32]byte
		id[0], id[31] = 0xAB, i
		c[0], c[31] = 0xCD, i
		entries = append(entries, &outbox.Entry{
			FabricTxID: id, Commitment: c, ChannelID: "ch", ChaincodeID: "cc",
			BlockNumber: uint64(i), Timestamp: 1720000000,
		})
	}
	if _, err := store.PutBlock(entries, 1); err != nil {
		t.Fatal(err)
	}

	snd, err := sender.New(store, client, nil, sender.Config{Workers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := snd.FlushOnce(ctx); err != nil {
		t.Fatal(err)
	}

	done, err := store.ListByStatus(outbox.StatusDone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 5 {
		t.Fatalf("sender pipeline: want 5 DONE, got %d", len(done))
	}
	for _, e := range done {
		rec, err := client.GetAnchor(ctx, e.FabricTxID)
		if err != nil || rec == nil || rec.Commitment != e.Commitment {
			t.Fatalf("on-chain anchor mismatch for %x: %+v %v", e.FabricTxID, rec, err)
		}
	}

	// Idempotent re-flush: nothing pending, nothing double-anchored.
	if err := snd.FlushOnce(ctx); err != nil {
		t.Fatal(err)
	}
}
