package capture

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-gateway/pkg/identity"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// GatewayConfig locates the Fabric peer and the relayer's Fabric identity.
type GatewayConfig struct {
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

// GatewaySource streams committed blocks from a Fabric peer via the Fabric
// Gateway block-event API. Blocks are delivered post-commit; the start block
// is driven by the outbox checkpoint, not the gateway's own checkpointers,
// so checkpoint advancement stays atomic with outbox writes.
type GatewaySource struct {
	conn    *grpc.ClientConn
	gateway *client.Gateway
	network *client.Network
}

var _ Source = (*GatewaySource)(nil)

// NewGatewaySource dials the peer and establishes a gateway session.
func NewGatewaySource(cfg GatewayConfig) (*GatewaySource, error) {
	transport := insecure.NewCredentials()
	if cfg.TLSCACertPath != "" {
		creds, err := credentials.NewClientTLSFromFile(cfg.TLSCACertPath, cfg.ServerNameOverride)
		if err != nil {
			return nil, fmt.Errorf("capture: load peer TLS CA: %w", err)
		}
		transport = creds
	}
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("capture: dial %s: %w", cfg.Endpoint, err)
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
		return nil, fmt.Errorf("capture: gateway connect: %w", err)
	}
	return &GatewaySource{conn: conn, gateway: gw, network: gw.GetNetwork(cfg.Channel)}, nil
}

// Blocks starts the block-event stream at startBlock.
func (g *GatewaySource) Blocks(ctx context.Context, startBlock uint64) (<-chan *common.Block, error) {
	return g.network.BlockEvents(ctx, client.WithStartBlock(startBlock))
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
		return nil, fmt.Errorf("capture: read identity cert: %w", err)
	}
	cert, err := identity.CertificateFromPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("capture: parse identity cert: %w", err)
	}
	id, err := identity.NewX509Identity(mspID, cert)
	if err != nil {
		return nil, fmt.Errorf("capture: build identity: %w", err)
	}
	return id, nil
}

func loadSigner(keyPath string) (identity.Sign, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("capture: read identity key: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("capture: identity key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("capture: parse identity key: %w", err)
	}
	sign, err := identity.NewPrivateKeySign(key)
	if err != nil {
		return nil, fmt.Errorf("capture: build signer: %w", err)
	}
	return sign, nil
}

// StaticSource replays a fixed slice of blocks; used in tests and for
// offline reprocessing.
type StaticSource struct {
	BlocksList []*common.Block
}

var _ Source = (*StaticSource)(nil)

// Blocks emits every stored block with number >= startBlock, then closes.
func (s *StaticSource) Blocks(ctx context.Context, startBlock uint64) (<-chan *common.Block, error) {
	ch := make(chan *common.Block)
	go func() {
		defer close(ch)
		for _, b := range s.BlocksList {
			if b.GetHeader().GetNumber() < startBlock {
				continue
			}
			select {
			case ch <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
