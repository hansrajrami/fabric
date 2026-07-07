package outbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// Store is the durable outbox contract. Implementations must make every
// method crash-atomic: after a crash, the store reflects a prefix of the
// operations issued, never a torn one.
type Store interface {
	// PutBlock inserts the given entries as PENDING and advances the capture
	// checkpoint to nextBlock, all in one atomic write. Entries whose
	// FabricTxID already exists are skipped (idempotent redelivery); the
	// count of newly inserted entries is returned.
	PutBlock(entries []*Entry, nextBlock uint64) (int, error)

	// Get returns the entry for txID, or nil if absent.
	Get(txID [32]byte) (*Entry, error)

	// Transition atomically moves the entry from status `from` to `to`,
	// applying mutate (if non-nil) to update auxiliary fields. It fails if
	// the entry is absent, not currently in `from`, or the transition is not
	// part of the state machine.
	Transition(txID [32]byte, from, to Status, mutate func(*Entry)) (*Entry, error)

	// Update atomically mutates auxiliary fields (attempts, retry time, EVM
	// tx hash) of an entry that must currently be in status `expect`,
	// without changing its status.
	Update(txID [32]byte, expect Status, mutate func(*Entry)) (*Entry, error)

	// ListByStatus returns up to limit entries currently in status (limit<=0
	// means no limit), ordered by FabricTxID.
	ListByStatus(status Status, limit int) ([]*Entry, error)

	// Checkpoint returns the next block number to process and whether a
	// checkpoint has been stored yet.
	Checkpoint() (uint64, bool, error)

	// SetCheckpoint stores the next block number to process.
	SetCheckpoint(next uint64) error

	// Quarantine records a malformed proof request (poison pill) so the
	// stream is never wedged and nothing is silently dropped.
	Quarantine(txID [32]byte, blockNumber uint64, reason string, raw []byte) error

	// Stats reports backlog by status, the oldest non-terminal entry's
	// CreatedAt (0 if none), and the quarantine count.
	Stats() (Stats, error)

	Close() error
}

// Stats is the outbox backlog report.
type Stats struct {
	CountByStatus   map[Status]int
	OldestActiveAge int64 // unix seconds of the oldest non-DONE entry's CreatedAt; 0 if none
	Quarantined     int
}

// Sentinel errors.
var (
	ErrNotFound          = errors.New("outbox: entry not found")
	ErrWrongStatus       = errors.New("outbox: entry not in expected status")
	ErrBadTransition     = errors.New("outbox: transition not allowed")
	ErrCheckpointGoback  = errors.New("outbox: checkpoint may not move backwards")
	ErrClosed            = errors.New("outbox: store closed")
)

// Key layout. Fixed single-byte prefixes keep iteration ranges tight.
//
//	e/<txid 32B>            -> entry JSON
//	s/<status 1B><txid 32B> -> nil (status index, maintained atomically)
//	c                       -> 8-byte BE next block (capture checkpoint)
//	q/<txid 32B>            -> quarantine record JSON
const (
	prefixEntry      = 'e'
	prefixStatus     = 's'
	keyCheckpoint    = 'c'
	prefixQuarantine = 'q'
)

func entryKey(txID [32]byte) []byte {
	k := make([]byte, 1+32)
	k[0] = prefixEntry
	copy(k[1:], txID[:])
	return k
}

func statusKey(status Status, txID [32]byte) []byte {
	k := make([]byte, 2+32)
	k[0] = prefixStatus
	k[1] = byte(status)
	copy(k[2:], txID[:])
	return k
}

func quarantineKey(txID [32]byte) []byte {
	k := make([]byte, 1+32)
	k[0] = prefixQuarantine
	copy(k[1:], txID[:])
	return k
}

// LevelDB is the embedded goleveldb implementation of Store.
type LevelDB struct {
	db *leveldb.DB

	// mu serializes read-modify-write cycles (CAS transitions, idempotent
	// inserts). LevelDB batches are atomic on disk but not isolated from
	// concurrent writers, so the lock provides the isolation.
	mu     sync.Mutex
	closed bool

	wo *opt.WriteOptions
}

var _ Store = (*LevelDB)(nil)

// Options tunes the store.
type Options struct {
	// NoSync disables fsync on writes. ONLY for tests/benchmarks: without
	// sync, a machine crash can lose acknowledged writes.
	NoSync bool
}

// Open opens (or creates) the outbox database at path.
func Open(path string, o *Options) (*LevelDB, error) {
	db, err := leveldb.OpenFile(path, &opt.Options{
		// The outbox is small and hot; default table sizes are fine. Strict
		// journal checksum verification on recovery.
		Strict: opt.DefaultStrict,
	})
	if err != nil {
		return nil, fmt.Errorf("outbox: open %s: %w", path, err)
	}
	sync := o == nil || !o.NoSync
	return &LevelDB{db: db, wo: &opt.WriteOptions{Sync: sync}}, nil
}

func (s *LevelDB) PutBlock(entries []*Entry, nextBlock uint64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}

	batch := new(leveldb.Batch)
	inserted := 0
	now := nowUnix()
	for _, e := range entries {
		exists, err := s.db.Has(entryKey(e.FabricTxID), nil)
		if err != nil {
			return 0, fmt.Errorf("outbox: has: %w", err)
		}
		if exists {
			continue // idempotent redelivery
		}
		cp := e.clone()
		cp.Status = StatusPending
		if cp.EntryType == "" {
			cp.EntryType = EntryTypeCommitmentV1
		}
		cp.CreatedAt = now
		cp.UpdatedAt = now
		raw, err := cp.marshal()
		if err != nil {
			return 0, fmt.Errorf("outbox: marshal: %w", err)
		}
		batch.Put(entryKey(cp.FabricTxID), raw)
		batch.Put(statusKey(StatusPending, cp.FabricTxID), nil)
		inserted++
	}

	var cpv [8]byte
	binary.BigEndian.PutUint64(cpv[:], nextBlock)
	batch.Put([]byte{keyCheckpoint}, cpv[:])

	if err := s.db.Write(batch, s.wo); err != nil {
		return 0, fmt.Errorf("outbox: write block batch: %w", err)
	}
	return inserted, nil
}

func (s *LevelDB) Get(txID [32]byte) (*Entry, error) {
	raw, err := s.db.Get(entryKey(txID), nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outbox: get: %w", err)
	}
	return unmarshalEntry(raw)
}

func (s *LevelDB) Transition(txID [32]byte, from, to Status, mutate func(*Entry)) (*Entry, error) {
	if !allowedTransitions[from][to] {
		return nil, fmt.Errorf("%w: %s -> %s", ErrBadTransition, from, to)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}

	e, err := s.Get(txID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("%w: %x", ErrNotFound, txID)
	}
	if e.Status != from {
		return nil, fmt.Errorf("%w: %x is %s, expected %s", ErrWrongStatus, txID, e.Status, from)
	}

	e.Status = to
	if mutate != nil {
		mutate(e)
		if e.Status != to {
			return nil, fmt.Errorf("%w: mutate must not change status", ErrBadTransition)
		}
	}
	e.UpdatedAt = nowUnix()

	raw, err := e.marshal()
	if err != nil {
		return nil, fmt.Errorf("outbox: marshal: %w", err)
	}
	batch := new(leveldb.Batch)
	batch.Put(entryKey(txID), raw)
	batch.Delete(statusKey(from, txID))
	batch.Put(statusKey(to, txID), nil)
	if err := s.db.Write(batch, s.wo); err != nil {
		return nil, fmt.Errorf("outbox: write transition: %w", err)
	}
	return e.clone(), nil
}

func (s *LevelDB) Update(txID [32]byte, expect Status, mutate func(*Entry)) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	e, err := s.Get(txID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("%w: %x", ErrNotFound, txID)
	}
	if e.Status != expect {
		return nil, fmt.Errorf("%w: %x is %s, expected %s", ErrWrongStatus, txID, e.Status, expect)
	}
	if mutate != nil {
		mutate(e)
		if e.Status != expect {
			return nil, fmt.Errorf("%w: Update must not change status", ErrBadTransition)
		}
	}
	e.UpdatedAt = nowUnix()
	raw, err := e.marshal()
	if err != nil {
		return nil, fmt.Errorf("outbox: marshal: %w", err)
	}
	if err := s.db.Put(entryKey(txID), raw, s.wo); err != nil {
		return nil, fmt.Errorf("outbox: write update: %w", err)
	}
	return e.clone(), nil
}

func (s *LevelDB) ListByStatus(status Status, limit int) ([]*Entry, error) {
	iter := s.db.NewIterator(util.BytesPrefix([]byte{prefixStatus, byte(status)}), nil)
	defer iter.Release()

	var out []*Entry
	for iter.Next() {
		if limit > 0 && len(out) >= limit {
			break
		}
		key := iter.Key()
		var txID [32]byte
		copy(txID[:], key[2:])
		e, err := s.Get(txID)
		if err != nil {
			return nil, err
		}
		if e == nil {
			// Index without entry can only mean a bug; surface loudly.
			return nil, fmt.Errorf("outbox: dangling status index for %x", txID)
		}
		out = append(out, e)
	}
	return out, iter.Error()
}

func (s *LevelDB) Checkpoint() (uint64, bool, error) {
	raw, err := s.db.Get([]byte{keyCheckpoint}, nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("outbox: checkpoint: %w", err)
	}
	if len(raw) != 8 {
		return 0, false, fmt.Errorf("outbox: corrupt checkpoint (%d bytes)", len(raw))
	}
	return binary.BigEndian.Uint64(raw), true, nil
}

func (s *LevelDB) SetCheckpoint(next uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	current, ok, err := s.checkpointLocked()
	if err != nil {
		return err
	}
	if ok && next < current {
		return fmt.Errorf("%w: %d < %d", ErrCheckpointGoback, next, current)
	}
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], next)
	return s.db.Put([]byte{keyCheckpoint}, v[:], s.wo)
}

func (s *LevelDB) checkpointLocked() (uint64, bool, error) {
	return s.Checkpoint()
}

type quarantineDTO struct {
	BlockNumber uint64 `json:"block_number"`
	Reason      string `json:"reason"`
	RawHex      string `json:"raw_hex"`
	At          int64  `json:"at"`
}

func (s *LevelDB) Quarantine(txID [32]byte, blockNumber uint64, reason string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	rec, err := marshalQuarantine(blockNumber, reason, raw)
	if err != nil {
		return err
	}
	return s.db.Put(quarantineKey(txID), rec, s.wo)
}

func (s *LevelDB) Stats() (Stats, error) {
	stats := Stats{CountByStatus: make(map[Status]int, len(statusNames))}
	var oldest int64
	for _, status := range AllStatuses() {
		iter := s.db.NewIterator(util.BytesPrefix([]byte{prefixStatus, byte(status)}), nil)
		for iter.Next() {
			stats.CountByStatus[status]++
			if status == StatusDone {
				continue
			}
			var txID [32]byte
			copy(txID[:], iter.Key()[2:])
			e, err := s.Get(txID)
			if err != nil {
				iter.Release()
				return Stats{}, err
			}
			if e != nil && (oldest == 0 || e.CreatedAt < oldest) {
				oldest = e.CreatedAt
			}
		}
		err := iter.Error()
		iter.Release()
		if err != nil {
			return Stats{}, err
		}
	}
	stats.OldestActiveAge = oldest

	qIter := s.db.NewIterator(util.BytesPrefix([]byte{prefixQuarantine}), nil)
	for qIter.Next() {
		stats.Quarantined++
	}
	err := qIter.Error()
	qIter.Release()
	return stats, err
}

func (s *LevelDB) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}
