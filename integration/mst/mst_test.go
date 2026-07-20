/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"syscall"

	"github.com/hyperledger/fabric-config/configtx"
	"github.com/hyperledger/fabric-protos-go/common"
	ab "github.com/hyperledger/fabric-protos-go/orderer"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/integration/nwo"
	"github.com/hyperledger/fabric/integration/ordererclient"
	"github.com/hyperledger/fabric/protoutil"
	dcli "github.com/moby/moby/client"
	"github.com/tedsuo/ifrit"
	grpc "google.golang.org/grpc"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These tests exercise the MST write-back on a real network: a peer-role
// identity endorses a mstscc RecordAnchor against the peer's own endorser,
// the signed transaction is ordered and committed, and validation accepts it.
// This is the exact chain the embedded LoopbackWriteBack drives, and it guards
// the three gates that failed on live networks:
//   - endorsement of the built-in mstscc (validation must not reject it as
//     INVALID_CHAINCODE — see ValidatorCommitter.EmbeddedSystemChaincodes),
//   - the channel Writers policy (must admit the peer role),
//   - the mstscc peer-role gate (must reject non-peer identities).
var _ = Describe("MST write-back", func() {
	var (
		client   dcli.APIClient
		testDir  string
		network  *nwo.Network
		process  ifrit.Process
		orderer  *nwo.Orderer
		org1peer *nwo.Peer

		pcc, occ       *grpc.ClientConn
		endorserClient pb.EndorserClient
		deliverClient  pb.DeliverClient
	)

	BeforeEach(func() {
		var err error
		testDir, err = os.MkdirTemp("", "mst")
		Expect(err).NotTo(HaveOccurred())

		client, err = dcli.New(dcli.FromEnv)
		Expect(err).NotTo(HaveOccurred())

		network = nwo.New(nwo.BasicSolo(), testDir, client, StartPort(), components)
		network.GenerateConfigTree()
		network.Bootstrap()

		process = ifrit.Invoke(network.NetworkGroupRunner())
		Eventually(process.Ready(), network.EventuallyTimeout).Should(BeClosed())

		orderer = network.Orderer("orderer")
		org1peer = network.Peer("Org1", "peer0")
		network.CreateAndJoinChannels(orderer)

		pcc = network.PeerClientConn(org1peer)
		endorserClient = pb.NewEndorserClient(pcc)
		deliverClient = pb.NewDeliverClient(pcc)
	})

	AfterEach(func() {
		if pcc != nil {
			pcc.Close()
		}
		if occ != nil {
			occ.Close()
		}
		if process != nil {
			process.Signal(syscall.SIGTERM)
			Eventually(process.Wait(), network.EventuallyTimeout).Should(Receive())
		}
		if network != nil {
			network.Cleanup()
		}
		os.RemoveAll(testDir)
	})

	// broadcastStream opens a fresh Broadcast stream to the orderer.
	broadcastStream := func() ab.AtomicBroadcast_BroadcastClient {
		occ = network.OrdererClientConn(orderer)
		bc, err := ab.NewAtomicBroadcastClient(occ).Broadcast(context.Background())
		Expect(err).NotTo(HaveOccurred())
		return bc
	}

	It("commits a peer-signed mstscc RecordAnchor once Writers admits the peer role", func() {
		By("widening each application org's Writers policy to include the peer role")
		widenWritersToPeer(network, orderer, org1peer, "testchannel")

		signer := peerNodeSigner(network, org1peer)
		fabricTxID := hex.EncodeToString([]byte("mst-e2e-anchor-0000000000000000")[:32])
		anchorRef := "0x" + hex.EncodeToString(make([]byte, 32))

		By("endorsing RecordAnchor against the peer's local endorser")
		signedProp, prop, txid := recordAnchorProposal("testchannel", signer, fabricTxID, anchorRef)
		presp, err := endorserClient.ProcessProposal(context.Background(), signedProp)
		Expect(err).NotTo(HaveOccurred())
		Expect(presp.GetResponse().GetStatus()).To(BeEquivalentTo(200), "peer-role endorsement of mstscc must succeed: %s", presp.GetResponse().GetMessage())

		By("assembling and broadcasting the transaction, and confirming it commits VALID")
		env, err := protoutil.CreateSignedTx(prop, signer, presp)
		Expect(err).NotTo(HaveOccurred())
		Expect(commitTx("testchannel", env, txid, deliverClient, broadcastStream(), signer)).To(Succeed())

		By("confirming the anchor status is recorded on the ledger")
		status := queryAnchorStatus(endorserClient, "testchannel", signer, fabricTxID)
		Expect(status).NotTo(BeNil())
		Expect(status.Status).To(Equal("CONFIRMED"))
		Expect(status.FabricTxID).To(Equal(fabricTxID))
		Expect(status.AnchorRef).To(Equal(anchorRef))
	})

	It("rejects a RecordAnchor endorsed by a client identity (peer-role gate)", func() {
		widenWritersToPeer(network, orderer, org1peer, "testchannel")

		By("endorsing RecordAnchor with a client/user identity")
		userSigner := network.PeerUserSigner(org1peer, "User1")
		fabricTxID := hex.EncodeToString([]byte("mst-e2e-anchor-client-000000000")[:32])
		signedProp, _, _ := recordAnchorProposal("testchannel", userSigner, fabricTxID, "0x"+hex.EncodeToString(make([]byte, 32)))

		presp, err := endorserClient.ProcessProposal(context.Background(), signedProp)
		// The peer-role gate rejects the write-back: the chaincode returns a
		// non-2xx response (a client identity is not a peer node).
		Expect(err).NotTo(HaveOccurred())
		Expect(presp.GetResponse().GetStatus()).NotTo(BeEquivalentTo(200))
		Expect(presp.GetResponse().GetMessage()).To(ContainSubstring("RecordAnchor"))
	})

	It("is rejected by the orderer when Writers excludes the peer role", func() {
		// No Writers widening: the default NodeOUs Writers is OR(admin,client),
		// which excludes peer, so the orderer must refuse the broadcast even
		// though endorsement succeeds.
		signer := peerNodeSigner(network, org1peer)
		fabricTxID := hex.EncodeToString([]byte("mst-e2e-anchor-forbidden-0000000")[:32])
		signedProp, prop, _ := recordAnchorProposal("testchannel", signer, fabricTxID, "0x"+hex.EncodeToString(make([]byte, 32)))

		presp, err := endorserClient.ProcessProposal(context.Background(), signedProp)
		Expect(err).NotTo(HaveOccurred())
		Expect(presp.GetResponse().GetStatus()).To(BeEquivalentTo(200), "endorsement itself must succeed")

		env, err := protoutil.CreateSignedTx(prop, signer, presp)
		Expect(err).NotTo(HaveOccurred())
		resp, err := ordererclient.Broadcast(network, orderer, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Status).To(Equal(common.Status_FORBIDDEN))
	})
})

// widenWritersToPeer changes each application org's Writers policy to admit the
// peer role, via a channel config update signed by the org admins. Without this
// the orderer rejects the peer-role write-back broadcast (FORBIDDEN).
func widenWritersToPeer(n *nwo.Network, o *nwo.Orderer, submitPeer *nwo.Peer, channel string) {
	current := nwo.GetConfig(n, submitPeer, o, channel)
	c := configtx.New(current)

	appOrgs := []string{"Org1", "Org2"}
	for _, org := range appOrgs {
		mspID := n.Organization(org).MSPID
		err := c.Application().Organization(org).SetPolicy(configtx.WritersPolicyKey, configtx.Policy{
			Type: "Signature",
			Rule: "OR('" + mspID + ".admin','" + mspID + ".client','" + mspID + ".peer')",
		})
		Expect(err).NotTo(HaveOccurred())
	}

	update, err := c.ComputeMarshaledUpdate(channel)
	Expect(err).NotTo(HaveOccurred())

	// Each org's policy value has that org's Admins as mod_policy, so both org
	// admins must sign the update.
	var signatures []*common.ConfigSignature
	for _, org := range appOrgs {
		p := n.Peer(org, "peer0")
		id := configtx.SigningIdentity{
			Certificate: parseCertificate(n.PeerUserCert(p, "Admin")),
			PrivateKey:  parsePrivateKey(n.PeerUserKey(p, "Admin")),
			MSPID:       n.Organization(org).MSPID,
		}
		sig, err := id.CreateConfigSignature(update)
		Expect(err).NotTo(HaveOccurred())
		signatures = append(signatures, sig)
	}

	env, err := configtx.NewEnvelope(update, signatures...)
	Expect(err).NotTo(HaveOccurred())
	submitter := configtx.SigningIdentity{
		Certificate: parseCertificate(n.PeerUserCert(submitPeer, "Admin")),
		PrivateKey:  parsePrivateKey(n.PeerUserKey(submitPeer, "Admin")),
		MSPID:       n.Organization(submitPeer.Organization).MSPID,
	}
	Expect(submitter.SignEnvelope(env)).To(Succeed())

	before := nwo.CurrentConfigBlockNumber(n, submitPeer, o, channel)
	resp, err := ordererclient.Broadcast(n, o, env)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.Status).To(Equal(common.Status_SUCCESS))
	Eventually(func() uint64 { return nwo.CurrentConfigBlockNumber(n, submitPeer, o, channel) }, n.EventuallyTimeout).
		Should(BeNumerically(">", before))
}

func parsePrivateKey(path string) crypto.PrivateKey {
	keyBytes, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	pemBlock, _ := pem.Decode(keyBytes)
	key, err := x509.ParsePKCS8PrivateKey(pemBlock.Bytes)
	Expect(err).NotTo(HaveOccurred())
	return key
}

func parseCertificate(path string) *x509.Certificate {
	certBytes, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	pemBlock, _ := pem.Decode(certBytes)
	cert, err := x509.ParseCertificate(pemBlock.Bytes)
	Expect(err).NotTo(HaveOccurred())
	return cert
}
