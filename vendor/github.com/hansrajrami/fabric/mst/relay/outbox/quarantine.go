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

type batchDTO struct {
	TxIDs []string `json:"tx_ids"`
	At    int64    `json:"at"`
}

func marshalBatch(txIDs [][32]byte) ([]byte, error) {
	dto := batchDTO{TxIDs: make([]string, len(txIDs)), At: nowUnix()}
	for i, id := range txIDs {
		dto.TxIDs[i] = hex.EncodeToString(id[:])
	}
	out, err := json.Marshal(&dto)
	if err != nil {
		return nil, fmt.Errorf("outbox: marshal batch: %w", err)
	}
	return out, nil
}

func unmarshalBatch(raw []byte) ([][32]byte, error) {
	var dto batchDTO
	if err := json.Unmarshal(raw, &dto); err != nil {
		return nil, fmt.Errorf("outbox: corrupt batch record: %w", err)
	}
	out := make([][32]byte, len(dto.TxIDs))
	for i, s := range dto.TxIDs {
		if err := decode32(s, &out[i]); err != nil {
			return nil, fmt.Errorf("outbox: corrupt batch member: %w", err)
		}
	}
	return out, nil
}
