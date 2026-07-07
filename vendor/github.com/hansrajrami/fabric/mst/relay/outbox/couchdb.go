package outbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// CouchDB is the CouchDB-backed Store implementation, used when the peer's
// state database is CouchDB (the outbox then lives on the same CouchDB
// server, in its own dedicated database — never inside the peer's own state
// databases).
//
// Correctness model: CouchDB has no multi-document transactions, so instead
// of LevelDB's atomic batch this backend relies on WRITE ORDERING plus
// idempotency — a block's entry documents are written (and durably accepted)
// BEFORE the checkpoint document advances. A crash in between merely causes
// the block to be redelivered, and entry inserts are idempotent by
// fabric_tx_id, so exactly-once capture still holds. Status transitions use
// CouchDB's _rev MVCC as the CAS.
type CouchDB struct {
	client   *http.Client
	base     *url.URL // server URL
	db       string
	username string
	password string

	mu     sync.Mutex
	closed bool
}

var _ Store = (*CouchDB)(nil)

// CouchDBOptions locates the server and target database.
type CouchDBOptions struct {
	// URL of the CouchDB server, e.g. "http://127.0.0.1:5984". A bare
	// "host:port" (the peer's couchDBAddress format) is treated as http.
	URL      string
	Username string
	Password string
	// Database to use; created if absent. Must satisfy CouchDB naming rules.
	Database string
	// Timeout per HTTP request (default 30s).
	Timeout time.Duration
}

// document kinds
const (
	kindEntry      = "entry"
	kindQuarantine = "quarantine"
)

const (
	entryIDPrefix      = "e:"
	quarantineIDPrefix = "q:"
	batchIDPrefix      = "b:"
	checkpointDocID    = "c:checkpoint"
	designDocID        = "_design/mst"
)

// designDoc holds the views the store needs. NOTE: the DONE status value (5)
// is mirrored in the active_by_created map function; it is pinned by
// TestStatusValuesArePinned next to the Go constants.
var designDoc = map[string]any{
	"_id":      designDocID,
	"language": "javascript",
	"views": map[string]any{
		"by_status": map[string]any{
			"map":    `function(doc){ if(doc.kind === "entry"){ emit([doc.status, doc._id], null); } }`,
			"reduce": "_count",
		},
		"active_by_created": map[string]any{
			"map": `function(doc){ if(doc.kind === "entry" && doc.status !== 5){ emit(doc.created_at, null); } }`,
		},
		"quarantined": map[string]any{
			"map":    `function(doc){ if(doc.kind === "quarantine"){ emit(doc._id, null); } }`,
			"reduce": "_count",
		},
	},
}

// OpenCouchDB connects, creates the database if needed, and installs the
// design document.
func OpenCouchDB(opts CouchDBOptions) (*CouchDB, error) {
	raw := opts.URL
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("outbox: couchdb url %q: %w", opts.URL, err)
	}
	if opts.Database == "" {
		return nil, fmt.Errorf("outbox: couchdb database name is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	s := &CouchDB{
		client:   &http.Client{Timeout: timeout},
		base:     base,
		db:       opts.Database,
		username: opts.Username,
		password: opts.Password,
	}

	// Create the database (201) or accept that it exists (412).
	status, _, err := s.do(http.MethodPut, s.db, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated && status != http.StatusPreconditionFailed {
		return nil, fmt.Errorf("outbox: create couchdb database %s: HTTP %d", s.db, status)
	}
	if err := s.ensureDesignDoc(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *CouchDB) ensureDesignDoc() error {
	var existing map[string]any
	status, body, err := s.do(http.MethodGet, s.path(designDocID), nil)
	if err != nil {
		return err
	}
	doc := make(map[string]any, len(designDoc))
	for k, v := range designDoc {
		doc[k] = v
	}
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &existing); err != nil {
			return fmt.Errorf("outbox: parse design doc: %w", err)
		}
		// Only rewrite when the views changed (avoids view-index rebuilds).
		existingViews, _ := json.Marshal(existing["views"])
		wantViews, _ := json.Marshal(designDoc["views"])
		if bytes.Equal(existingViews, wantViews) {
			return nil
		}
		doc["_rev"] = existing["_rev"]
	}
	status, body, err = s.do(http.MethodPut, s.path(designDocID), doc)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("outbox: install design doc: HTTP %d: %s", status, body)
	}
	return nil
}

// ---- document shapes ----

// couchEntryDoc wraps entryDTO with CouchDB metadata.
type couchEntryDoc struct {
	ID   string `json:"_id"`
	Rev  string `json:"_rev,omitempty"`
	Kind string `json:"kind"`
	entryDTO
}

func entryDocID(txID [32]byte) string {
	return entryIDPrefix + fmt.Sprintf("%x", txID)
}

func toEntryDoc(e *Entry, rev string) (*couchEntryDoc, error) {
	raw, err := e.marshal()
	if err != nil {
		return nil, err
	}
	var dto entryDTO
	if err := json.Unmarshal(raw, &dto); err != nil {
		return nil, err
	}
	return &couchEntryDoc{ID: entryDocID(e.FabricTxID), Rev: rev, Kind: kindEntry, entryDTO: dto}, nil
}

func (d *couchEntryDoc) toEntry() (*Entry, error) {
	raw, err := json.Marshal(d.entryDTO)
	if err != nil {
		return nil, err
	}
	return unmarshalEntry(raw)
}

// ---- Store implementation ----

func (s *CouchDB) PutBlock(entries []*Entry, nextBlock uint64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}

	inserted := 0
	var docs []any
	now := nowUnix()
	for _, e := range entries {
		existing, _, err := s.getEntryDoc(e.FabricTxID)
		if err != nil {
			return 0, err
		}
		if existing != nil {
			continue // idempotent redelivery
		}
		cp := e.clone()
		cp.Status = StatusPending
		if cp.EntryType == "" {
			cp.EntryType = EntryTypeCommitmentV1
		}
		cp.CreatedAt = now
		cp.UpdatedAt = now
		doc, err := toEntryDoc(cp, "")
		if err != nil {
			return 0, err
		}
		docs = append(docs, doc)
		inserted++
	}

	// Entries FIRST, durably; only then the checkpoint may move.
	if len(docs) > 0 {
		status, body, err := s.do(http.MethodPost, s.path("_bulk_docs"), map[string]any{"docs": docs})
		if err != nil {
			return 0, err
		}
		if status != http.StatusCreated {
			return 0, fmt.Errorf("outbox: bulk insert: HTTP %d: %s", status, body)
		}
		var results []struct {
			ID    string `json:"id"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &results); err != nil {
			return 0, fmt.Errorf("outbox: bulk insert response: %w", err)
		}
		for _, r := range results {
			if r.OK {
				continue
			}
			if r.Error == "conflict" {
				inserted-- // raced with another writer; entry exists — fine
				continue
			}
			return 0, fmt.Errorf("outbox: bulk insert %s: %s", r.ID, r.Error)
		}
	}

	if err := s.advanceCheckpointLocked(nextBlock); err != nil {
		return 0, err
	}
	return inserted, nil
}

func (s *CouchDB) Get(txID [32]byte) (*Entry, error) {
	doc, _, err := s.getEntryDoc(txID)
	if err != nil || doc == nil {
		return nil, err
	}
	return doc.toEntry()
}

func (s *CouchDB) getEntryDoc(txID [32]byte) (*couchEntryDoc, string, error) {
	status, body, err := s.do(http.MethodGet, s.path(entryDocID(txID)), nil)
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusNotFound {
		return nil, "", nil
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("outbox: get entry: HTTP %d", status)
	}
	var doc couchEntryDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "", fmt.Errorf("outbox: corrupt entry doc: %w", err)
	}
	return &doc, doc.Rev, nil
}

func (s *CouchDB) Transition(txID [32]byte, from, to Status, mutate func(*Entry)) (*Entry, error) {
	if !allowedTransitions[from][to] {
		return nil, fmt.Errorf("%w: %s -> %s", ErrBadTransition, from, to)
	}
	return s.readModifyWrite(txID, from, func(e *Entry) error {
		e.Status = to
		if mutate != nil {
			mutate(e)
			if e.Status != to {
				return fmt.Errorf("%w: mutate must not change status", ErrBadTransition)
			}
		}
		return nil
	})
}

func (s *CouchDB) Update(txID [32]byte, expect Status, mutate func(*Entry)) (*Entry, error) {
	return s.readModifyWrite(txID, expect, func(e *Entry) error {
		if mutate != nil {
			mutate(e)
			if e.Status != expect {
				return fmt.Errorf("%w: Update must not change status", ErrBadTransition)
			}
		}
		return nil
	})
}

func (s *CouchDB) readModifyWrite(txID [32]byte, expect Status, apply func(*Entry) error) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}

	doc, rev, err := s.getEntryDoc(txID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: %x", ErrNotFound, txID)
	}
	e, err := doc.toEntry()
	if err != nil {
		return nil, err
	}
	if e.Status != expect {
		return nil, fmt.Errorf("%w: %x is %s, expected %s", ErrWrongStatus, txID, e.Status, expect)
	}
	if err := apply(e); err != nil {
		return nil, err
	}
	e.UpdatedAt = nowUnix()

	updated, err := toEntryDoc(e, rev)
	if err != nil {
		return nil, err
	}
	status, body, err := s.do(http.MethodPut, s.path(entryDocID(txID)), updated)
	if err != nil {
		return nil, err
	}
	if status == http.StatusConflict {
		// _rev CAS lost against a concurrent writer: same contract as the
		// LevelDB CAS — exactly one winner.
		return nil, fmt.Errorf("%w: %x concurrently modified", ErrWrongStatus, txID)
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("outbox: write entry: HTTP %d: %s", status, body)
	}
	return e.clone(), nil
}

func (s *CouchDB) ListByStatus(st Status, limit int) ([]*Entry, error) {
	params := url.Values{}
	params.Set("reduce", "false")
	params.Set("include_docs", "true")
	params.Set("startkey", fmt.Sprintf(`[%d]`, st))
	params.Set("endkey", fmt.Sprintf(`[%d,{}]`, st))
	if limit > 0 {
		params.Set("limit", fmt.Sprint(limit))
	}
	status, body, err := s.do(http.MethodGet, s.path("_design/mst/_view/by_status")+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("outbox: by_status view: HTTP %d: %s", status, body)
	}
	var result struct {
		Rows []struct {
			Doc json.RawMessage `json:"doc"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("outbox: by_status response: %w", err)
	}
	out := make([]*Entry, 0, len(result.Rows))
	for _, row := range result.Rows {
		var doc couchEntryDoc
		if err := json.Unmarshal(row.Doc, &doc); err != nil {
			return nil, fmt.Errorf("outbox: corrupt entry doc: %w", err)
		}
		e, err := doc.toEntry()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].FabricTxID[:], out[j].FabricTxID[:]) < 0
	})
	return out, nil
}

type checkpointDoc struct {
	ID   string `json:"_id"`
	Rev  string `json:"_rev,omitempty"`
	Next uint64 `json:"next"`
}

func (s *CouchDB) Checkpoint() (uint64, bool, error) {
	doc, err := s.getCheckpoint()
	if err != nil || doc == nil {
		return 0, false, err
	}
	return doc.Next, true, nil
}

func (s *CouchDB) getCheckpoint() (*checkpointDoc, error) {
	status, body, err := s.do(http.MethodGet, s.path(checkpointDocID), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("outbox: get checkpoint: HTTP %d", status)
	}
	var doc checkpointDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("outbox: corrupt checkpoint: %w", err)
	}
	return &doc, nil
}

func (s *CouchDB) SetCheckpoint(next uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	current, err := s.getCheckpoint()
	if err != nil {
		return err
	}
	if current != nil && next < current.Next {
		return fmt.Errorf("%w: %d < %d", ErrCheckpointGoback, next, current.Next)
	}
	return s.putCheckpoint(current, next)
}

// advanceCheckpointLocked moves the checkpoint forward; a lower value (block
// redelivery) is silently kept at the maximum.
func (s *CouchDB) advanceCheckpointLocked(next uint64) error {
	current, err := s.getCheckpoint()
	if err != nil {
		return err
	}
	if current != nil && next <= current.Next {
		return nil
	}
	return s.putCheckpoint(current, next)
}

func (s *CouchDB) putCheckpoint(current *checkpointDoc, next uint64) error {
	doc := checkpointDoc{ID: checkpointDocID, Next: next}
	if current != nil {
		doc.Rev = current.Rev
	}
	status, body, err := s.do(http.MethodPut, s.path(checkpointDocID), doc)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("outbox: write checkpoint: HTTP %d: %s", status, body)
	}
	return nil
}

type couchQuarantineDoc struct {
	ID   string `json:"_id"`
	Rev  string `json:"_rev,omitempty"`
	Kind string `json:"kind"`
	quarantineDTO
}

func (s *CouchDB) Quarantine(txID [32]byte, blockNumber uint64, reason string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	id := quarantineIDPrefix + fmt.Sprintf("%x", txID)

	rec, err := marshalQuarantine(blockNumber, reason, raw)
	if err != nil {
		return err
	}
	var dto quarantineDTO
	if err := json.Unmarshal(rec, &dto); err != nil {
		return err
	}
	doc := couchQuarantineDoc{ID: id, Kind: kindQuarantine, quarantineDTO: dto}

	// Overwrite-on-retry semantics, matching LevelDB.
	status, body, err := s.do(http.MethodGet, s.path(id), nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		var existing struct {
			Rev string `json:"_rev"`
		}
		if err := json.Unmarshal(body, &existing); err == nil {
			doc.Rev = existing.Rev
		}
	}
	status, body, err = s.do(http.MethodPut, s.path(id), doc)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("outbox: write quarantine: HTTP %d: %s", status, body)
	}
	return nil
}

type couchBatchDoc struct {
	ID   string `json:"_id"`
	Rev  string `json:"_rev,omitempty"`
	Kind string `json:"kind"`
	batchDTO
}

func (s *CouchDB) PutBatch(root [32]byte, txIDs [][32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	raw, err := marshalBatch(txIDs)
	if err != nil {
		return err
	}
	var dto batchDTO
	if err := json.Unmarshal(raw, &dto); err != nil {
		return err
	}
	id := batchIDPrefix + fmt.Sprintf("%x", root)
	doc := couchBatchDoc{ID: id, Kind: "batch", batchDTO: dto}
	// Overwrite-safe: fetch the current rev if the record exists.
	status, body, err := s.do(http.MethodGet, s.path(id), nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		var existing struct {
			Rev string `json:"_rev"`
		}
		if err := json.Unmarshal(body, &existing); err == nil {
			doc.Rev = existing.Rev
		}
	}
	status, body, err = s.do(http.MethodPut, s.path(id), doc)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("outbox: write batch record: HTTP %d: %s", status, body)
	}
	return nil
}

func (s *CouchDB) GetBatch(root [32]byte) ([][32]byte, error) {
	id := batchIDPrefix + fmt.Sprintf("%x", root)
	status, body, err := s.do(http.MethodGet, s.path(id), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("outbox: get batch: HTTP %d", status)
	}
	var doc couchBatchDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("outbox: corrupt batch doc: %w", err)
	}
	raw, err := json.Marshal(doc.batchDTO)
	if err != nil {
		return nil, err
	}
	return unmarshalBatch(raw)
}

func (s *CouchDB) Stats() (Stats, error) {
	stats := Stats{CountByStatus: make(map[Status]int, len(statusNames))}

	// Counts per status from the reduced view.
	status, body, err := s.do(http.MethodGet, s.path("_design/mst/_view/by_status")+"?group_level=1", nil)
	if err != nil {
		return Stats{}, err
	}
	if status != http.StatusOK {
		return Stats{}, fmt.Errorf("outbox: status counts: HTTP %d: %s", status, body)
	}
	var counts struct {
		Rows []struct {
			Key   []json.Number `json:"key"`
			Value int           `json:"value"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &counts); err != nil {
		return Stats{}, fmt.Errorf("outbox: status counts response: %w", err)
	}
	for _, row := range counts.Rows {
		if len(row.Key) != 1 {
			continue
		}
		code, err := row.Key[0].Int64()
		if err != nil {
			continue
		}
		stats.CountByStatus[Status(code)] = row.Value
	}

	// Oldest active entry: the active_by_created view is keyed by created_at.
	status, body, err = s.do(http.MethodGet, s.path("_design/mst/_view/active_by_created")+"?limit=1", nil)
	if err != nil {
		return Stats{}, err
	}
	if status != http.StatusOK {
		return Stats{}, fmt.Errorf("outbox: oldest active: HTTP %d: %s", status, body)
	}
	var oldest struct {
		Rows []struct {
			Key int64 `json:"key"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &oldest); err != nil {
		return Stats{}, fmt.Errorf("outbox: oldest active response: %w", err)
	}
	if len(oldest.Rows) > 0 {
		stats.OldestActiveAge = oldest.Rows[0].Key
	}

	// Quarantine count.
	status, body, err = s.do(http.MethodGet, s.path("_design/mst/_view/quarantined"), nil)
	if err != nil {
		return Stats{}, err
	}
	if status != http.StatusOK {
		return Stats{}, fmt.Errorf("outbox: quarantine count: HTTP %d: %s", status, body)
	}
	var quarantined struct {
		Rows []struct {
			Value int `json:"value"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &quarantined); err != nil {
		return Stats{}, fmt.Errorf("outbox: quarantine count response: %w", err)
	}
	if len(quarantined.Rows) > 0 {
		stats.Quarantined = quarantined.Rows[0].Value
	}
	return stats, nil
}

func (s *CouchDB) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// ---- HTTP plumbing ----

func (s *CouchDB) path(p string) string {
	return s.db + "/" + p
}

func (s *CouchDB) do(method, p string, payload any) (int, []byte, error) {
	var reqBody io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("outbox: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}
	u := *s.base
	// p may already carry query parameters.
	if i := strings.IndexByte(p, '?'); i >= 0 {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + p[:i]
		u.RawQuery = p[i+1:]
	} else {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + p
	}
	req, err := http.NewRequest(method, u.String(), reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("outbox: build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.username != "" {
		req.SetBasicAuth(s.username, s.password)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("outbox: couchdb %s %s: %w", method, p, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("outbox: read response: %w", err)
	}
	return resp.StatusCode, body, nil
}
