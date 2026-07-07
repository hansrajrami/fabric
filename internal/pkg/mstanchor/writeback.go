/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-protos-go/common"
	gp "github.com/hyperledger/fabric-protos-go/gateway"
	mspproto "github.com/hyperledger/fabric-protos-go/msp"
	"github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/bccsp/utils"
	"github.com/hyperledger/fabric/protoutil"

	"github.com/hansrajrami/fabric/mst/relay/outbox"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

// statusConfirmed mirrors fabricwb.StatusConfirmed for the embedded path.
const statusConfirmed = "CONFIRMED"

// writeBackTimeout bounds one RecordAnchor round trip (endorse + order +
// commit). Failures re-park the entry at CONFIRMED; the sender retries and
// RecordAnchor is idempotent.
const writeBackTimeout = 2 * time.Minute

// GatewayInvoker is the in-process slice of the peer's gateway server the
// write-back needs; *gateway.Server satisfies it. Calling the server's Go
// methods directly avoids a loopback network hop entirely.
type GatewayInvoker interface {
	Endorse(ctx context.Context, request *gp.EndorseRequest) (*gp.EndorseResponse, error)
	Submit(ctx context.Context, request *gp.SubmitRequest) (*gp.SubmitResponse, error)
	CommitStatus(ctx context.Context, signedRequest *gp.SignedCommitStatusRequest) (*gp.CommitStatusResponse, error)
}

// LoopbackWriteBack records anchor status on Fabric by submitting
// RecordAnchor transactions through the peer's own embedded gateway, signed
// with the relayer's Fabric identity.
type LoopbackWriteBack struct {
	gateway   GatewayInvoker
	signer    *identitySigner
	chaincode string
}

var _ sender.WriteBack = (*LoopbackWriteBack)(nil)

// NewLoopbackWriteBack loads the relayer identity and wraps the gateway.
func NewLoopbackWriteBack(gateway GatewayInvoker, chaincode, mspID, certPath, keyPath string) (*LoopbackWriteBack, error) {
	signer, err := newIdentitySigner(mspID, certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &LoopbackWriteBack{gateway: gateway, signer: signer, chaincode: chaincode}, nil
}

// Record submits RecordAnchor(fabricTxID, evmTxHash, CONFIRMED) and waits
// for commit. Idempotent end to end: the chaincode is a quiet no-op for an
// already-recorded id.
func (w *LoopbackWriteBack) Record(ctx context.Context, e *outbox.Entry) error {
	ctx, cancel := context.WithTimeout(ctx, writeBackTimeout)
	defer cancel()

	args := [][]byte{
		[]byte("RecordAnchor"),
		[]byte(hex.EncodeToString(e.FabricTxID[:])),
		[]byte("0x" + hex.EncodeToString(e.EVMTxHash[:])),
		[]byte(statusConfirmed),
	}
	cis := &peer.ChaincodeInvocationSpec{
		ChaincodeSpec: &peer.ChaincodeSpec{
			Type:        peer.ChaincodeSpec_GOLANG,
			ChaincodeId: &peer.ChaincodeID{Name: w.chaincode},
			Input:       &peer.ChaincodeInput{Args: args},
		},
	}
	creator, err := w.signer.Serialize()
	if err != nil {
		return fmt.Errorf("mstanchor: serialize identity: %w", err)
	}
	proposal, txID, err := protoutil.CreateChaincodeProposal(
		common.HeaderType_ENDORSER_TRANSACTION, e.ChannelID, cis, creator,
	)
	if err != nil {
		return fmt.Errorf("mstanchor: build proposal: %w", err)
	}
	signedProposal, err := protoutil.GetSignedProposal(proposal, w.signer)
	if err != nil {
		return fmt.Errorf("mstanchor: sign proposal: %w", err)
	}

	endorsed, err := w.gateway.Endorse(ctx, &gp.EndorseRequest{
		TransactionId:       txID,
		ChannelId:           e.ChannelID,
		ProposedTransaction: signedProposal,
	})
	if err != nil {
		return fmt.Errorf("mstanchor: endorse RecordAnchor: %w", err)
	}
	envelope := endorsed.GetPreparedTransaction()
	envelope.Signature, err = w.signer.Sign(envelope.GetPayload())
	if err != nil {
		return fmt.Errorf("mstanchor: sign transaction: %w", err)
	}

	if _, err := w.gateway.Submit(ctx, &gp.SubmitRequest{
		TransactionId:       txID,
		ChannelId:           e.ChannelID,
		PreparedTransaction: envelope,
	}); err != nil {
		return fmt.Errorf("mstanchor: submit RecordAnchor: %w", err)
	}

	statusRequest := &gp.CommitStatusRequest{
		ChannelId:     e.ChannelID,
		TransactionId: txID,
		Identity:      creator,
	}
	requestBytes, err := proto.Marshal(statusRequest)
	if err != nil {
		return fmt.Errorf("mstanchor: marshal status request: %w", err)
	}
	signature, err := w.signer.Sign(requestBytes)
	if err != nil {
		return fmt.Errorf("mstanchor: sign status request: %w", err)
	}
	statusResponse, err := w.gateway.CommitStatus(ctx, &gp.SignedCommitStatusRequest{
		Request:   requestBytes,
		Signature: signature,
	})
	if err != nil {
		return fmt.Errorf("mstanchor: commit status: %w", err)
	}
	if statusResponse.GetResult() != peer.TxValidationCode_VALID {
		return fmt.Errorf("mstanchor: RecordAnchor invalidated: %s", statusResponse.GetResult())
	}
	return nil
}

// identitySigner is a minimal Fabric signing identity over a PEM cert/key
// pair: SHA-256 + low-S ECDSA (Fabric rejects high-S signatures), creator =
// the standard SerializedIdentity proto. It implements protoutil.Signer.
type identitySigner struct {
	creator []byte
	key     *ecdsa.PrivateKey
}

var _ protoutil.Signer = (*identitySigner)(nil)

func newIdentitySigner(mspID, certPath, keyPath string) (*identitySigner, error) {
	if mspID == "" || certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("mstanchor: write-back requires mst.writeback mspID, certPath, and keyPath")
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("mstanchor: read write-back cert: %w", err)
	}
	creator, err := proto.Marshal(&mspproto.SerializedIdentity{Mspid: mspID, IdBytes: certPEM})
	if err != nil {
		return nil, fmt.Errorf("mstanchor: serialize identity: %w", err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("mstanchor: read write-back key: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("mstanchor: write-back key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mstanchor: parse write-back key: %w", err)
	}
	ecKey, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("mstanchor: write-back key must be ECDSA, got %T", parsed)
	}
	return &identitySigner{creator: creator, key: ecKey}, nil
}

func (s *identitySigner) Serialize() ([]byte, error) { return s.creator, nil }

func (s *identitySigner) Sign(msg []byte) ([]byte, error) {
	digest := sha256.Sum256(msg)
	sig, err := s.key.Sign(rand.Reader, digest[:], nil)
	if err != nil {
		return nil, err
	}
	return utils.SignatureToLowS(&s.key.PublicKey, sig)
}
