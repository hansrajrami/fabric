package sender

import (
	"context"

	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

// WriteBack records on Fabric that an entry was anchored (spec section 12
// step 4). Implementations must be idempotent by fabric_tx_id: retries may
// re-record, and must never re-anchor anything on MST.
type WriteBack interface {
	Record(ctx context.Context, e *outbox.Entry) error
}

// NoopWriteBack skips write-back (used until the anchor-status chaincode is
// deployed, and in tests).
type NoopWriteBack struct{}

// Record does nothing, successfully.
func (NoopWriteBack) Record(context.Context, *outbox.Entry) error { return nil }
