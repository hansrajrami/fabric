/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric/common/flogging"
	commonledger "github.com/hyperledger/fabric/common/ledger"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/stretchr/testify/require"

	"github.com/hansrajrami/fabric/mst/relay/capture"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

// fakeIterator mimics the block-store iterator: serves queued blocks, then
// blocks on Next() until Close() (which makes Next return nil, nil).
type fakeIterator struct {
	results []commonledger.QueryResult
	pos     int
	mu      sync.Mutex
	closed  chan struct{}
}

func (f *fakeIterator) Next() (commonledger.QueryResult, error) {
	f.mu.Lock()
	if f.pos < len(f.results) {
		r := f.results[f.pos]
		f.pos++
		f.mu.Unlock()
		return r, nil
	}
	f.mu.Unlock()
	<-f.closed // tail: wait for close, like the real blocks iterator
	return nil, nil
}

func (f *fakeIterator) Close() {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
}

// fakeLedger provides only GetBlocksIterator; every other PeerLedger method
// panics via the embedded nil interface if the source starts using more.
type fakeLedger struct {
	ledger.PeerLedger
	iter *fakeIterator
}

func (f *fakeLedger) GetBlocksIterator(startBlock uint64) (commonledger.ResultsIterator, error) {
	return f.iter, nil
}

func TestLedgerSourceFeedsCaptureEndToEnd(t *testing.T) {
	payload := []byte{0, 0, 0, 0} // canonical empty payload
	block := buildBlock(
		t, 0,
		txSpec{
			txID: testTxID("a1"), channel: "mychannel", ts: 1720000001, valid: true,
			chaincode: "mst-example", eventName: "MSTProofRequest", payload: payload,
		},
	)
	iter := &fakeIterator{results: []commonledger.QueryResult{block}, closed: make(chan struct{})}
	source := newLedgerSource(&fakeLedger{iter: iter}, flogging.MustGetLogger("test"))

	store, err := outbox.Open(t.TempDir(), &outbox.Options{NoSync: true})
	require.NoError(t, err)
	defer store.Close()

	svc := capture.New(source, store, capture.Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	require.Eventually(t, func() bool {
		pending, err := store.ListByStatus(outbox.StatusPending, 0)
		return err == nil && len(pending) == 1
	}, 5*time.Second, 10*time.Millisecond, "block from the ledger iterator must reach the outbox")

	next, ok, err := store.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint64(1), next)

	// Shutdown: cancel closes the iterator, the stream ends, Run returns nil.
	cancel()
	require.NoError(t, <-done)
}
