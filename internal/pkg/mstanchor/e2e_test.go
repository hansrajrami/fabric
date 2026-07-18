/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-chaincode-go/shimtest"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/hyperledger/fabric/common/flogging"
	commonledger "github.com/hyperledger/fabric/common/ledger"
	"github.com/hyperledger/fabric/core/scc/mstscc"
	"github.com/stretchr/testify/require"

	"github.com/hansrajrami/fabric/mst/relay/capture"
	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

// This file is a SIMULATED end-to-end test of the Phase 1.5 pipeline. The two
// external systems are the only fakes:
//
//   - the MST chain is an in-memory simMSTChain keyed by contract address, so
//     each channel's per-channel contract is observable and isolated (this
//     mirrors what evm.Binding does over a real chain);
//   - the Fabric write-back target is the REAL mstscc system chaincode, driven
//     through a shimtest.MockStub.
//
// Everything between — capture, the durable outbox, and the sender — is the
// real production code. No go-ethereum simulated backend is vendored, so this
// in-memory approach (the same one mst/relay/sender/sender_test.go uses) is the
// faithful, dependency-free way to exercise the whole flow.

// ---------------------------------------------------------------------------
// Simulated MST chain (external system #1)
// ---------------------------------------------------------------------------

// simMSTChain is an in-memory stand-in for the MST blockchain. Anchors are
// stored per contract address so that per-channel contract isolation is
// directly observable.
type simMSTChain struct {
	mu       sync.Mutex
	anchored map[string]map[[32]byte]*evm.AnchorRecord // contract -> fabricTxID -> record
	receipts map[[32]byte]bool                         // evm tx hash -> included
	next     byte
}

func newSimMSTChain() *simMSTChain {
	return &simMSTChain{
		anchored: map[string]map[[32]byte]*evm.AnchorRecord{},
		receipts: map[[32]byte]bool{},
	}
}

func (c *simMSTChain) get(contract string, fabricTxID [32]byte) *evm.AnchorRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := c.anchored[contract]; m != nil {
		return m[fabricTxID]
	}
	return nil
}

func (c *simMSTChain) submit(contract string, fabricTxID, commitment [32]byte, blockNumber uint64) [32]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	var hash [32]byte
	hash[0] = 0xE0
	hash[31] = c.next
	c.receipts[hash] = true
	m := c.anchored[contract]
	if m == nil {
		m = map[[32]byte]*evm.AnchorRecord{}
		c.anchored[contract] = m
	}
	if _, exists := m[fabricTxID]; !exists { // contract idempotency: first write wins
		m[fabricTxID] = &evm.AnchorRecord{Commitment: commitment, BlockNumber: blockNumber, EVMTimestamp: 1, Exists: true}
	}
	return hash
}

// boundSimClient pairs the shared simMSTChain with one contract address — the
// in-memory analogue of evm.Binding. It satisfies sender.AnchorClient.
type boundSimClient struct {
	chain    *simMSTChain
	contract string
}

var _ sender.AnchorClient = (*boundSimClient)(nil)

func (b *boundSimClient) GetAnchor(_ context.Context, fabricTxID [32]byte) (*evm.AnchorRecord, error) {
	return b.chain.get(b.contract, fabricTxID), nil
}

func (b *boundSimClient) SubmitAnchor(_ context.Context, fabricTxID, commitment [32]byte, blockNumber uint64) ([32]byte, error) {
	return b.chain.submit(b.contract, fabricTxID, commitment, blockNumber), nil
}

func (b *boundSimClient) SubmitAnchorBatch(_ context.Context, ids, commitments [][32]byte, blockNumbers []uint64) ([32]byte, error) {
	var last [32]byte
	for i := range ids {
		last = b.chain.submit(b.contract, ids[i], commitments[i], blockNumbers[i])
	}
	return last, nil
}

func (b *boundSimClient) SubmitAnchorRoot(context.Context, [32]byte, uint64) ([32]byte, error) {
	return [32]byte{}, fmt.Errorf("simMSTChain: root anchoring not used in this test")
}

func (b *boundSimClient) GetRoot(context.Context, [32]byte) (*evm.RootRecord, error) {
	return nil, nil
}

func (b *boundSimClient) TxIncluded(_ context.Context, txHash [32]byte) (bool, error) {
	b.chain.mu.Lock()
	defer b.chain.mu.Unlock()
	return b.chain.receipts[txHash], nil
}

func (b *boundSimClient) WaitConfirmed(_ context.Context, txHash [32]byte, _ uint64) error {
	b.chain.mu.Lock()
	defer b.chain.mu.Unlock()
	if !b.chain.receipts[txHash] {
		return fmt.Errorf("simMSTChain: tx %x never landed", txHash)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Write-back into the REAL mstscc chaincode (external system #2)
// ---------------------------------------------------------------------------

// sccWriteBack is a sender.WriteBack that records anchor status by invoking the
// real mstscc system chaincode through a MockStub — the in-memory analogue of
// the peer's LoopbackWriteBack. MockStub is not goroutine-safe, so a mutex
// serialises invocations.
type sccWriteBack struct {
	mu        sync.Mutex
	stub      *shimtest.MockStub
	channelID string
	calls     int
}

var _ sender.WriteBack = (*sccWriteBack)(nil)

func (w *sccWriteBack) Record(_ context.Context, e *outbox.Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	w.stub.ChannelID = w.channelID
	args := [][]byte{
		[]byte(mstscc.RecordAnchor),
		[]byte(hex.EncodeToString(e.FabricTxID[:])),
		[]byte("0x" + hex.EncodeToString(e.EVMTxHash[:])),
		[]byte(mstscc.StatusConfirmed),
	}
	res := w.stub.MockInvoke(fmt.Sprintf("wb-%d", w.calls), args)
	if res.Status != int32(200) {
		return fmt.Errorf("mstscc write-back rejected: %s", res.Message)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// enabledGetter builds a ChannelConfigGetter that reports the given channels as
// MST-enabled with the given per-channel contract addresses.
func enabledGetter(contracts map[string]string) mstscc.ChannelConfigGetter {
	apps := map[string]channelconfig.Application{}
	for ch, addr := range contracts {
		apps[ch] = mstApp{cfg: &channelconfig.MSTAnchorConfig{Enabled: true, ContractAddress: addr}}
	}
	return staticGetter{apps: apps}
}

type mstApp struct {
	cfg *channelconfig.MSTAnchorConfig
}

func (mstApp) Organizations() map[string]channelconfig.ApplicationOrg { return nil }
func (mstApp) APIPolicyMapper() channelconfig.PolicyMapper            { return nil }
func (mstApp) Capabilities() channelconfig.ApplicationCapabilities    { return nil }
func (a mstApp) MSTAnchorConfig() (*channelconfig.MSTAnchorConfig, bool) {
	if a.cfg == nil {
		return nil, false
	}
	return a.cfg, true
}

type staticGetter struct {
	apps map[string]channelconfig.Application
}

func (g staticGetter) GetApplicationConfig(cid string) (channelconfig.Application, bool) {
	a, ok := g.apps[cid]
	return a, ok
}

// captureBlock runs the real capture service over a single block via the fake
// ledger, returning the single PENDING outbox entry it produced.
func captureBlock(t *testing.T, store outbox.Store, block *commonledger.QueryResult) *outbox.Entry {
	t.Helper()
	iter := &fakeIterator{results: []commonledger.QueryResult{*block}, closed: make(chan struct{})}
	source := newLedgerSource(&fakeLedger{iter: iter}, flogging.MustGetLogger("e2e"))

	svc := capture.New(source, store, capture.Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	var pending []*outbox.Entry
	require.Eventually(t, func() bool {
		var err error
		pending, err = store.ListByStatus(outbox.StatusPending, 0)
		return err == nil && len(pending) == 1
	}, 5*time.Second, 10*time.Millisecond, "capture must produce exactly one pending entry")

	cancel()
	require.NoError(t, <-done)
	return pending[0]
}

// perTxFlush drains the outbox once with a single worker (MockStub is not
// concurrency-safe) and the default per-tx cadence.
func perTxFlush(t *testing.T, store outbox.Store, client sender.AnchorClient, wb sender.WriteBack) {
	t.Helper()
	snd, err := sender.New(store, client, wb, sender.Config{Workers: 1}, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, snd.FlushOnce(ctx))
}

func newProofBlock(t *testing.T, number uint64, txSeed, channel string) *commonledger.QueryResult {
	t.Helper()
	block := buildBlock(t, number, txSpec{
		txID: testTxID(txSeed), channel: channel, ts: 1720000001, valid: true,
		chaincode: "mst-example", eventName: "MSTProofRequest", payload: []byte{0, 0, 0, 0},
	})
	var qr commonledger.QueryResult = block
	return &qr
}

func statusOf(t *testing.T, store outbox.Store, txID [32]byte) outbox.Status {
	t.Helper()
	e, err := store.Get(txID)
	require.NoError(t, err)
	return e.Status
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestE2EHappyPath drives one opted-in transaction the whole way: block →
// capture → outbox → sender → MST anchor → mstscc ledger fact.
func TestE2EHappyPath(t *testing.T) {
	const channel = "channel-a"
	contract := "0x1111111111111111111111111111111111111111"

	store, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	require.NoError(t, err)
	defer store.Close()

	entry := captureBlock(t, store, newProofBlock(t, 0, "a1", channel))
	require.Equal(t, outbox.StatusPending, entry.Status)
	require.Equal(t, channel, entry.ChannelID)

	chain := newSimMSTChain()
	scc := shimtest.NewMockStub("mstscc", mstscc.New(nil, enabledGetter(map[string]string{channel: contract})))
	wb := &sccWriteBack{stub: scc, channelID: channel}

	perTxFlush(t, store, &boundSimClient{chain: chain, contract: contract}, wb)

	// 1. The outbox entry reached the terminal DONE state. Re-fetch it: the
	// sender populated EVMTxHash during submission (the captured entry was
	// still PENDING with a zero hash).
	final, err := store.Get(entry.FabricTxID)
	require.NoError(t, err)
	require.Equal(t, outbox.StatusDone, final.Status)

	// 2. The anchor landed on THIS channel's contract with the captured commitment.
	rec := chain.get(contract, entry.FabricTxID)
	require.NotNil(t, rec, "anchor must exist on the channel's own contract")
	require.Equal(t, entry.Commitment, rec.Commitment)

	// 3. The mstscc ledger holds the anchor status as a queryable fact.
	scc.ChannelID = channel
	txHex := hex.EncodeToString(entry.FabricTxID[:])
	q := scc.MockInvoke("q", [][]byte{[]byte(mstscc.IsAnchored), []byte(txHex)})
	require.Equal(t, int32(200), q.Status)
	require.Equal(t, "true", string(q.Payload))

	q = scc.MockInvoke("q2", [][]byte{[]byte(mstscc.QueryAnchorStatus), []byte(txHex)})
	require.Equal(t, int32(200), q.Status, q.Message)
	var rec2 mstscc.AnchorStatus
	require.NoError(t, json.Unmarshal(q.Payload, &rec2))
	require.Equal(t, strings.ToLower(txHex), rec2.FabricTxID)
	require.Equal(t, "0x"+hex.EncodeToString(final.EVMTxHash[:]), rec2.AnchorRef)
	require.Equal(t, mstscc.StatusConfirmed, rec2.Status)
}

// TestE2EPerChannelIsolation proves each channel anchors to its OWN contract.
func TestE2EPerChannelIsolation(t *testing.T) {
	const chanA, chanB = "channel-a", "channel-b"
	contractA := "0xaAaAaAAAaaaaAaAaAAAaaaaaAaaaAAaAaaAAAaAa"
	contractB := "0xBbBBBBBBbbBBBbbbBBBBbbBbBbBbbBbBbbBBBBbB"

	chain := newSimMSTChain()
	getter := enabledGetter(map[string]string{chanA: contractA, chanB: contractB})
	scc := shimtest.NewMockStub("mstscc", mstscc.New(nil, getter))

	// Channel A.
	storeA, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	require.NoError(t, err)
	defer storeA.Close()
	entryA := captureBlock(t, storeA, newProofBlock(t, 0, "a1", chanA))
	perTxFlush(t, storeA, &boundSimClient{chain: chain, contract: contractA}, &sccWriteBack{stub: scc, channelID: chanA})

	// Channel B.
	storeB, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	require.NoError(t, err)
	defer storeB.Close()
	entryB := captureBlock(t, storeB, newProofBlock(t, 0, "b1", chanB))
	perTxFlush(t, storeB, &boundSimClient{chain: chain, contract: contractB}, &sccWriteBack{stub: scc, channelID: chanB})

	require.Equal(t, outbox.StatusDone, statusOf(t, storeA, entryA.FabricTxID))
	require.Equal(t, outbox.StatusDone, statusOf(t, storeB, entryB.FabricTxID))

	// Each anchor is only on its own channel's contract.
	require.NotNil(t, chain.get(contractA, entryA.FabricTxID), "A's anchor on contract A")
	require.NotNil(t, chain.get(contractB, entryB.FabricTxID), "B's anchor on contract B")
	require.Nil(t, chain.get(contractB, entryA.FabricTxID), "A's anchor must NOT appear on contract B")
	require.Nil(t, chain.get(contractA, entryB.FabricTxID), "B's anchor must NOT appear on contract A")
}

// TestE2EChannelConfigGate proves the write-back only becomes a ledger fact when
// the channel's configuration governs MST anchoring on: a disabled channel's
// RecordAnchor is rejected by the SCC, so the entry never finishes.
func TestE2EChannelConfigGate(t *testing.T) {
	const channel = "channel-off"
	contract := "0x2222222222222222222222222222222222222222"

	store, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	require.NoError(t, err)
	defer store.Close()
	entry := captureBlock(t, store, newProofBlock(t, 0, "c1", channel))

	chain := newSimMSTChain()
	// The channel is DISABLED in its config (Enabled: false).
	getter := staticGetter{apps: map[string]channelconfig.Application{
		channel: mstApp{cfg: &channelconfig.MSTAnchorConfig{Enabled: false}},
	}}
	scc := shimtest.NewMockStub("mstscc", mstscc.New(nil, getter))
	wb := &sccWriteBack{stub: scc, channelID: channel}

	perTxFlush(t, store, &boundSimClient{chain: chain, contract: contract}, wb)

	// The anchor was submitted to MST, but the SCC rejected the write-back, so
	// the entry is stuck at CONFIRMED (not DONE) and there is no ledger fact.
	require.Equal(t, outbox.StatusConfirmed, statusOf(t, store, entry.FabricTxID))
	require.NotNil(t, chain.get(contract, entry.FabricTxID), "anchor was still submitted to MST")

	scc.ChannelID = channel
	q := scc.MockInvoke("q", [][]byte{[]byte(mstscc.IsAnchored), []byte(hex.EncodeToString(entry.FabricTxID[:]))})
	require.Equal(t, int32(500), q.Status, "querying a disabled channel is rejected by the gate")
}

// captureWithConfig runs capture over one block with the given (per-channel)
// capture config and returns once the block has been processed (checkpoint
// advanced), regardless of how many entries it produced.
func captureWithConfig(t *testing.T, store outbox.Store, cfg capture.Config, block *commonledger.QueryResult) {
	t.Helper()
	iter := &fakeIterator{results: []commonledger.QueryResult{*block}, closed: make(chan struct{})}
	source := newLedgerSource(&fakeLedger{iter: iter}, flogging.MustGetLogger("e2e"))
	svc := capture.New(source, store, cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	require.Eventually(t, func() bool {
		next, ok, err := store.Checkpoint()
		return err == nil && ok && next >= 1
	}, 5*time.Second, 10*time.Millisecond, "block must be processed")
	cancel()
	require.NoError(t, <-done)
}

// TestE2EPerChannelCaptureScope proves the promoted capture scope is read from
// channel config and actually changes what gets anchored: the same plain
// (non-opted-in) transaction is captured on an "all" channel but not on an
// "opt-in" channel — using the peer's CaptureConfigFor builder end to end.
func TestE2EPerChannelCaptureScope(t *testing.T) {
	peerCfg := &Config{} // only peer-local knobs; scope comes from the channel

	plainBlock := func() *commonledger.QueryResult {
		// No eventName → not opted in.
		block := buildBlock(t, 0, txSpec{
			txID: testTxID("51"), channel: "ch", ts: 1720000001, valid: true,
			chaincode: "mst-example",
		})
		var qr commonledger.QueryResult = block
		return &qr
	}

	// Channel configured "all": the plain tx IS captured.
	storeAll, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	require.NoError(t, err)
	defer storeAll.Close()
	captureWithConfig(t, storeAll, peerCfg.CaptureConfigFor(&channelconfig.MSTAnchorConfig{CaptureMode: "all"}), plainBlock())
	pendingAll, err := storeAll.ListByStatus(outbox.StatusPending, 0)
	require.NoError(t, err)
	require.Len(t, pendingAll, 1, `"all" mode anchors the plain transaction`)

	// Channel configured "opt-in" (default): the same plain tx is NOT captured.
	storeOpt, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	require.NoError(t, err)
	defer storeOpt.Close()
	captureWithConfig(t, storeOpt, peerCfg.CaptureConfigFor(&channelconfig.MSTAnchorConfig{CaptureMode: "opt-in"}), plainBlock())
	pendingOpt, err := storeOpt.ListByStatus(outbox.StatusPending, 0)
	require.NoError(t, err)
	require.Empty(t, pendingOpt, `"opt-in" mode ignores the non-opted transaction`)
}

// TestE2EEchoLoopGuard proves the write-back is idempotent and never emits an
// event, so it can never re-enter the capture pipeline.
func TestE2EEchoLoopGuard(t *testing.T) {
	const channel = "channel-a"
	contract := "0x3333333333333333333333333333333333333333"

	store, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	require.NoError(t, err)
	defer store.Close()
	entry := captureBlock(t, store, newProofBlock(t, 0, "e1", channel))

	chain := newSimMSTChain()
	scc := shimtest.NewMockStub("mstscc", mstscc.New(nil, enabledGetter(map[string]string{channel: contract})))
	wb := &sccWriteBack{stub: scc, channelID: channel}
	perTxFlush(t, store, &boundSimClient{chain: chain, contract: contract}, wb)
	require.Equal(t, outbox.StatusDone, statusOf(t, store, entry.FabricTxID))

	// Re-invoking RecordAnchor for the same tx is a quiet, idempotent no-op.
	scc.ChannelID = channel
	res := scc.MockInvoke("again", [][]byte{
		[]byte(mstscc.RecordAnchor),
		[]byte(hex.EncodeToString(entry.FabricTxID[:])),
		[]byte("0x" + hex.EncodeToString(entry.EVMTxHash[:])),
		[]byte(mstscc.StatusConfirmed),
	})
	require.Equal(t, int32(200), res.Status)

	// The write-back never emits a chaincode event: nothing can re-enter capture.
	select {
	case ev := <-scc.ChaincodeEventsChannel:
		t.Fatalf("mstscc must never emit an event, got %v", ev)
	default:
	}
}
