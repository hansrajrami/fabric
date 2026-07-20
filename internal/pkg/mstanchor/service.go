/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyperledger/fabric/common/channelconfig"
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
// channels exist, their ledgers, their parsed application config (for the
// channel-level MST anchoring settings), and their full channel config (for the
// MSPs' NodeOUs settings). *peer.Peer satisfies it.
type PeerLedgers interface {
	GetLedger(channelID string) ledger.PeerLedger
	GetApplicationConfig(cid string) (channelconfig.Application, bool)
	GetChannelConfig(cid string) channelconfig.Resources
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
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	metricsLn *http.Server
	// warned tracks channels for which a config/allowlist mismatch has already
	// been logged, so the 10s discovery loop does not spam the log.
	warned map[string]struct{}
	// warnedNodeOUs is the same, kept separate so the NodeOUs advisory warning
	// (which does not skip anchoring) neither swallows nor is swallowed by the
	// allowlist/chainID warnings above.
	warnedNodeOUs map[string]struct{}
}

type pipeline struct {
	channelID string
	store     outbox.Store
	// cancel stops just this channel's capture+sender goroutines; done waits for
	// them so the store is closed only after they have stopped using it.
	cancel context.CancelFunc
	done   *sync.WaitGroup
	// fp is the fingerprint of the channel-governed config this pipeline was
	// started with; a change means the config was updated and the pipeline is
	// hot-reloaded.
	fp string
}

// New builds the embedded service. writeback may be nil (no write-back
// until the anchor-status chaincode is configured; see NewLoopbackWriteBack).
func New(cfg *Config, peer PeerLedgers, channels ChannelIDLister, writeback sender.WriteBack) *Service {
	return &Service{
		cfg:           cfg,
		peer:          peer,
		channels:      channels,
		writeback:     writeback,
		pipelines:     map[string]*pipeline{},
		warned:        map[string]struct{}{},
		warnedNodeOUs: map[string]struct{}{},
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
	logger.Infow("MST anchoring enabled (embedded); contracts are per-channel from channel config",
		"sender", client.Sender(), "outbox", s.cfg.OutboxPath)

	if s.cfg.MetricsAddr != "" {
		s.startMetrics()
	}

	// Gas-balance watcher: an underfunded relayer account stalls anchoring
	// safely but silently (entries queue in the outboxes) — make it loud.
	watcher := sender.NewBalanceWatcher(client, sender.GweiToWei(s.cfg.EVM.MinBalanceGwei), nil)
	if watcher.Enabled() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			watcher.Run(ctx, 0)
		}()
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
	s.wg.Wait() // drains the discover loop, balance watcher, and all pipeline goroutines
	s.mu.Lock()
	defer s.mu.Unlock()
	// All pipeline goroutines have stopped (s.wg.Wait above), so their stores are
	// safe to close.
	for _, p := range s.pipelines {
		_ = p.store.Close()
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

// startNewPipelines reconciles running pipelines with the channels' current
// configuration on every discovery tick: it starts a pipeline for a newly-enabled
// channel, stops one whose channel disabled anchoring (or that this peer no longer
// anchors), and hot-reloads one whose channel-governed config changed — so a
// channel-config update takes effect live, not only on peer restart.
func (s *Service) startNewPipelines(ctx context.Context) {
	for _, channelID := range s.channels() {
		// Enablement is authoritative from the channel's own configuration
		// (all orgs agreeing through the Application group's modification
		// policy), not this peer's core.yaml.
		mstCfg, ok := s.channelMSTConfig(channelID)
		want := ok && mstCfg.Enabled

		// The core.yaml mst.channels allowlist is a peer-local secondary filter:
		// a peer may decline to anchor a channel even though the channel enabled
		// it. Surface that mismatch loudly (once).
		if want && !s.cfg.channelAllowed(channelID) {
			s.warnOnce(channelID, "channel enabled MST anchoring in its config but this peer's mst.channels allowlist excludes it; not anchoring")
			want = false
		}
		// A peer has one RPC endpoint and therefore one chain. If the channel
		// declares a different chain id than the peer is connected to, anchoring
		// it would write to the wrong chain — refuse.
		if want && mstCfg.ChainID != 0 && s.client.ChainID() != 0 && mstCfg.ChainID != s.client.ChainID() {
			s.warnOnce(channelID, fmt.Sprintf("channel MST chainID %d does not match the peer's connected chain %d; not anchoring", mstCfg.ChainID, s.client.ChainID()))
			want = false
		}

		s.mu.Lock()
		running, isRunning := s.pipelines[channelID]
		var runningFP string
		if isRunning {
			runningFP = running.fp
		}
		s.mu.Unlock()

		var desiredFP string
		if want {
			desiredFP = pipelineFingerprint(mstCfg)
		}

		switch planPipelineAction(isRunning, runningFP, want, desiredFP) {
		case actionStart:
			// The mstscc write-back gate authorizes only peer-role identities;
			// warn (once) if the write-back org lacks NodeOUs peer classification.
			s.warnIfWritebackOrgLacksNodeOUs(channelID)
			if err := s.startPipeline(ctx, channelID, mstCfg); err != nil {
				logger.Errorw("failed to start anchoring pipeline", "channel", channelID, "err", err)
			}
		case actionRestart:
			logger.Infow("channel MST config changed; reloading pipeline", "channel", channelID)
			s.stopPipeline(channelID)
			if err := s.startPipeline(ctx, channelID, mstCfg); err != nil {
				logger.Errorw("failed to reload anchoring pipeline", "channel", channelID, "err", err)
			}
		case actionStop:
			logger.Infow("MST anchoring no longer active for channel; stopping pipeline", "channel", channelID)
			s.stopPipeline(channelID)
		case actionNone:
		}
	}
}

type pipelineAction int

const (
	actionNone pipelineAction = iota
	actionStart
	actionStop
	actionRestart
)

// planPipelineAction decides what to do for one channel given whether a pipeline
// is running (and the config fingerprint it runs with), whether this peer should
// be anchoring the channel now (want), and the desired fingerprint.
func planPipelineAction(running bool, runningFP string, want bool, desiredFP string) pipelineAction {
	switch {
	case want && !running:
		return actionStart
	case want && running && runningFP != desiredFP:
		return actionRestart
	case !want && running:
		return actionStop
	default:
		return actionNone
	}
}

// pipelineFingerprint is a deterministic digest of the channel-governed config
// that shapes a pipeline (capture scope, batch strategy, confirmations, cadence,
// chain id, contract address). A change means the config was updated and the
// pipeline is hot-reloaded. Peer-local settings (workers, backoff, outbox path)
// are not here — they cannot change without a peer restart.
func pipelineFingerprint(mst *channelconfig.MSTAnchorConfig) string {
	raw, _ := json.Marshal(mst)
	return string(raw)
}

// channelMSTConfig reads the channel's MST anchoring configuration from its
// parsed application config.
func (s *Service) channelMSTConfig(channelID string) (*channelconfig.MSTAnchorConfig, bool) {
	app, ok := s.peer.GetApplicationConfig(channelID)
	if !ok {
		return nil, false
	}
	return app.MSTAnchorConfig()
}

// warnOnce logs a per-channel warning at most once for the service's lifetime.
func (s *Service) warnOnce(channelID, msg string) {
	s.mu.Lock()
	_, seen := s.warned[channelID]
	if !seen {
		s.warned[channelID] = struct{}{}
	}
	s.mu.Unlock()
	if !seen {
		logger.Warnw(msg, "channel", channelID)
	}
}

// warnIfWritebackOrgLacksNodeOUs logs a loud (once-per-channel) warning when the
// write-back cannot be authorized because an org's MSP lacks NodeOUs peer
// classification. When the peer's write-back MSPID is known it targets that org
// (a definite problem for this peer); otherwise it warns if any application org
// lacks it. It does not stop anchoring — the anchor still lands on MST; only the
// Fabric write-back record would be rejected by the mstscc peer-role gate.
func (s *Service) warnIfWritebackOrgLacksNodeOUs(channelID string) {
	res := s.peer.GetChannelConfig(channelID)
	if res == nil {
		return
	}
	config := res.ConfigtxValidator().ConfigProto()
	if config == nil {
		return
	}
	missing, total := channelconfig.ApplicationOrgsMissingPeerNodeOUs(config)
	if len(missing) == 0 || total == 0 {
		return
	}
	s.mu.Lock()
	_, seen := s.warnedNodeOUs[channelID]
	if !seen {
		s.warnedNodeOUs[channelID] = struct{}{}
	}
	s.mu.Unlock()
	if seen {
		return
	}

	wbMSPID := s.cfg.WriteBack.MSPID
	if wbMSPID != "" && contains(missing, wbMSPID) {
		logger.Warnw("MST anchoring is enabled but this peer's write-back org lacks NodeOUs peer classification; write-back records WILL be rejected (enable NodeOUs on the MSP, or point mst.writeback.* at a peer node identity)",
			"channel", channelID, "writebackMSPID", wbMSPID)
		return
	}
	logger.Warnw("MST anchoring is enabled but some application org(s) lack NodeOUs peer classification; write-back signed by them will be rejected",
		"channel", channelID, "orgsWithoutNodeOUs", strings.Join(missing, ","))
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Service) startPipeline(ctx context.Context, channelID string, mstCfg *channelconfig.MSTAnchorConfig) error {
	l := s.peer.GetLedger(channelID)
	if l == nil {
		return fmt.Errorf("no ledger for channel %s", channelID)
	}

	// Bind this channel's own MST contract onto the shared EVM client (one
	// relayer account, one serialized nonce sequence across all channels).
	binding, err := s.client.Bind(mstCfg.ContractAddress)
	if err != nil {
		return fmt.Errorf("channel %s: %w", channelID, err)
	}

	// The outbox backend follows the peer's state database: CouchDB peers
	// keep the outbox on the same CouchDB server (own databases), LevelDB
	// peers keep it embedded on local disk.
	var store outbox.Store
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

	// Capture scope, batch strategy, and confirmations are channel-governed:
	// read them from the channel's config so all peers anchor identically.
	captureCfg := s.cfg.CaptureConfigFor(mstCfg)
	snd, err := sender.New(store, binding, s.writeback, s.cfg.SenderConfigFor(mstCfg), nil)
	if err != nil {
		store.Close()
		return err
	}
	source := newLedgerSource(l, logger.With("channel", channelID))
	logger.Infow("binding channel to its MST contract", "channel", channelID, "contract", binding.Contract())

	// Per-pipeline context so this channel can be stopped/hot-reloaded on a config
	// change without disturbing the others. Goroutines are tracked by both s.wg
	// (so Stop drains everything) and the pipeline's own done wg (so stopPipeline
	// closes the store only after they have stopped).
	pctx, pcancel := context.WithCancel(ctx)
	done := &sync.WaitGroup{}

	s.mu.Lock()
	s.pipelines[channelID] = &pipeline{
		channelID: channelID,
		store:     store,
		cancel:    pcancel,
		done:      done,
		fp:        pipelineFingerprint(mstCfg),
	}
	s.mu.Unlock()

	// Capture loop with restart backoff: an iterator failure must not end
	// anchoring for the channel.
	s.wg.Add(2)
	done.Add(2)
	go func() {
		defer s.wg.Done()
		defer done.Done()
		backoff := time.Second
		for {
			svc := capture.New(source, store, captureCfg, nil)
			err := svc.Run(pctx)
			if pctx.Err() != nil {
				return
			}
			logger.Errorw("capture stream ended; restarting", "channel", channelID, "err", err, "backoff", backoff)
			select {
			case <-pctx.Done():
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
		defer done.Done()
		if err := snd.Run(pctx); err != nil && pctx.Err() == nil {
			logger.Errorw("sender stopped", "channel", channelID, "err", err)
		}
	}()

	logger.Infow("anchoring pipeline started", "channel", channelID)
	return nil
}

// stopPipeline stops one channel's pipeline: it cancels the capture+sender
// goroutines, waits for them to exit, and closes the outbox store. Safe to call
// for a channel with no running pipeline (no-op).
func (s *Service) stopPipeline(channelID string) {
	s.mu.Lock()
	p, ok := s.pipelines[channelID]
	if ok {
		delete(s.pipelines, channelID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	p.cancel()
	p.done.Wait()
	_ = p.store.Close()
	logger.Infow("anchoring pipeline stopped", "channel", channelID)
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
			sender.MetricsHandler(p.store, nil).ServeHTTP(w, r)
		}
		// The EVM client (and thus the gas balance) is shared across all
		// channel pipelines: emit the gauge exactly once per scrape.
		fmt.Fprintf(w, "# relayer account\n")
		sender.BalanceMetricsHandler(s.client).ServeHTTP(w, r)
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
