package fabricwb

import (
	"context"
	"log/slog"

	"github.com/hyperledger/fabric-gateway/pkg/client"

	"github.com/hansrajrami/fabric/mst/relay/outbox"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

// gatewayContract adapts *client.Contract to Submitter.
type gatewayContract struct {
	contract *client.Contract
}

func (g gatewayContract) Submit(name string, args ...string) ([]byte, error) {
	return g.contract.Submit(name, client.WithArguments(args...))
}

// writeBack adapts Writer to the sender.WriteBack interface.
type writeBack struct {
	writer *Writer
}

// New builds a sender.WriteBack that records anchor status via the given
// chaincode on the (already connected) gateway network — the relayer reuses
// the capture service's single peer connection.
func New(network *client.Network, chaincodeName string, log *slog.Logger) sender.WriteBack {
	contract := network.GetContract(chaincodeName)
	return writeBack{writer: NewWriter(gatewayContract{contract}, log)}
}

func (w writeBack) Record(ctx context.Context, e *outbox.Entry) error {
	return w.writer.RecordAnchor(ctx, e.FabricTxID, e.EVMTxHash)
}
