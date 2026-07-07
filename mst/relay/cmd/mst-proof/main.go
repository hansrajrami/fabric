// mst-proof exports the Merkle inclusion proof for a transaction anchored
// under a batch root, from the relayer's outbox. Hand the resulting JSON to
// anyone holding the transaction's original data: together with mst-verify
// they can independently confirm the transaction against the on-chain root.
//
// Usage:
//
//	mst-proof --outbox /var/lib/mst-relay/outbox/mychannel --tx-id <64 hex>
//	mst-proof --couchdb-url http://127.0.0.1:5984 --couchdb-database mst_outbox_mychannel --tx-id <64 hex>
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/hansrajrami/fabric/mst/canonical"
	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

type proofFile struct {
	FabricTxID string   `json:"fabric_tx_id"`
	Leaf       string   `json:"leaf"` // the tx's commitment
	Root       string   `json:"root"`
	LeafCount  int      `json:"leaf_count"`
	Proof      []string `json:"proof"`
}

func main() {
	os.Exit(run())
}

func run() int {
	var (
		outboxPath = flag.String("outbox", "", "LevelDB outbox directory (per channel)")
		couchURL   = flag.String("couchdb-url", "", "CouchDB server URL (instead of --outbox)")
		couchUser  = flag.String("couchdb-username", "", "CouchDB username")
		couchPass  = flag.String("couchdb-password", "", "CouchDB password")
		couchDB    = flag.String("couchdb-database", "", "CouchDB outbox database (e.g. mst_outbox_mychannel)")
		txIDHex    = flag.String("tx-id", "", "Fabric transaction id (64 hex chars)")
	)
	flag.Parse()

	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
		return 2
	}

	txID, err := canonical.ParseFabricTxID(*txIDHex)
	if err != nil {
		return fail("%v", err)
	}

	var store outbox.Store
	switch {
	case *couchURL != "":
		store, err = outbox.OpenCouchDB(outbox.CouchDBOptions{
			URL: *couchURL, Username: *couchUser, Password: *couchPass, Database: *couchDB,
		})
	case *outboxPath != "":
		store, err = outbox.Open(*outboxPath, nil)
	default:
		return fail("one of --outbox or --couchdb-url is required")
	}
	if err != nil {
		return fail("open outbox: %v", err)
	}
	defer store.Close()

	entry, err := store.Get(txID)
	if err != nil {
		return fail("read entry: %v", err)
	}
	if entry == nil {
		return fail("transaction %s is not in this outbox", *txIDHex)
	}
	if entry.BatchRoot == ([32]byte{}) {
		return fail("transaction %s was anchored individually (no batch); verify it directly with mst-verify", *txIDHex)
	}

	members, err := store.GetBatch(entry.BatchRoot)
	if err != nil {
		return fail("read batch record: %v", err)
	}
	if members == nil {
		return fail("batch record for root %x is missing", entry.BatchRoot)
	}

	leaves := make([][32]byte, 0, len(members))
	for _, member := range members {
		memberEntry, err := store.Get(member)
		if err != nil || memberEntry == nil {
			return fail("batch member %x missing from outbox: %v", member, err)
		}
		leaves = append(leaves, memberEntry.Commitment)
	}

	root, err := canonical.MerkleRoot(leaves)
	if err != nil {
		return fail("rebuild root: %v", err)
	}
	if root != entry.BatchRoot {
		return fail("rebuilt root %x does not match recorded root %x — outbox inconsistent", root, entry.BatchRoot)
	}
	proof, err := canonical.MerkleProof(leaves, entry.Commitment)
	if err != nil {
		return fail("build proof: %v", err)
	}

	out := proofFile{
		FabricTxID: *txIDHex,
		Leaf:       "0x" + hex.EncodeToString(entry.Commitment[:]),
		Root:       "0x" + hex.EncodeToString(root[:]),
		LeafCount:  len(leaves),
		Proof:      make([]string, len(proof)),
	}
	for i, p := range proof {
		out.Proof[i] = "0x" + hex.EncodeToString(p[:])
	}
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return fail("%v", err)
	}
	fmt.Println(string(raw))
	return 0
}
