// Package gwsource streams committed blocks from a Fabric peer via the
// Fabric Gateway block-event API (fabric-protos-go-apiv2) and parses them
// into the proto-free txmodel for the capture core.
//
// This package is the standalone relayer's block source. It must NEVER be
// imported by code that links into the peer binary: the peer carries the old
// fabric-protos-go module, and both proto modules register the same proto
// file paths (linking them together panics at init). The embedded peer
// source lives in the fabric tree instead (internal/pkg/mstanchor).
package gwsource

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-gateway/pkg/identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/hansrajrami/fabric/mst/relay/blockparse"
	"github.com/hansrajrami/fabric/mst/relay/capture"
	"github.com/hansrajrami/fabric/mst/relay/txmodel"
)

// Config locates the Fabric peer and the relayer's Fabric identity.
type Config struct {
	// Endpoint is the peer's gateway address, e.g. "peer0.org1.example.com:7051".
	Endpoint string
	// TLSCACertPath is the peer's TLS CA certificate; empty means an
	// insecure (plaintext) connection — local development only.
	TLSCACertPath string
	// ServerNameOverride overrides the TLS server name (dev setups).
	ServerNameOverride string
	// MSPID, CertPath, KeyPath are the relayer's Fabric identity (X.509
	// cert and matching private key, PEM).
	MSPID    string
	CertPath string
	KeyPath  string
	// Channel is the channel whose blocks are captured.
	Channel string
}

// GatewaySource implements capture.Source over the gateway block-event API.
// The start block is driven by the outbox checkpoint, not the gateway's own
// checkpointers, so checkpoint advancement stays atomic with outbox writes.
type GatewaySource struct {
	conn    *grpc.ClientConn
	gateway *client.Gateway
	network *client.Network
	log     *slog.Logger
}

var _ capture.Source = (*GatewaySource)(nil)

// New dials the peer and establishes a gateway session.
func New(cfg Config, log *slog.Logger) (*GatewaySource, error) {
	if log == nil {
		log = slog.Default()
	}
	transport := insecure.NewCredentials()
	if cfg.TLSCACertPath != "" {
		creds, err := credentials.NewClientTLSFromFile(cfg.TLSCACertPath, cfg.ServerNameOverride)
		if err != nil {
			return nil, fmt.Errorf("gwsource: load peer TLS CA: %w", err)
		}
		transport = creds
	}
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("gwsource: dial %s: %w", cfg.Endpoint, err)
	}

	id, err := loadX509Identity(cfg.MSPID, cfg.CertPath)
	if err != nil {
		conn.Close()
		return nil, err
	}
	sign, err := loadSigner(cfg.KeyPath)
	if err != nil {
		conn.Close()
		return nil, err
	}

	gw, err := client.Connect(id, client.WithSign(sign), client.WithClientConnection(conn))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("gwsource: gateway connect: %w", err)
	}
	return &GatewaySource{conn: conn, gateway: gw, network: gw.GetNetwork(cfg.Channel), log: log}, nil
}

// Blocks starts the block-event stream at startBlock and parses each block.
func (g *GatewaySource) Blocks(ctx context.Context, startBlock uint64) (<-chan *txmodel.Block, error) {
	raw, err := g.network.BlockEvents(ctx, client.WithStartBlock(startBlock))
	if err != nil {
		return nil, err
	}
	out := make(chan *txmodel.Block)
	go func() {
		defer close(out)
		for block := range raw {
			parsed, err := blockparse.Parse(block)
			if err != nil {
				// A block that cannot even be shaped is a protocol-level
				// failure; end the stream and let the reconnect loop retry.
				g.log.Error("unparseable block from gateway", "err", err)
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

// Network exposes the gateway network handle (reused by the write-back
// client so the relayer holds a single peer connection).
func (g *GatewaySource) Network() *client.Network { return g.network }

// Close tears down the gateway session and connection.
func (g *GatewaySource) Close() error {
	g.gateway.Close()
	return g.conn.Close()
}

func loadX509Identity(mspID, certPath string) (identity.Identity, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("gwsource: read identity cert: %w", err)
	}
	cert, err := identity.CertificateFromPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("gwsource: parse identity cert: %w", err)
	}
	id, err := identity.NewX509Identity(mspID, cert)
	if err != nil {
		return nil, fmt.Errorf("gwsource: build identity: %w", err)
	}
	return id, nil
}

func loadSigner(keyPath string) (identity.Sign, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("gwsource: read identity key: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("gwsource: identity key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gwsource: parse identity key: %w", err)
	}
	sign, err := identity.NewPrivateKeySign(key)
	if err != nil {
		return nil, fmt.Errorf("gwsource: build signer: %w", err)
	}
	return sign, nil
}
