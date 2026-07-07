// mst-relayd is the MST anchoring relayer daemon: capture (Fabric block
// events -> outbox) and sender (outbox -> MST anchor contract -> Fabric
// write-back) in one process, joined only by the durable outbox.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hansrajrami/fabric/mst/relay/capture"
	"github.com/hansrajrami/fabric/mst/relay/config"
	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/fabricwb"
	"github.com/hansrajrami/fabric/mst/relay/gwsource"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mst-relayd failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "mst-relayd.json", "path to the relayer config file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	store, err := cfg.OpenOutbox()
	if err != nil {
		return err
	}
	defer store.Close()

	evmCfg, err := cfg.EVMConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := evm.Dial(ctx, evmCfg)
	if err != nil {
		return err
	}
	defer client.Close()
	log.Info("EVM client ready", "sender", client.Sender(), "contract", cfg.EVM.ContractAddress)

	source, err := gwsource.New(cfg.GatewayConfig(), log)
	if err != nil {
		return err
	}
	defer source.Close()
	log.Info("Fabric gateway connected", "endpoint", cfg.Fabric.Endpoint, "channel", cfg.Fabric.Channel)

	// Write-back target: wired to the anchor-status chaincode when
	// configured; no-op otherwise.
	var wb sender.WriteBack = sender.NoopWriteBack{}
	if cfg.Fabric.AnchorStatusChaincode != "" {
		wb = fabricwb.New(source.Network(), cfg.Fabric.AnchorStatusChaincode, log)
	}

	snd, err := sender.New(store, client, wb, cfg.SenderConfig(), log)
	if err != nil {
		return err
	}

	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", sender.MetricsHandler(store))
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			if _, err := store.Stats(); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintln(w, "ok")
		})
		srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("metrics server failed", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		log.Info("metrics listening", "addr", cfg.MetricsAddr)
	}

	// Capture loop with reconnect backoff: a dropped peer stream must never
	// take the relayer down. The outbox checkpoint makes reconnects safe.
	captureErr := make(chan error, 1)
	go func() {
		backoff := time.Second
		for {
			svc := capture.New(source, store, cfg.CaptureConfig(), log)
			err := svc.Run(ctx)
			if ctx.Err() != nil {
				captureErr <- nil
				return
			}
			log.Error("capture stream ended; reconnecting", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				captureErr <- nil
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()

	senderErr := make(chan error, 1)
	go func() { senderErr <- snd.Run(ctx) }()

	log.Info("mst-relayd running")
	select {
	case err := <-captureErr:
		return err
	case err := <-senderErr:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		<-senderErr
		return nil
	}
}
