/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/core/ledger"

	"github.com/hansrajrami/fabric/mst/relay/capture"
	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

var logger = flogging.MustGetLogger("mstanchor")

// channelDiscoveryInterval is how often newly joined channels are picked up.
const channelDiscoveryInterval = 10 * time.Second

// PeerLedgers is the narrow slice of the peer the service needs: which
// channels exist and their ledgers. *peer.Peer satisfies it.
type PeerLedgers interface {
	GetLedger(channelID string) ledger.PeerLedger
}

// ChannelIDLister enumerates the peer's joined channel ids.
type ChannelIDLister func() []string

// Service runs one anchoring pipeline per participating channel. Each
// channel gets its own outbox directory (block numbers — and therefore
// checkpoints — are per-channel); the EVM client, with its single nonce
// sequence, is shared.
type Service struct {
	cfg       *Config
	peer      PeerLedgers
	channels  ChannelIDLister
	client    *evm.Client
	writeback sender.WriteBack

	mu        sync.Mutex
	pipelines map[string]*pipeline
	stores    []outbox.Store
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	metricsLn *http.Server
}

type pipeline struct {
	channelID string
	store     outbox.Store
}

// New builds the embedded service. writeback may be nil (no write-back
// until the anchor-status chaincode is configured; see NewLoopbackWriteBack).
func New(cfg *Config, peer PeerLedgers, channels ChannelIDLister, writeback sender.WriteBack) *Service {
	return &Service{
		cfg:       cfg,
		peer:      peer,
		channels:  channels,
		writeback: writeback,
		pipelines: map[string]*pipeline{},
	}
}

// Start dials the MST chain and launches the channel-discovery loop. It
// fails fast on misconfiguration (bad key, unreachable RPC is tolerated —
// the sender retries) so a broken embedded config is visible at peer boot.
func (s *Service) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	evmCfg, err := s.cfg.EVMConfig()
	if err != nil {
		cancel()
		return err
	}
	client, err := evm.Dial(ctx, evmCfg)
	if err != nil {
		cancel()
		return fmt.Errorf("mstanchor: %w", err)
	}
	s.client = client
	logger.Infow("MST anchoring enabled (embedded)", "sender", client.Sender(),
		"contract", s.cfg.EVM.ContractAddress, "outbox", s.cfg.OutboxPath)

	if s.cfg.MetricsAddr != "" {
		s.startMetrics()
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.discoverLoop(ctx)
	}()
	return nil
}

// Stop shuts the pipelines down and closes the outboxes and EVM client.
func (s *Service) Stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, store := range s.stores {
		_ = store.Close()
	}
	if s.client != nil {
		s.client.Close()
	}
	if s.metricsLn != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.metricsLn.Shutdown(shutdownCtx)
		cancel()
	}
	logger.Info("MST anchoring stopped")
}

// discoverLoop starts a pipeline for every participating channel, then
// keeps watching for newly joined channels.
func (s *Service) discoverLoop(ctx context.Context) {
	ticker := time.NewTicker(channelDiscoveryInterval)
	defer ticker.Stop()
	for {
		s.startNewPipelines(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) startNewPipelines(ctx context.Context) {
	for _, channelID := range s.channels() {
		if !s.cfg.channelAllowed(channelID) {
			continue
		}
		s.mu.Lock()
		_, running := s.pipelines[channelID]
		s.mu.Unlock()
		if running {
			continue
		}
		if err := s.startPipeline(ctx, channelID); err != nil {
			logger.Errorw("failed to start anchoring pipeline", "channel", channelID, "err", err)
		}
	}
}

func (s *Service) startPipeline(ctx context.Context, channelID string) error {
	l := s.peer.GetLedger(channelID)
	if l == nil {
		return fmt.Errorf("no ledger for channel %s", channelID)
	}

	// The outbox backend follows the peer's state database: CouchDB peers
	// keep the outbox on the same CouchDB server (own databases), LevelDB
	// peers keep it embedded on local disk.
	var store outbox.Store
	var err error
	if s.cfg.Outbox.Backend == "couchdb" {
		store, err = outbox.OpenCouchDB(outbox.CouchDBOptions{
			URL:      s.cfg.Outbox.CouchDB.Address,
			Username: s.cfg.Outbox.CouchDB.Username,
			Password: s.cfg.Outbox.CouchDB.Password,
			Database: couchDatabaseName(channelID),
		})
	} else {
		store, err = outbox.Open(filepath.Join(s.cfg.OutboxPath, channelID), nil)
	}
	if err != nil {
		return err
	}

	snd, err := sender.New(store, s.client, s.writeback, s.cfg.SenderConfig(), nil)
	if err != nil {
		store.Close()
		return err
	}
	source := newLedgerSource(l, logger.With("channel", channelID))

	s.mu.Lock()
	s.pipelines[channelID] = &pipeline{channelID: channelID, store: store}
	s.stores = append(s.stores, store)
	s.mu.Unlock()

	// Capture loop with restart backoff: an iterator failure must not end
	// anchoring for the channel.
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		backoff := time.Second
		for {
			svc := capture.New(source, store, s.cfg.CaptureConfig(), nil)
			err := svc.Run(ctx)
			if ctx.Err() != nil {
				return
			}
			logger.Errorw("capture stream ended; restarting", "channel", channelID, "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
	go func() {
		defer s.wg.Done()
		if err := snd.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Errorw("sender stopped", "channel", channelID, "err", err)
		}
	}()

	logger.Infow("anchoring pipeline started", "channel", channelID)
	return nil
}

func (s *Service) startMetrics() {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		pipelines := make([]*pipeline, 0, len(s.pipelines))
		for _, p := range s.pipelines {
			pipelines = append(pipelines, p)
		}
		s.mu.Unlock()
		for _, p := range pipelines {
			fmt.Fprintf(w, "# channel %s\n", p.channelID)
			sender.MetricsHandler(p.store).ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: s.cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	s.metricsLn = srv
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Errorw("mst metrics server failed", "err", err)
		}
	}()
	logger.Infow("mst metrics listening", "addr", s.cfg.MetricsAddr)
}
