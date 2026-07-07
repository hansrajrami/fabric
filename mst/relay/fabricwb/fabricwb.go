// Package fabricwb records anchor status back on Fabric (spec section 14):
// after an anchor is confirmed on MST, the relayer invokes the anchor-status
// chaincode so Fabric can answer "is this tx anchored?" from its own ledger.
package fabricwb

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
)

// Submitter is the slice of fabric-gateway's Contract API the writer uses;
// *client.Contract satisfies it via the adapter below, tests use a fake.
type Submitter interface {
	Submit(transactionName string, args ...string) ([]byte, error)
}

// Writer submits RecordAnchor transactions. It is idempotent end to end:
// RecordAnchor itself is a no-op for an already-recorded tx id, so retries
// can never duplicate anything (and never touch MST at all).
type Writer struct {
	contract Submitter
	log      *slog.Logger
}

// NewWriter builds a write-back writer over any Submitter.
func NewWriter(contract Submitter, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.Default()
	}
	return &Writer{contract: contract, log: log}
}

// StatusConfirmed is the status string recorded on Fabric.
const StatusConfirmed = "CONFIRMED"

// RecordAnchor invokes the anchor-status chaincode with the fabric tx id,
// the anchor reference on MST (the EVM tx hash), and the status.
func (w *Writer) RecordAnchor(_ context.Context, fabricTxID, evmTxHash [32]byte) error {
	txID := hex.EncodeToString(fabricTxID[:])
	anchorRef := "0x" + hex.EncodeToString(evmTxHash[:])
	if _, err := w.contract.Submit("RecordAnchor", txID, anchorRef, StatusConfirmed); err != nil {
		return fmt.Errorf("fabricwb: RecordAnchor(%s): %w", txID, err)
	}
	w.log.Info("anchor status written back", "txID", txID, "anchorRef", anchorRef)
	return nil
}
