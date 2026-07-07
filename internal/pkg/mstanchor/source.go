/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"context"
	"fmt"

	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/common/flogging"
	commonledger "github.com/hyperledger/fabric/common/ledger"
	"github.com/hyperledger/fabric/core/ledger"

	"github.com/hansrajrami/fabric/mst/relay/capture"
	"github.com/hansrajrami/fabric/mst/relay/txmodel"
)

// ledgerSource implements capture.Source over the peer's own committed
// ledger: GetBlocksIterator delivers blocks strictly post-commit and blocks
// until the next block is available, which is exactly the tailing semantics
// the capture loop needs. Reading the local ledger costs no network hop and
// cannot interfere with commit.
type ledgerSource struct {
	ledger ledger.PeerLedger
	log    *flogging.FabricLogger
}

var _ capture.Source = (*ledgerSource)(nil)

func newLedgerSource(l ledger.PeerLedger, log *flogging.FabricLogger) *ledgerSource {
	return &ledgerSource{ledger: l, log: log}
}

// Blocks streams parsed blocks from startBlock. The iterator's blocking
// Next() has no context awareness, so a watcher goroutine closes it when ctx
// ends; a closed iterator returns (nil, nil), which cleanly ends the stream.
func (s *ledgerSource) Blocks(ctx context.Context, startBlock uint64) (<-chan *txmodel.Block, error) {
	iter, err := s.ledger.GetBlocksIterator(startBlock)
	if err != nil {
		return nil, fmt.Errorf("mstanchor: blocks iterator at %d: %w", startBlock, err)
	}

	out := make(chan *txmodel.Block)
	go func() {
		<-ctx.Done()
		iter.Close()
	}()
	go func() {
		defer close(out)
		for {
			result, err := next(iter)
			if err != nil {
				s.log.Errorw("ledger block iterator failed", "err", err)
				return
			}
			if result == nil {
				return // iterator closed (shutdown)
			}
			parsed, err := ParseBlock(result)
			if err != nil {
				s.log.Errorw("unparseable committed block", "err", err)
				return
			}
			select {
			case out <- parsed:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func next(iter commonledger.ResultsIterator) (*common.Block, error) {
	result, err := iter.Next()
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	block, ok := result.(*common.Block)
	if !ok {
		return nil, fmt.Errorf("mstanchor: unexpected iterator result %T", result)
	}
	return block, nil
}
