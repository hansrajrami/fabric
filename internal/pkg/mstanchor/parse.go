/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"fmt"

	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/internal/pkg/txflags"
	"github.com/hyperledger/fabric/protoutil"

	"github.com/hansrajrami/fabric/mst/relay/txmodel"
)

// ParseBlock converts a committed block (the peer's own fabric-protos-go
// representation) into the proto-free txmodel consumed by the capture core.
// It is the in-peer counterpart of the standalone relayer's blockparse
// package (which does the same over fabric-protos-go-apiv2 — the two proto
// modules cannot be linked into one binary, hence two thin parsers over one
// shared model).
func ParseBlock(block *common.Block) (*txmodel.Block, error) {
	if block == nil {
		return nil, fmt.Errorf("mstanchor: nil block")
	}
	out := &txmodel.Block{Number: block.GetHeader().GetNumber()}

	flags := transactionsFilter(block)
	for i, envelopeBytes := range block.GetData().GetData() {
		tx, isEndorser, err := parseEnvelope(envelopeBytes)
		if err != nil {
			out.Bad = append(out.Bad, txmodel.BadEnvelope{Index: i, Err: err})
			continue
		}
		if !isEndorser {
			continue
		}
		tx.Valid = flags != nil && flags.IsValid(i)
		out.Txs = append(out.Txs, *tx)
	}
	return out, nil
}

func transactionsFilter(block *common.Block) txflags.ValidationFlags {
	metadata := block.GetMetadata().GetMetadata()
	if int(common.BlockMetadataIndex_TRANSACTIONS_FILTER) >= len(metadata) {
		return nil
	}
	return txflags.ValidationFlags(metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER])
}

func parseEnvelope(envelopeBytes []byte) (*txmodel.Tx, bool, error) {
	envelope, err := protoutil.UnmarshalEnvelope(envelopeBytes)
	if err != nil {
		return nil, false, fmt.Errorf("unmarshal envelope: %w", err)
	}
	payload, err := protoutil.UnmarshalPayload(envelope.GetPayload())
	if err != nil {
		return nil, false, fmt.Errorf("unmarshal payload: %w", err)
	}
	channelHeader, err := protoutil.UnmarshalChannelHeader(payload.GetHeader().GetChannelHeader())
	if err != nil {
		return nil, false, fmt.Errorf("unmarshal channel header: %w", err)
	}
	if channelHeader.GetType() != int32(common.HeaderType_ENDORSER_TRANSACTION) {
		return nil, false, nil
	}

	tx := &txmodel.Tx{
		TxID:      channelHeader.GetTxId(),
		ChannelID: channelHeader.GetChannelId(),
	}
	if ts := channelHeader.GetTimestamp(); ts != nil && ts.GetSeconds() > 0 {
		tx.TimestampUnix = uint64(ts.GetSeconds())
	}

	events, err := readChaincodeEvents(payload.GetData())
	if err != nil {
		return nil, false, err
	}
	tx.Events = events
	return tx, true, nil
}

// readChaincodeEvents mirrors the walk in the peer's own gateway event code
// (internal/pkg/gateway/event/transaction.go): undecodable individual
// actions are skipped, a malformed transaction envelope is an error.
func readChaincodeEvents(payloadData []byte) ([]txmodel.Event, error) {
	transaction, err := protoutil.UnmarshalTransaction(payloadData)
	if err != nil {
		return nil, fmt.Errorf("unmarshal transaction: %w", err)
	}

	var events []txmodel.Event
	for _, action := range transaction.GetActions() {
		actionPayload, err := protoutil.UnmarshalChaincodeActionPayload(action.GetPayload())
		if err != nil {
			continue
		}
		responsePayload, err := protoutil.UnmarshalProposalResponsePayload(actionPayload.GetAction().GetProposalResponsePayload())
		if err != nil {
			continue
		}
		chaincodeAction, err := protoutil.UnmarshalChaincodeAction(responsePayload.GetExtension())
		if err != nil {
			continue
		}
		event, err := protoutil.UnmarshalChaincodeEvents(chaincodeAction.GetEvents())
		if err != nil {
			continue
		}
		if event.GetChaincodeId() == "" || event.GetEventName() == "" {
			continue
		}
		events = append(events, txmodel.Event{
			ChaincodeID: event.GetChaincodeId(),
			EventName:   event.GetEventName(),
			Payload:     event.GetPayload(),
		})
	}
	return events, nil
}
