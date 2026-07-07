package sender

import (
	"context"
	"errors"
	"math/big"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeBalance struct {
	mu  sync.Mutex
	wei *big.Int
	err error
}

func (f *fakeBalance) Balance(context.Context) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return new(big.Int).Set(f.wei), nil
}

func (f *fakeBalance) set(wei *big.Int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wei, f.err = wei, err
}

func TestBalanceWatcherLowAndRecovery(t *testing.T) {
	reader := &fakeBalance{wei: GweiToWei(50)}
	w := NewBalanceWatcher(reader, GweiToWei(100), nil)
	if !w.Enabled() {
		t.Fatal("positive threshold must enable the watcher")
	}

	ctx := context.Background()
	if !w.Check(ctx) {
		t.Fatal("balance below threshold must report low")
	}
	if !w.Check(ctx) {
		t.Fatal("must keep reporting low every round while underfunded")
	}

	reader.set(GweiToWei(500), nil)
	if w.Check(ctx) {
		t.Fatal("balance above threshold must report ok")
	}
	if w.low {
		t.Fatal("recovery must clear the low state")
	}
}

func TestBalanceWatcherFetchErrorKeepsState(t *testing.T) {
	reader := &fakeBalance{wei: GweiToWei(50)}
	w := NewBalanceWatcher(reader, GweiToWei(100), nil)
	ctx := context.Background()

	if !w.Check(ctx) {
		t.Fatal("setup: expected low")
	}
	// An RPC blip must not read as a funding recovery.
	reader.set(nil, errors.New("rpc down"))
	if !w.Check(ctx) {
		t.Fatal("fetch error must keep the previous (low) state")
	}
	reader.set(GweiToWei(500), nil)
	if w.Check(ctx) {
		t.Fatal("expected recovery once the RPC is back")
	}
}

func TestBalanceWatcherDisabled(t *testing.T) {
	reader := &fakeBalance{wei: big.NewInt(0)}
	for _, threshold := range []*big.Int{nil, big.NewInt(0)} {
		w := NewBalanceWatcher(reader, threshold, nil)
		if w.Enabled() {
			t.Fatalf("threshold %v must disable the watcher", threshold)
		}
		if w.Check(context.Background()) {
			t.Fatal("disabled watcher must never report low")
		}
		// Run must return immediately for a disabled watcher.
		w.Run(context.Background(), 0)
	}
}

func TestMetricsHandlerBalanceGauge(t *testing.T) {
	store := openStore(t)
	seedPending(t, store, 1)

	// With a reader: gauge present, in gwei.
	reader := &fakeBalance{wei: GweiToWei(1234)}
	rec := httptest.NewRecorder()
	MetricsHandler(store, reader).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "mst_relayer_balance_gwei 1234") {
		t.Fatalf("gauge missing:\n%s", body)
	}
	if !strings.Contains(body, `mst_outbox_entries{status="PENDING"} 1`) {
		t.Fatalf("outbox metrics must still be present:\n%s", body)
	}

	// Reader error: scrape survives, gauge absent with a comment.
	reader.set(nil, errors.New("rpc down"))
	rec = httptest.NewRecorder()
	MetricsHandler(store, reader).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body = rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("scrape must not fail on balance errors: %d", rec.Code)
	}
	if strings.Contains(body, "\nmst_relayer_balance_gwei ") {
		t.Fatalf("gauge sample must be absent on fetch error:\n%s", body)
	}
	if !strings.Contains(body, "unavailable") {
		t.Fatalf("expected unavailability comment:\n%s", body)
	}

	// Nil reader: no gauge, no comment.
	rec = httptest.NewRecorder()
	MetricsHandler(store, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rec.Body.String(), "mst_relayer_balance") {
		t.Fatal("nil reader must not emit balance lines")
	}
}

func TestGweiToWei(t *testing.T) {
	if GweiToWei(1).Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Fatal("1 gwei must be 1e9 wei")
	}
	if got := weiToGwei(GweiToWei(2500)); got != 2500 {
		t.Fatalf("round trip: %v", got)
	}
}
