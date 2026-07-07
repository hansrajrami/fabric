package outbox

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeCouch is a minimal in-process CouchDB emulating exactly the REST
// subset the CouchDB store uses: database creation, document GET/PUT with
// _rev MVCC and 409 conflicts, _bulk_docs with per-document results, and the
// three views of the design document (map+reduce emulated by scanning).
//
// It lets the full backend contract suite run without a real CouchDB; CI
// additionally runs the same suite against a genuine couchdb:3 service
// container, which is the authoritative check.
type fakeCouch struct {
	mu  sync.Mutex
	dbs map[string]map[string]map[string]any // db -> docID -> doc
}

func newFakeCouch() *fakeCouch {
	return &fakeCouch{dbs: map[string]map[string]map[string]any{}}
}

func (f *fakeCouch) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		db := parts[0]
		rest := ""
		if len(parts) == 2 {
			rest = parts[1]
		}

		switch {
		case rest == "" && r.Method == http.MethodPut: // create database
			if _, exists := f.dbs[db]; exists {
				writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": "file_exists"})
				return
			}
			f.dbs[db] = map[string]map[string]any{}
			writeJSON(w, http.StatusCreated, map[string]any{"ok": true})

		case rest == "_bulk_docs" && r.Method == http.MethodPost:
			f.bulkDocs(w, r, db)

		case strings.HasPrefix(rest, "_design/mst/_view/"):
			f.view(w, r, db, strings.TrimPrefix(rest, "_design/mst/_view/"))

		case r.Method == http.MethodGet:
			doc, ok := f.doc(db, rest)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
				return
			}
			writeJSON(w, http.StatusOK, doc)

		case r.Method == http.MethodPut:
			f.putDoc(w, r, db, rest)

		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		}
	})
}

func (f *fakeCouch) doc(db, id string) (map[string]any, bool) {
	docs, ok := f.dbs[db]
	if !ok {
		return nil, false
	}
	doc, ok := docs[id]
	return doc, ok
}

func decodeBody(r *http.Request, into any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func bumpRev(doc map[string]any) {
	gen := 0
	if rev, ok := doc["_rev"].(string); ok {
		gen, _ = strconv.Atoi(strings.SplitN(rev, "-", 2)[0])
	}
	doc["_rev"] = fmt.Sprintf("%d-fake", gen+1)
}

func (f *fakeCouch) putDoc(w http.ResponseWriter, r *http.Request, db, id string) {
	docs, ok := f.dbs[db]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}
	var doc map[string]any
	if err := decodeBody(r, &doc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
		return
	}
	existing, exists := docs[id]
	if exists && existing["_rev"] != doc["_rev"] {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "conflict"})
		return
	}
	if !exists && doc["_rev"] != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "conflict"})
		return
	}
	doc["_id"] = id
	bumpRev(doc)
	docs[id] = doc
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id, "rev": doc["_rev"]})
}

func (f *fakeCouch) bulkDocs(w http.ResponseWriter, r *http.Request, db string) {
	docs, ok := f.dbs[db]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}
	var payload struct {
		Docs []map[string]any `json:"docs"`
	}
	if err := decodeBody(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
		return
	}
	results := make([]map[string]any, 0, len(payload.Docs))
	for _, doc := range payload.Docs {
		id, _ := doc["_id"].(string)
		existing, exists := docs[id]
		if exists && existing["_rev"] != doc["_rev"] {
			results = append(results, map[string]any{"id": id, "error": "conflict"})
			continue
		}
		bumpRev(doc)
		docs[id] = doc
		results = append(results, map[string]any{"id": id, "ok": true, "rev": doc["_rev"]})
	}
	writeJSON(w, http.StatusCreated, results)
}

// view emulates the three design-document views by scanning documents.
func (f *fakeCouch) view(w http.ResponseWriter, r *http.Request, db, name string) {
	docs, ok := f.dbs[db]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}
	q := r.URL.Query()
	limit := -1
	if l := q.Get("limit"); l != "" {
		limit, _ = strconv.Atoi(l)
	}

	num := func(doc map[string]any, field string) int64 {
		v, _ := doc[field].(float64)
		return int64(v)
	}

	switch name {
	case "by_status":
		var statusFilter *int64
		if sk := q.Get("startkey"); sk != "" {
			var key []int64
			if err := json.Unmarshal([]byte(sk), &key); err == nil && len(key) >= 1 {
				statusFilter = &key[0]
			}
		}
		type row struct {
			id  string
			doc map[string]any
		}
		var matched []row
		counts := map[int64]int{}
		for id, doc := range docs {
			if doc["kind"] != kindEntry {
				continue
			}
			st := num(doc, "status")
			counts[st]++
			if statusFilter == nil || st == *statusFilter {
				matched = append(matched, row{id, doc})
			}
		}
		if q.Get("reduce") == "false" {
			sort.Slice(matched, func(i, j int) bool { return matched[i].id < matched[j].id })
			if limit >= 0 && len(matched) > limit {
				matched = matched[:limit]
			}
			rows := make([]map[string]any, 0, len(matched))
			for _, m := range matched {
				rows = append(rows, map[string]any{"id": m.id, "doc": m.doc})
			}
			writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
			return
		}
		// group_level=1 reduce: count per status.
		statuses := make([]int64, 0, len(counts))
		for st := range counts {
			statuses = append(statuses, st)
		}
		sort.Slice(statuses, func(i, j int) bool { return statuses[i] < statuses[j] })
		rows := make([]map[string]any, 0, len(statuses))
		for _, st := range statuses {
			rows = append(rows, map[string]any{"key": []int64{st}, "value": counts[st]})
		}
		writeJSON(w, http.StatusOK, map[string]any{"rows": rows})

	case "active_by_created":
		var createdAts []int64
		for _, doc := range docs {
			if doc["kind"] != kindEntry || num(doc, "status") == int64(StatusDone) {
				continue
			}
			createdAts = append(createdAts, num(doc, "created_at"))
		}
		sort.Slice(createdAts, func(i, j int) bool { return createdAts[i] < createdAts[j] })
		if limit >= 0 && len(createdAts) > limit {
			createdAts = createdAts[:limit]
		}
		rows := make([]map[string]any, 0, len(createdAts))
		for _, at := range createdAts {
			rows = append(rows, map[string]any{"key": at})
		}
		writeJSON(w, http.StatusOK, map[string]any{"rows": rows})

	case "quarantined":
		count := 0
		for _, doc := range docs {
			if doc["kind"] == kindQuarantine {
				count++
			}
		}
		rows := []map[string]any{}
		if count > 0 {
			rows = append(rows, map[string]any{"key": nil, "value": count})
		}
		writeJSON(w, http.StatusOK, map[string]any{"rows": rows})

	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown view"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TestCouchDBStoreContractAgainstFake runs the identical backend contract
// suite against the in-process fake, so the CouchDB client logic is covered
// on every developer machine. The authoritative run against real CouchDB
// happens in CI (see couchdb_test.go).
func TestCouchDBStoreContractAgainstFake(t *testing.T) {
	server := httptest.NewServer(newFakeCouch().handler())
	t.Cleanup(server.Close)

	seq := 0
	runStoreSuite(t, func(t *testing.T) Store {
		seq++
		s, err := OpenCouchDB(CouchDBOptions{
			URL:      server.URL,
			Database: fmt.Sprintf("mst_outbox_fake_%d", seq),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
