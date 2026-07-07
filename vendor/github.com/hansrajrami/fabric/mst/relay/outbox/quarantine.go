package outbox

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func marshalQuarantine(blockNumber uint64, reason string, raw []byte) ([]byte, error) {
	rec := quarantineDTO{
		BlockNumber: blockNumber,
		Reason:      reason,
		RawHex:      hex.EncodeToString(raw),
		At:          nowUnix(),
	}
	out, err := json.Marshal(&rec)
	if err != nil {
		return nil, fmt.Errorf("outbox: marshal quarantine: %w", err)
	}
	return out, nil
}
