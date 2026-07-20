/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	pcommon "github.com/hyperledger/fabric-protos-go/common"
	ab "github.com/hyperledger/fabric-protos-go/orderer"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/integration"
	"github.com/hyperledger/fabric/integration/nwo"
	"github.com/hyperledger/fabric/protoutil"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMST(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MST integration Suite")
}

var (
	buildServer *nwo.BuildServer
	components  *nwo.Components
)

var _ = SynchronizedBeforeSuite(func() []byte {
	buildServer = nwo.NewBuildServer()
	buildServer.Serve()

	components = buildServer.Components()
	payload, err := json.Marshal(components)
	Expect(err).NotTo(HaveOccurred())
	return payload
}, func(payload []byte) {
	err := json.Unmarshal(payload, &components)
	Expect(err).NotTo(HaveOccurred())
})

var _ = SynchronizedAfterSuite(func() {}, func() {
	buildServer.Shutdown()
})

func StartPort() int {
	return integration.MSTBasePort.StartPortForNode()
}

// peerNodeSigner builds a SigningIdentity backed by the peer's OWN node identity
// (its signcert + keystore key). With NodeOUs enabled this identity is
// classified as MSPRole_PEER, which is what the mstscc RecordAnchor gate
// requires — exactly the identity the embedded LoopbackWriteBack signs with.
func peerNodeSigner(n *nwo.Network, p *nwo.Peer) *nwo.SigningIdentity {
	keystore := filepath.Join(n.PeerLocalMSPDir(p), "keystore")
	keys, err := os.ReadDir(keystore)
	Expect(err).NotTo(HaveOccurred())
	Expect(keys).To(HaveLen(1))
	return &nwo.SigningIdentity{
		CertPath: n.PeerCert(p),
		KeyPath:  filepath.Join(keystore, keys[0].Name()),
		MSPID:    n.Organization(p.Organization).MSPID,
	}
}

// recordAnchorProposal builds and signs a mstscc RecordAnchor proposal with the
// given signer — the same argument shape the embedded write-back uses:
// RecordAnchor(fabricTxID hex, anchorRef "0x"+hex, "CONFIRMED").
func recordAnchorProposal(channel string, signer *nwo.SigningIdentity, fabricTxIDHex, anchorRef string) (*pb.SignedProposal, *pb.Proposal, string) {
	creator, err := signer.Serialize()
	Expect(err).NotTo(HaveOccurred())

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			Type:        pb.ChaincodeSpec_GOLANG,
			ChaincodeId: &pb.ChaincodeID{Name: "mstscc"},
			Input: &pb.ChaincodeInput{Args: [][]byte{
				[]byte("RecordAnchor"),
				[]byte(fabricTxIDHex),
				[]byte(anchorRef),
				[]byte("CONFIRMED"),
			}},
		},
	}
	prop, txid, err := protoutil.CreateChaincodeProposal(pcommon.HeaderType_ENDORSER_TRANSACTION, channel, cis, creator)
	Expect(err).NotTo(HaveOccurred())
	signedProp, err := protoutil.GetSignedProposal(prop, signer)
	Expect(err).NotTo(HaveOccurred())
	return signedProp, prop, txid
}

// queryAnchorStatus invokes mstscc QueryAnchorStatus as a read (no ordering) via
// the endorser and returns the decoded status, or nil if not found.
func queryAnchorStatus(endorser pb.EndorserClient, channel string, signer *nwo.SigningIdentity, fabricTxIDHex string) *anchorStatus {
	creator, err := signer.Serialize()
	Expect(err).NotTo(HaveOccurred())
	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			Type:        pb.ChaincodeSpec_GOLANG,
			ChaincodeId: &pb.ChaincodeID{Name: "mstscc"},
			Input:       &pb.ChaincodeInput{Args: [][]byte{[]byte("QueryAnchorStatus"), []byte(fabricTxIDHex)}},
		},
	}
	prop, _, err := protoutil.CreateChaincodeProposal(pcommon.HeaderType_ENDORSER_TRANSACTION, channel, cis, creator)
	Expect(err).NotTo(HaveOccurred())
	signedProp, err := protoutil.GetSignedProposal(prop, signer)
	Expect(err).NotTo(HaveOccurred())

	presp, err := endorser.ProcessProposal(context.Background(), signedProp)
	Expect(err).NotTo(HaveOccurred())
	if presp.GetResponse().GetStatus() < 200 || presp.GetResponse().GetStatus() >= 400 {
		return nil
	}
	payload := presp.GetResponse().GetPayload()
	if len(payload) == 0 {
		return nil
	}
	var s anchorStatus
	Expect(json.Unmarshal(payload, &s)).To(Succeed())
	return &s
}

type anchorStatus struct {
	FabricTxID string `json:"fabric_tx_id"`
	AnchorRef  string `json:"anchor_ref"`
	Status     string `json:"status"`
	RecordedAt int64  `json:"recorded_at"`
}

// commitTx broadcasts tx and waits (via DeliverFiltered) until txid appears in a
// committed block, returning an error if it committed with a non-VALID
// validation code — this is what catches INVALID_CHAINCODE regressions.
func commitTx(channel string, tx *pcommon.Envelope, txid string, dc pb.DeliverClient, oc ab.AtomicBroadcast_BroadcastClient, signer *nwo.SigningIdentity) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	df, err := dc.DeliverFiltered(ctx)
	Expect(err).NotTo(HaveOccurred())
	defer df.CloseSend()

	seek, err := protoutil.CreateSignedEnvelope(
		pcommon.HeaderType_DELIVER_SEEK_INFO,
		channel,
		signer,
		&ab.SeekInfo{
			Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
			Start:    &ab.SeekPosition{Type: &ab.SeekPosition_Newest{Newest: &ab.SeekNewest{}}},
			Stop:     &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{Number: math.MaxUint64}}},
		},
		0, 0,
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(df.Send(seek)).To(Succeed())
	Expect(oc.Send(tx)).To(Succeed())

	for {
		resp, err := df.Recv()
		if err != nil {
			return err
		}
		fb, ok := resp.Type.(*pb.DeliverResponse_FilteredBlock)
		if !ok {
			return fmt.Errorf("unexpected response %T", resp.Type)
		}
		for _, ftx := range fb.FilteredBlock.FilteredTransactions {
			if ftx.Txid != txid {
				continue
			}
			if ftx.TxValidationCode != pb.TxValidationCode_VALID {
				return fmt.Errorf("transaction invalidated with status (%s)", ftx.TxValidationCode)
			}
			return nil
		}
	}
}
