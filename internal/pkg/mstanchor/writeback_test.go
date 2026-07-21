/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mstanchor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	gp "github.com/hyperledger/fabric-protos-go/gateway"
	"github.com/hyperledger/fabric-protos-go/peer"
	"github.com/stretchr/testify/require"

	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

// fakeEndorser is a stand-in for the peer's local endorser. It records the
// signed proposal it was handed and returns a canned response.
type fakeEndorser struct {
	gotProposal *peer.SignedProposal
	response    *peer.ProposalResponse
	err         error
}

func (f *fakeEndorser) ProcessProposal(_ context.Context, signedProp *peer.SignedProposal) (*peer.ProposalResponse, error) {
	f.gotProposal = signedProp
	return f.response, f.err
}

// fakeGateway is a stand-in for the peer's gateway. It records the envelope it
// was asked to order and the commit-status query, and returns canned results.
type fakeGateway struct {
	submitted    *gp.SubmitRequest
	submitErr    error
	commitResult peer.TxValidationCode
	commitErr    error
}

func (f *fakeGateway) Submit(_ context.Context, request *gp.SubmitRequest) (*gp.SubmitResponse, error) {
	f.submitted = request
	return &gp.SubmitResponse{}, f.submitErr
}

func (f *fakeGateway) CommitStatus(_ context.Context, _ *gp.SignedCommitStatusRequest) (*gp.CommitStatusResponse, error) {
	return &gp.CommitStatusResponse{Result: f.commitResult}, f.commitErr
}

// writeBackFixture writes an ephemeral ECDSA cert/key pair to disk (as the
// relayer identity expects) and returns a LoopbackWriteBack wired to the given
// fakes.
func writeBackFixture(t *testing.T, endorser EndorserProcessor, gateway GatewayInvoker) *LoopbackWriteBack {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mst-relayer"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600))

	wb, err := NewLoopbackWriteBack(endorser, gateway, "mstscc", "Org1MSP", certPath, keyPath)
	require.NoError(t, err)
	return wb
}

// endorsedResponse builds a successful ProposalResponse with a non-nil
// endorsement, which CreateSignedTx requires to assemble the envelope.
func endorsedResponse(status int32) *peer.ProposalResponse {
	return &peer.ProposalResponse{
		Response:    &peer.Response{Status: status, Message: "msg", Payload: []byte("resp")},
		Payload:     []byte("proposal-response-payload"),
		Endorsement: &peer.Endorsement{Endorser: []byte("endorser"), Signature: []byte("sig")},
	}
}

func sampleEntry() *outbox.Entry {
	return &outbox.Entry{
		ChannelID:  "mychannel",
		FabricTxID: [32]byte{0x01, 0x02},
		EVMTxHash:  [32]byte{0xaa, 0xbb},
	}
}

// TestLoopbackWriteBackRecordSuccess exercises the happy path: the write-back
// endorses against the local endorser (NOT the gateway), assembles a signed
// envelope, orders it via the gateway, and confirms commit.
func TestLoopbackWriteBackRecordSuccess(t *testing.T) {
	endorser := &fakeEndorser{response: endorsedResponse(200)}
	gateway := &fakeGateway{commitResult: peer.TxValidationCode_VALID}
	wb := writeBackFixture(t, endorser, gateway)

	require.NoError(t, wb.Record(context.Background(), sampleEntry()))

	// The proposal was endorsed via the local endorser.
	require.NotNil(t, endorser.gotProposal, "endorser must receive the signed proposal")
	// The envelope handed to the gateway is signed (CreateSignedTx signed it),
	// so gateway.Submit — which rejects unsigned envelopes — is satisfied.
	require.NotNil(t, gateway.submitted)
	require.NotEmpty(t, gateway.submitted.GetPreparedTransaction().GetSignature())
	require.Equal(t, "mychannel", gateway.submitted.GetChannelId())
}

// TestLoopbackWriteBackRecordEndorseError surfaces a failed endorsement.
func TestLoopbackWriteBackRecordEndorseError(t *testing.T) {
	endorser := &fakeEndorser{err: context.DeadlineExceeded}
	gateway := &fakeGateway{commitResult: peer.TxValidationCode_VALID}
	wb := writeBackFixture(t, endorser, gateway)

	err := wb.Record(context.Background(), sampleEntry())
	require.Error(t, err)
	require.Contains(t, err.Error(), "endorse RecordAnchor")
	require.Nil(t, gateway.submitted, "a failed endorsement must not be ordered")
}

// TestLoopbackWriteBackRecordEndorsementRejected surfaces a non-2xx chaincode
// response (e.g. the peer-role gate rejecting the relayer identity).
func TestLoopbackWriteBackRecordEndorsementRejected(t *testing.T) {
	endorser := &fakeEndorser{response: endorsedResponse(500)}
	gateway := &fakeGateway{commitResult: peer.TxValidationCode_VALID}
	wb := writeBackFixture(t, endorser, gateway)

	err := wb.Record(context.Background(), sampleEntry())
	require.Error(t, err)
	require.Contains(t, err.Error(), "endorsement failed")
	require.Nil(t, gateway.submitted, "a rejected endorsement must not be ordered")
}

// TestLoopbackWriteBackRecordInvalidated surfaces a transaction that commits
// with a non-VALID validation code that is not the benign MVCC race.
func TestLoopbackWriteBackRecordInvalidated(t *testing.T) {
	endorser := &fakeEndorser{response: endorsedResponse(200)}
	gateway := &fakeGateway{commitResult: peer.TxValidationCode_INVALID_CHAINCODE}
	wb := writeBackFixture(t, endorser, gateway)

	err := wb.Record(context.Background(), sampleEntry())
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalidated")
}

// TestLoopbackWriteBackRecordMVCCConflictIsSuccess treats an MVCC_READ_CONFLICT
// as success: it can only mean a concurrent write-back already recorded this
// exact anchor (the tx's read set is only the deterministic anchor key), so the
// entry should finalize instead of being re-parked for retry.
func TestLoopbackWriteBackRecordMVCCConflictIsSuccess(t *testing.T) {
	endorser := &fakeEndorser{response: endorsedResponse(200)}
	gateway := &fakeGateway{commitResult: peer.TxValidationCode_MVCC_READ_CONFLICT}
	wb := writeBackFixture(t, endorser, gateway)

	require.NoError(t, wb.Record(context.Background(), sampleEntry()))
}
