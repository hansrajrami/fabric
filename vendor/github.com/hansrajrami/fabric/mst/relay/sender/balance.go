package sender

import (
	"context"
	"log/slog"
	"math/big"
	"time"
)

// BalanceReader reports the relayer account's balance in wei; *evm.Client
// satisfies it.
type BalanceReader interface {
	Balance(ctx context.Context) (*big.Int, error)
}

// gweiPerWei converts for display: Prometheus values are float64, and gwei
// magnitudes stay well within float64 precision where wei would not.
var gweiInWei = big.NewInt(1_000_000_000)

// weiToGwei renders a wei balance as gwei for logs and metrics.
func weiToGwei(wei *big.Int) float64 {
	gwei, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), new(big.Float).SetInt(gweiInWei)).Float64()
	return gwei
}

// defaultBalanceInterval is how often the watcher re-checks the balance.
const defaultBalanceInterval = time.Minute

// BalanceWatcher periodically checks the relayer's gas balance and logs
// loudly while it sits below the configured threshold: running out of gas is
// the most likely real-world anchoring outage, and it is otherwise silent
// (entries simply queue in the outbox). Operators alert on the error log
// pattern and/or on the mst_relayer_balance_gwei metrics gauge.
type BalanceWatcher struct {
	reader    BalanceReader
	threshold *big.Int // wei; nil or zero disables the watcher
	log       *slog.Logger

	low bool // current state, for single recovery notices
}

// NewBalanceWatcher builds a watcher. thresholdWei nil or zero means
// monitoring is disabled (Run returns immediately); the metrics gauge is
// independent of this and always exposed when a reader is wired.
func NewBalanceWatcher(reader BalanceReader, thresholdWei *big.Int, log *slog.Logger) *BalanceWatcher {
	if log == nil {
		log = slog.Default()
	}
	return &BalanceWatcher{reader: reader, threshold: thresholdWei, log: log}
}

// Enabled reports whether a positive threshold is configured.
func (w *BalanceWatcher) Enabled() bool {
	return w.reader != nil && w.threshold != nil && w.threshold.Sign() > 0
}

// Check performs one balance round. It returns whether the balance is
// currently below the threshold (false on fetch errors, which are logged
// and never fatal — transient RPC failures must not crash the loop).
func (w *BalanceWatcher) Check(ctx context.Context) bool {
	if !w.Enabled() {
		return false
	}
	balance, err := w.reader.Balance(ctx)
	if err != nil {
		// Keep the previous state: an RPC blip is not a funding recovery.
		w.log.Warn("relayer balance check failed", "err", err)
		return w.low
	}

	if balance.Cmp(w.threshold) < 0 {
		// Log EVERY round while low: alerting pipelines key on recurring
		// error patterns, and a single line at crossing is easy to miss.
		w.low = true
		w.log.Error("relayer gas balance below threshold — anchoring will stall when it runs out; fund the relayer account",
			"balanceGwei", weiToGwei(balance), "thresholdGwei", weiToGwei(w.threshold))
		return true
	}
	if w.low {
		w.low = false
		w.log.Info("relayer gas balance recovered above threshold",
			"balanceGwei", weiToGwei(balance), "thresholdGwei", weiToGwei(w.threshold))
	}
	return false
}

// Run checks on the given interval (default one minute) until ctx ends.
// A disabled watcher returns immediately.
func (w *BalanceWatcher) Run(ctx context.Context, interval time.Duration) {
	if !w.Enabled() {
		return
	}
	if interval <= 0 {
		interval = defaultBalanceInterval
	}
	w.Check(ctx) // immediate first check: catch an unfunded account at boot
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Check(ctx)
		}
	}
}

// GweiToWei converts a configured gwei threshold to wei.
func GweiToWei(gwei uint64) *big.Int {
	return new(big.Int).Mul(new(big.Int).SetUint64(gwei), gweiInWei)
}
