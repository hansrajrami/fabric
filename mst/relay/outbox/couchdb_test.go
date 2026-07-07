package outbox

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestCouchDBStoreContract runs the identical backend contract suite against
// a real CouchDB. Skipped unless MST_COUCHDB_URL is set, e.g.:
//
//	docker run -d -p 5984:5984 -e COUCHDB_USER=admin -e COUCHDB_PASSWORD=adminpw couchdb:3
//	MST_COUCHDB_URL=http://127.0.0.1:5984 MST_COUCHDB_USER=admin MST_COUCHDB_PASSWORD=adminpw \
//	  go test ./outbox/ -run CouchDB
//
// CI runs it via a couchdb service container.
func TestCouchDBStoreContract(t *testing.T) {
	url := os.Getenv("MST_COUCHDB_URL")
	if url == "" {
		t.Skip("MST_COUCHDB_URL not set; skipping CouchDB backend test")
	}
	var seq atomic.Int64
	runStoreSuite(t, func(t *testing.T) Store {
		// A fresh database per subtest keeps runs independent.
		db := fmt.Sprintf("mst_outbox_test_%d_%d", time.Now().UnixNano(), seq.Add(1))
		s, err := OpenCouchDB(CouchDBOptions{
			URL:      url,
			Username: os.Getenv("MST_COUCHDB_USER"),
			Password: os.Getenv("MST_COUCHDB_PASSWORD"),
			Database: db,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// TestStatusValuesArePinned guards the DONE constant mirrored inside the
// CouchDB design document's active_by_created map function: if the status
// codes ever change, this fails before the view silently miscounts.
func TestStatusValuesArePinned(t *testing.T) {
	pinned := map[Status]uint8{
		StatusPending:     1,
		StatusSubmitted:   2,
		StatusConfirmed:   3,
		StatusWrittenBack: 4,
		StatusDone:        5, // mirrored in couchdb.go designDoc (status !== 5)
	}
	for status, want := range pinned {
		if uint8(status) != want {
			t.Fatalf("status %s changed value: %d (update couchdb.go designDoc!)", status, status)
		}
	}
}
