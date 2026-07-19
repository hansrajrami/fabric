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
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

// Hardhat/anvil dev accounts #0 (deployer/owner) and #1 (a second funded key).
const (
	devKey  = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	devKey1 = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
)

// selector4 returns the 4-byte function selector for a Solidity signature.
func selector4(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

// encodeInitialize ABI-encodes the initialize(bool allowlistEnabled,
// address[] initialRelayers) call the proxy runs once in its own storage.
func encodeInitialize(allowlistEnabled bool, relayers []common.Address) []byte {
	head := make([]byte, 2*32) // (bool, offset)
	if allowlistEnabled {
		head[31] = 1
	}
	head[63] = 0x40 // offset of the address[] tail (2 words in)
	tail := make([]byte, 32+len(relayers)*32)
	binary.BigEndian.PutUint64(tail[24:32], uint64(len(relayers)))
	for i, r := range relayers {
		copy(tail[32+i*32+12:32+i*32+32], r[:])
	}
	return append(append(selector4("initialize(bool,address[])"), head...), tail...)
}

// encodeProxyConstructor ABI-encodes TransparentUpgradeableProxy's
// constructor(address logic, address initialOwner, bytes data) tail. initialOwner
// owns the auto-created ProxyAdmin (the upgrade key); data is the initialize call.
func encodeProxyConstructor(logic, initialOwner common.Address, data []byte) []byte {
	head := make([]byte, 3*32)
	copy(head[12:32], logic[:])
	copy(head[32+12:64], initialOwner[:])
	head[95] = 0x60 // offset of the bytes arg (3 words in)
	padded := (len(data) + 31) / 32 * 32
	tail := make([]byte, 32+padded)
	binary.BigEndian.PutUint64(tail[24:32], uint64(len(data)))
	copy(tail[32:32+len(data)], data)
	return append(head, tail...)
}

// readBytecode loads a compiled artifact's creation bytecode from abi/.
func readBytecode(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash("../../anchor-contracts/abi/" + name))
	if err != nil {
		t.Fatalf("read artifact (run `npm run build` in mst/anchor-contracts first): %v", err)
	}
	var artifact struct {
		Bytecode string `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	bc, err := hex.DecodeString(strings.TrimPrefix(artifact.Bytecode, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return bc
}

// deployRaw sends a contract-creation transaction and returns the deployed address.
func deployRaw(t *testing.T, ctx context.Context, eth *ethclient.Client, key *ecdsa.PrivateKey, chainID *big.Int, data []byte) common.Address {
	t.Helper()
	from := crypto.PubkeyToAddress(key.PublicKey)
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
		Gas:       2_500_000, // headroom for the proxy + auto ProxyAdmin + initialize
		Data:      data,
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
			return receipt.ContractAddress
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

// deployAnchor deploys the MSTAnchor implementation and a TransparentUpgradeableProxy
// in front of it (its real deployment shape), and returns the PROXY address — the
// stable, channel-facing address the relay talks to. The ABI/selectors are identical
// through the proxy's delegatecall, so the client code is unchanged.
func deployAnchor(t *testing.T, rpcURL string, allowlistEnabled bool, relayers []common.Address) string {
	t.Helper()
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

	// 1. Implementation — its constructor only calls _disableInitializers() (no args).
	impl := deployRaw(t, ctx, eth, key, chainID, readBytecode(t, "MSTAnchor.json"))
	// 2. Proxy — initialize runs in the proxy's storage with `from` as owner; `from`
	//    also owns the ProxyAdmin (deployer-key upgrade authority).
	proxyDeploy := append(
		readBytecode(t, "TransparentUpgradeableProxy.json"),
		encodeProxyConstructor(impl, from, encodeInitialize(allowlistEnabled, relayers))...,
	)
	return deployRaw(t, ctx, eth, key, chainID, proxyDeploy).Hex()
}

// TestIntegrationRelayerAllowlist exercises the owner-only setRelayer path on an
// allowlist-enabled contract: a non-allowlisted account's anchor reverts, the
// owner adds it, its anchor then succeeds, and after removal it reverts again.
// Membership is verified behaviorally (anchor succeeds vs ErrReverted) since the
// Go client has no isRelayer getter.
func TestIntegrationRelayerAllowlist(t *testing.T) {
	rpcURL := os.Getenv("MST_EVM_RPC")
	if rpcURL == "" {
		t.Skip("MST_EVM_RPC not set; skipping EVM integration test")
	}
	ctx := context.Background()

	// Allowlist enabled, no initial relayers (owner = deployer = dev #0).
	contractAddr := deployAnchor(t, rpcURL, true, nil)
	t.Logf("allowlisted MSTAnchor deployed at %s", contractAddr)

	owner, err := evm.Dial(ctx, evm.Config{RPCURL: rpcURL, ContractAddress: contractAddr, PrivateKeyHex: devKey})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	guest, err := evm.Dial(ctx, evm.Config{RPCURL: rpcURL, ContractAddress: contractAddr, PrivateKeyHex: devKey1})
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()

	var txID, txID2, commitment [32]byte
	copy(txID[:], []byte("allowlist-tx-000000000000000001"))
	copy(txID2[:], []byte("allowlist-tx-000000000000000002"))
	copy(commitment[:], []byte("allowlist-commitment-0000000001"))

	anchor := func(c *evm.Client, id [32]byte) error {
		hash, err := c.SubmitAnchor(ctx, id, commitment, 1)
		if err != nil {
			return err
		}
		return c.WaitConfirmed(ctx, hash, 1)
	}

	// 1. Non-allowlisted guest → NotRelayer revert. The revert may surface either
	// when the receipt is checked (ErrReverted) or at send time — some nodes
	// (e.g. Hardhat with throwOnTransactionFailures) reject a reverting tx from
	// eth_sendTransaction — so accept both.
	if err := anchor(guest, txID); !isReverted(err) {
		t.Fatalf("expected a revert for non-allowlisted anchor, got %v", err)
	}

	// 2. Owner allowlists the guest.
	setHash, err := owner.SetRelayer(ctx, guest.Sender(), true)
	if err != nil {
		t.Fatalf("setRelayer add: %v", err)
	}
	if err := owner.WaitConfirmed(ctx, setHash, 1); err != nil {
		t.Fatalf("setRelayer add not confirmed: %v", err)
	}

	// 3. Guest anchor now succeeds and is recorded.
	if err := anchor(guest, txID); err != nil {
		t.Fatalf("allowlisted anchor should succeed, got %v", err)
	}
	rec, err := guest.GetAnchor(ctx, txID)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.Commitment != commitment {
		t.Fatalf("anchor not recorded/commitment mismatch: %+v", rec)
	}

	// 4. Owner removes the guest → a fresh anchor reverts again.
	rmHash, err := owner.SetRelayer(ctx, guest.Sender(), false)
	if err != nil {
		t.Fatalf("setRelayer remove: %v", err)
	}
	if err := owner.WaitConfirmed(ctx, rmHash, 1); err != nil {
		t.Fatalf("setRelayer remove not confirmed: %v", err)
	}
	if err := anchor(guest, txID2); !isReverted(err) {
		t.Fatalf("expected a revert after removal, got %v", err)
	}
}

// isReverted reports whether err represents an on-chain revert, surfaced either as
// a mined-and-failed receipt (evm.ErrReverted) or at send time (nodes that reject a
// reverting tx from eth_sendTransaction).
func isReverted(err error) bool {
	return err != nil && (errors.Is(err, evm.ErrReverted) ||
		strings.Contains(strings.ToLower(err.Error()), "revert"))
}

func TestIntegrationClientAndSender(t *testing.T) {
	rpcURL := os.Getenv("MST_EVM_RPC")
	if rpcURL == "" {
		t.Skip("MST_EVM_RPC not set; skipping EVM integration test")
	}
	ctx := context.Background()
	contractAddr := deployAnchor(t, rpcURL, false, nil)
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
