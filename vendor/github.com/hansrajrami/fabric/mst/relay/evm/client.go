package evm

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Config locates the MST chain and the relayer's key. Everything is plain
// configuration so pointing at the real MST network is a config-only change.
type Config struct {
	// RPCURL is the JSON-RPC endpoint (http(s) or ws(s)).
	RPCURL string
	// ContractAddress is the deployed MSTAnchor address (0x hex).
	ContractAddress string
	// PrivateKeyHex is the relayer's secp256k1 key (hex, no 0x needed).
	// Load it from an env var or secret store — never from a committed file.
	PrivateKeyHex string
	// ChainID; if zero it is fetched from the node at startup.
	ChainID uint64
	// GasLimit for anchor transactions (default 200000, plus 60000 per extra
	// batch element).
	GasLimit uint64
	// TipCapGwei is the priority fee; 0 lets the node suggest.
	TipCapGwei uint64
	// ReceiptPollInterval controls confirmation polling (default 500ms).
	ReceiptPollInterval time.Duration
}

const (
	defaultGasLimit     = 200_000
	perBatchItemGas     = 60_000
	defaultReceiptPoll  = 500 * time.Millisecond
	basefeeHeadroomMult = 2 // maxFee = 2*baseFee + tip: survives fee spikes
)

// Client talks to the MSTAnchor contract through go-ethereum's ethclient
// with EIP-1559 transactions signed by a local key.
type Client struct {
	eth      *ethclient.Client
	key      *ecdsa.PrivateKey
	sender   common.Address
	contract common.Address
	chainID  *big.Int
	signer   types.Signer
	cfg      Config

	// nonce management: initialized from PendingNonceAt, advanced locally,
	// resynced on nonce errors. The mutex serializes submissions; parallel
	// sender workers still overlap on confirmation waits, which is where the
	// latency actually is.
	nonceMu     sync.Mutex
	nonce       uint64
	nonceInited bool
}

// Dial connects, derives the sender address, and pins the chain id.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.ReceiptPollInterval <= 0 {
		cfg.ReceiptPollInterval = defaultReceiptPoll
	}
	if cfg.GasLimit == 0 {
		cfg.GasLimit = defaultGasLimit
	}
	eth, err := ethclient.DialContext(ctx, cfg.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("evm: dial %s: %w", cfg.RPCURL, err)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.PrivateKeyHex, "0x"))
	if err != nil {
		eth.Close()
		return nil, fmt.Errorf("evm: parse relayer key: %w", err)
	}
	if !common.IsHexAddress(cfg.ContractAddress) {
		eth.Close()
		return nil, fmt.Errorf("evm: bad contract address %q", cfg.ContractAddress)
	}

	chainID := new(big.Int).SetUint64(cfg.ChainID)
	if cfg.ChainID == 0 {
		chainID, err = eth.ChainID(ctx)
		if err != nil {
			eth.Close()
			return nil, fmt.Errorf("evm: fetch chain id: %w", err)
		}
	}

	return &Client{
		eth:      eth,
		key:      key,
		sender:   crypto.PubkeyToAddress(key.PublicKey),
		contract: common.HexToAddress(cfg.ContractAddress),
		chainID:  chainID,
		signer:   types.LatestSignerForChainID(chainID),
		cfg:      cfg,
	}, nil
}

// Sender returns the relayer's EVM address.
func (c *Client) Sender() string { return c.sender.Hex() }

// Close releases the RPC connection.
func (c *Client) Close() { c.eth.Close() }

// GetAnchor reads the anchor record for fabricTxID; nil when not anchored.
func (c *Client) GetAnchor(ctx context.Context, fabricTxID [32]byte) (*AnchorRecord, error) {
	data := packGetAnchor(fabricTxID)
	ret, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &c.contract, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("evm: getAnchor call: %w", err)
	}
	rec, err := unpackGetAnchor(ret)
	if err != nil {
		return nil, err
	}
	if !rec.Exists {
		return nil, nil
	}
	return rec, nil
}

// SubmitAnchor sends anchor(fabricTxID, commitment, blockNumber) and returns
// the EVM transaction hash without waiting for inclusion.
func (c *Client) SubmitAnchor(ctx context.Context, fabricTxID, commitment [32]byte, blockNumber uint64) ([32]byte, error) {
	return c.submit(ctx, packAnchor(fabricTxID, commitment, blockNumber), c.cfg.GasLimit)
}

// SubmitAnchorBatch sends anchorBatch for several entries in one transaction.
func (c *Client) SubmitAnchorBatch(ctx context.Context, ids, commitments [][32]byte, blockNumbers []uint64) ([32]byte, error) {
	data, err := packAnchorBatch(ids, commitments, blockNumbers)
	if err != nil {
		return [32]byte{}, err
	}
	gas := c.cfg.GasLimit + perBatchItemGas*uint64(len(ids))
	return c.submit(ctx, data, gas)
}

func (c *Client) submit(ctx context.Context, calldata []byte, gasLimit uint64) ([32]byte, error) {
	c.nonceMu.Lock()
	defer c.nonceMu.Unlock()

	tip := new(big.Int)
	if c.cfg.TipCapGwei > 0 {
		tip.SetUint64(c.cfg.TipCapGwei * 1e9)
	} else {
		suggested, err := c.eth.SuggestGasTipCap(ctx)
		if err != nil {
			return [32]byte{}, fmt.Errorf("evm: suggest tip: %w", err)
		}
		tip = suggested
	}
	head, err := c.eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return [32]byte{}, fmt.Errorf("evm: head: %w", err)
	}
	feeCap := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(basefeeHeadroomMult)))

	send := func(nonce uint64) (common.Hash, error) {
		tx := types.NewTx(&types.DynamicFeeTx{
			ChainID:   c.chainID,
			Nonce:     nonce,
			GasTipCap: tip,
			GasFeeCap: feeCap,
			Gas:       gasLimit,
			To:        &c.contract,
			Data:      calldata,
		})
		signed, err := types.SignTx(tx, c.signer, c.key)
		if err != nil {
			return common.Hash{}, fmt.Errorf("evm: sign: %w", err)
		}
		if err := c.eth.SendTransaction(ctx, signed); err != nil {
			return common.Hash{}, err
		}
		return signed.Hash(), nil
	}

	nonce, err := c.currentNonceLocked(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	hash, err := send(nonce)
	if err != nil && isNonceError(err) {
		// Some other process used our nonce (or a restart raced a pending
		// tx): resync once and retry.
		if err2 := c.resyncNonceLocked(ctx); err2 != nil {
			return [32]byte{}, err2
		}
		hash, err = send(c.nonce)
		nonce = c.nonce
	}
	if err != nil {
		return [32]byte{}, fmt.Errorf("evm: send: %w", err)
	}
	c.nonce = nonce + 1
	return hash, nil
}

func (c *Client) currentNonceLocked(ctx context.Context) (uint64, error) {
	if !c.nonceInited {
		if err := c.resyncNonceLocked(ctx); err != nil {
			return 0, err
		}
	}
	return c.nonce, nil
}

func (c *Client) resyncNonceLocked(ctx context.Context) error {
	n, err := c.eth.PendingNonceAt(ctx, c.sender)
	if err != nil {
		return fmt.Errorf("evm: pending nonce: %w", err)
	}
	c.nonce = n
	c.nonceInited = true
	return nil
}

func isNonceError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "nonce too low") ||
		strings.Contains(msg, "nonce too high") ||
		strings.Contains(msg, "replacement transaction underpriced") ||
		strings.Contains(msg, "already known")
}

// ErrReverted is returned when an anchor transaction was included but failed.
// The anchor contract's happy paths never revert (duplicates are no-ops), so
// a revert means misconfiguration (e.g. allowlist without this relayer).
var ErrReverted = errors.New("evm: transaction reverted")

// TxIncluded reports whether the transaction has a receipt yet.
func (c *Client) TxIncluded(ctx context.Context, txHash [32]byte) (bool, error) {
	_, err := c.eth.TransactionReceipt(ctx, common.Hash(txHash))
	if errors.Is(err, ethereum.NotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("evm: receipt: %w", err)
	}
	return true, nil
}

// WaitConfirmed blocks until txHash has the required number of confirmations
// (1 = included). It returns ErrReverted for an included-but-failed tx and
// ctx errors on timeout/cancel — the caller retries; idempotency makes that
// safe.
func (c *Client) WaitConfirmed(ctx context.Context, txHash [32]byte, confirmations uint64) error {
	if confirmations == 0 {
		confirmations = 1
	}
	ticker := time.NewTicker(c.cfg.ReceiptPollInterval)
	defer ticker.Stop()

	for {
		receipt, err := c.eth.TransactionReceipt(ctx, common.Hash(txHash))
		switch {
		case errors.Is(err, ethereum.NotFound):
			// Not yet included; keep polling.
		case err != nil:
			return fmt.Errorf("evm: receipt: %w", err)
		case receipt.Status != types.ReceiptStatusSuccessful:
			return fmt.Errorf("%w: %x", ErrReverted, txHash)
		default:
			head, err := c.eth.BlockNumber(ctx)
			if err != nil {
				return fmt.Errorf("evm: head number: %w", err)
			}
			// Reorg guard: the receipt block plus (confirmations-1)
			// descendants must exist.
			if head >= receipt.BlockNumber.Uint64()+confirmations-1 {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
