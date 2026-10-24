package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigorepo "github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/repo/mst"
	"github.com/bluesky-social/indigo/atproto/syntax"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const schemaRepos = `CREATE TABLE IF NOT EXISTS atrepo_repos (
	did TEXT PRIMARY KEY,
	root TEXT NOT NULL,
	commit_cid TEXT NOT NULL,
	rev TEXT NOT NULL,
	pubkey TEXT NOT NULL,
	created_at INTEGER NOT NULL
)`

const schemaBlocks = `CREATE TABLE IF NOT EXISTS atrepo_blocks (
	did TEXT NOT NULL,
	cid TEXT NOT NULL,
	data BLOB NOT NULL,
	PRIMARY KEY (did, cid)
) WITHOUT ROWID`

const schemaCommits = `CREATE TABLE IF NOT EXISTS atrepo_commits (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	did TEXT NOT NULL,
	cid TEXT NOT NULL,
	rev TEXT NOT NULL,
	data TEXT NOT NULL,
	prev TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (did, rev),
	UNIQUE (did, cid)
)`

const schemaCommitsIndex = `CREATE INDEX IF NOT EXISTS atrepo_commits_did_seq ON atrepo_commits (did, seq)`

const schemaEvents = `CREATE TABLE IF NOT EXISTS atrepo_events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	did TEXT NOT NULL,
	rev TEXT NOT NULL,
	since_rev TEXT NOT NULL,
	commit_cid TEXT NOT NULL,
	data TEXT NOT NULL,
	prev_data TEXT NOT NULL,
	blocks BLOB NOT NULL,
	ops TEXT NOT NULL,
	blobs TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (did, rev)
)`

const schemaEventsIndex = `CREATE INDEX IF NOT EXISTS atrepo_events_did_seq ON atrepo_events (did, seq)`

type SQLiteStore struct {
	db      *sql.DB
	writeMu sync.Mutex
	sink    EventSink
	clk     *syntax.TIDClock
}

type repoRow struct {
	root   cid.Cid
	commit cid.Cid
	rev    syntax.TID
	pubkey string
}

func NewSQLiteStore(ctx context.Context, path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, fmt.Errorf("repo: empty sqlite path")
	}
	dsn := ""
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	} else {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("repo: creating sqlite directory: %w", err)
			}
		}
		dsn = "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("repo: opening sqlite: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}
	db.SetMaxIdleConns(4)
	s := &SQLiteStore{
		db:  db,
		clk: syntax.NewTIDClock(0),
	}
	if err := s.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) init(ctx context.Context) error {
	for _, stmt := range []string{
		schemaRepos,
		schemaBlocks,
		schemaCommits,
		schemaCommitsIndex,
		schemaEvents,
		schemaEventsIndex,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("repo: creating schema: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) SetEventSink(sink EventSink) {
	s.writeMu.Lock()
	s.sink = sink
	s.writeMu.Unlock()
}

func (s *SQLiteStore) nextTID() syntax.TID {
	return s.clk.Next()
}

func (s *SQLiteStore) CreateRepo(ctx context.Context, did syntax.DID, key atcrypto.PrivateKey) (Repo, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if key == nil {
		return nil, fmt.Errorf("%w: signing key required", ErrInvalidRequest)
	}
	pub, err := key.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("repo: deriving public key: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	exists, err := s.repoExists(ctx, d)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrRepoExists
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("repo: beginning create: %w", err)
	}
	defer tx.Rollback()
	bs := newTxBlockstore(ctx, tx, d.String())
	tree := mst.NewEmptyTree()
	root, err := tree.WriteDiffBlocks(ctx, bs)
	if err != nil {
		return nil, fmt.Errorf("repo: writing empty mst: %w", err)
	}
	rev := s.nextTID()
	commit := &Commit{
		DID:     d.String(),
		Version: indigorepo.ATPROTO_REPO_VERSION,
		Data:    *root,
		Rev:     rev.String(),
	}
	if err := commit.Sign(key); err != nil {
		return nil, fmt.Errorf("repo: signing genesis commit: %w", err)
	}
	cdata, ccid, err := marshalCommit(commit)
	if err != nil {
		return nil, fmt.Errorf("repo: encoding genesis commit: %w", err)
	}
	cblk, err := blocks.NewBlockWithCid(cdata, ccid)
	if err != nil {
		return nil, fmt.Errorf("repo: building genesis commit block: %w", err)
	}
	if err := bs.Put(ctx, cblk); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO atrepo_repos (did, root, commit_cid, rev, pubkey, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		d.String(), root.String(), ccid.String(), rev.String(), pub.Multibase(), now.UnixMicro()); err != nil {
		return nil, fmt.Errorf("repo: inserting repo: %w", err)
	}
	cr := &CommitRecord{
		DID:    d,
		CID:    ccid,
		Rev:    rev,
		Data:   *root,
		Signed: commit,
		Time:   now,
	}
	if err := insertCommitRow(ctx, tx, d.String(), cr); err != nil {
		return nil, err
	}
	carBytes, err := WriteCAR(ccid, bs.orderedFrom(ccid))
	if err != nil {
		return nil, err
	}
	ev := &CommitEvent{
		Repo:   d,
		Commit: ccid,
		Rev:    rev,
		Data:   *root,
		Blocks: carBytes,
		Time:   now,
	}
	seq, err := insertEventRow(ctx, tx, ev)
	if err != nil {
		return nil, err
	}
	ev.Seq = seq
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("repo: committing repo creation: %w", err)
	}
	handle := &repoHandle{store: s, did: d, key: key, pub: pub}
	if sink := s.sink; sink != nil {
		if err := sink.OnCommit(ctx, ev); err != nil {
			return handle, err
		}
	}
	return handle, nil
}

func (s *SQLiteStore) OpenRepo(ctx context.Context, did syntax.DID, key atcrypto.PrivateKey) (Repo, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	row, err := s.loadRepoRow(ctx, d)
	if err != nil {
		return nil, err
	}
	pub, err := atcrypto.ParsePublicMultibase(row.pubkey)
	if err != nil {
		return nil, fmt.Errorf("%w: stored public key: %v", ErrCorruptRepo, err)
	}
	cr, err := s.loadCommitByCID(ctx, s.db, d.String(), row.commit)
	if err != nil {
		return nil, err
	}
	if cr.Rev != row.rev || cr.Data != row.root {
		return nil, fmt.Errorf("%w: repo head does not match stored commit", ErrCorruptRepo)
	}
	if key != nil {
		kp, err := key.PublicKey()
		if err != nil {
			return nil, fmt.Errorf("repo: deriving public key: %w", err)
		}
		if !kp.Equal(pub) {
			return nil, fmt.Errorf("%w: signing key does not match repo key", ErrInvalidRequest)
		}
	}
	return &repoHandle{store: s, did: d, key: key, pub: pub}, nil
}

func (s *SQLiteStore) DeleteRepo(ctx context.Context, did syntax.DID) error {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("repo: beginning delete: %w", err)
	}
	defer tx.Rollback()
	for _, table := range []string{"atrepo_events", "atrepo_commits", "atrepo_blocks"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE did = ?", d.String()); err != nil {
			return fmt.Errorf("repo: deleting from %s: %w", table, err)
		}
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM atrepo_repos WHERE did = ?", d.String())
	if err != nil {
		return fmt.Errorf("repo: deleting repo: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRepoNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("repo: committing delete: %w", err)
	}
	return nil
}

func (s *SQLiteStore) RepoExists(ctx context.Context, did syntax.DID) (bool, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return s.repoExists(ctx, d)
}

func (s *SQLiteStore) repoExists(ctx context.Context, did syntax.DID) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM atrepo_repos WHERE did = ?", did.String()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLiteStore) ListRepos(ctx context.Context) ([]syntax.DID, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT did FROM atrepo_repos ORDER BY did")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []syntax.DID{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		d, err := syntax.ParseDID(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: stored did: %v", ErrCorruptRepo, err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) loadRepoRow(ctx context.Context, did syntax.DID) (*repoRow, error) {
	var rootRaw, commitRaw, revRaw, pubkey string
	err := s.db.QueryRowContext(ctx,
		"SELECT root, commit_cid, rev, pubkey FROM atrepo_repos WHERE did = ?", did.String()).
		Scan(&rootRaw, &commitRaw, &revRaw, &pubkey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRepoNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repo: loading repo: %w", err)
	}
	root, err := parseCID(rootRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: stored root: %v", ErrCorruptRepo, err)
	}
	commit, err := parseCID(commitRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: stored commit: %v", ErrCorruptRepo, err)
	}
	rev, err := syntax.ParseTID(revRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: stored rev: %v", ErrCorruptRepo, err)
	}
	return &repoRow{root: root, commit: commit, rev: rev, pubkey: pubkey}, nil
}

func insertCommitRow(ctx context.Context, tx *sql.Tx, did string, cr *CommitRecord) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO atrepo_commits (did, cid, rev, data, prev, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		did, cr.CID.String(), cr.Rev.String(), cr.Data.String(), formatCIDPtr(cr.Prev), cr.Time.UnixMicro())
	if err != nil {
		return fmt.Errorf("repo: inserting commit: %w", err)
	}
	return nil
}

func insertEventRow(ctx context.Context, tx *sql.Tx, ev *CommitEvent) (int64, error) {
	ops, err := encodeOps(ev.Ops)
	if err != nil {
		return 0, err
	}
	blobs, err := encodeCIDs(ev.Blobs)
	if err != nil {
		return 0, err
	}
	since := ""
	if ev.Since != nil {
		since = ev.Since.String()
	}
	res, err := tx.ExecContext(ctx,
		"INSERT INTO atrepo_events (did, rev, since_rev, commit_cid, data, prev_data, blocks, ops, blobs, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		ev.Repo.String(), ev.Rev.String(), since, ev.Commit.String(), ev.Data.String(), formatCIDPtr(ev.PrevData), ev.Blocks, ops, blobs, ev.Time.UnixMicro())
	if err != nil {
		return 0, fmt.Errorf("repo: inserting event: %w", err)
	}
	return res.LastInsertId()
}

func (s *SQLiteStore) loadCommitByCID(ctx context.Context, q blockQueryer, did string, commitCID cid.Cid) (*CommitRecord, error) {
	var seq, created int64
	var revRaw, dataRaw, prevRaw string
	err := q.QueryRowContext(ctx,
		"SELECT seq, rev, data, prev, created_at FROM atrepo_commits WHERE did = ? AND cid = ?",
		did, commitCID.String()).Scan(&seq, &revRaw, &dataRaw, &prevRaw, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCommitNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repo: loading commit: %w", err)
	}
	blk, err := readBlock(ctx, q, did, commitCID)
	if err != nil {
		return nil, err
	}
	commit, err := parseCommitBlock(blk.RawData())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptRepo, err)
	}
	if err := commit.VerifyStructure(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptRepo, err)
	}
	rev, err := syntax.ParseTID(revRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: commit rev: %v", ErrCorruptRepo, err)
	}
	data, err := parseCID(dataRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: commit data: %v", ErrCorruptRepo, err)
	}
	prev, err := parseCIDPtr(prevRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: commit prev: %v", ErrCorruptRepo, err)
	}
	if commit.DID != did || commit.Rev != rev.String() || commit.Data != data {
		return nil, fmt.Errorf("%w: commit fields do not match stored row", ErrCorruptRepo)
	}
	if formatCIDPtr(prev) != formatCIDPtr(commit.Prev) {
		return nil, fmt.Errorf("%w: commit prev does not match stored row", ErrCorruptRepo)
	}
	d, err := syntax.ParseDID(did)
	if err != nil {
		return nil, fmt.Errorf("%w: stored did: %v", ErrCorruptRepo, err)
	}
	return &CommitRecord{
		DID:    d,
		CID:    commitCID,
		Rev:    rev,
		Data:   data,
		Prev:   prev,
		Signed: commit,
		Seq:    seq,
		Time:   time.UnixMicro(created).UTC(),
	}, nil
}

func (s *SQLiteStore) Head(ctx context.Context, did syntax.DID) (*CommitRecord, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	row, err := s.loadRepoRow(ctx, d)
	if err != nil {
		return nil, err
	}
	cr, err := s.loadCommitByCID(ctx, s.db, d.String(), row.commit)
	if err != nil {
		return nil, err
	}
	if cr.Rev != row.rev || cr.Data != row.root {
		return nil, fmt.Errorf("%w: head does not match repo row", ErrCorruptRepo)
	}
	return cr, nil
}

func (s *SQLiteStore) GetCommit(ctx context.Context, did syntax.DID, rev syntax.TID) (*CommitRecord, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	r, err := syntax.ParseTID(string(rev))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	var cidRaw string
	err = s.db.QueryRowContext(ctx, "SELECT cid FROM atrepo_commits WHERE did = ? AND rev = ?", d.String(), r.String()).Scan(&cidRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCommitNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repo: loading commit rev: %w", err)
	}
	c, err := parseCID(cidRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: stored commit cid: %v", ErrCorruptRepo, err)
	}
	return s.loadCommitByCID(ctx, s.db, d.String(), c)
}

func (s *SQLiteStore) ListCommits(ctx context.Context, did syntax.DID, since *syntax.TID, limit int) ([]CommitRecord, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	limit = min(opt.Positive(limit, DefaultCommits), MaxCommits)
	sinceRaw := ""
	if since != nil {
		r, err := syntax.ParseTID(string(*since))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		sinceRaw = r.String()
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT cid FROM atrepo_commits WHERE did = ? AND rev > ? ORDER BY seq ASC LIMIT ?",
		d.String(), sinceRaw, limit)
	if err != nil {
		return nil, err
	}
	var cids []cid.Cid
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		c, err := parseCID(raw)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("%w: stored commit cid: %v", ErrCorruptRepo, err)
		}
		cids = append(cids, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]CommitRecord, 0, len(cids))
	for _, c := range cids {
		cr, err := s.loadCommitByCID(ctx, s.db, d.String(), c)
		if err != nil {
			return nil, err
		}
		out = append(out, *cr)
	}
	return out, nil
}

func (s *SQLiteStore) ListEvents(ctx context.Context, since int64, limit int) ([]*CommitEvent, error) {
	limit = min(opt.Positive(limit, DefaultCommits), MaxCommits)
	rows, err := s.db.QueryContext(ctx,
		"SELECT seq, did, rev, since_rev, commit_cid, data, prev_data, blocks, ops, blobs, created_at FROM atrepo_events WHERE seq > ? ORDER BY seq ASC LIMIT ?",
		since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CommitEvent{}
	for rows.Next() {
		var seq, created int64
		var didRaw, revRaw, sinceRaw, commitRaw, dataRaw, prevRaw string
		var carBytes, opsBytes, blobBytes []byte
		if err := rows.Scan(&seq, &didRaw, &revRaw, &sinceRaw, &commitRaw, &dataRaw, &prevRaw, &carBytes, &opsBytes, &blobBytes, &created); err != nil {
			return nil, err
		}
		d, err := syntax.ParseDID(didRaw)
		if err != nil {
			return nil, fmt.Errorf("%w: event did: %v", ErrCorruptRepo, err)
		}
		rev, err := syntax.ParseTID(revRaw)
		if err != nil {
			return nil, fmt.Errorf("%w: event rev: %v", ErrCorruptRepo, err)
		}
		var sincePtr *syntax.TID
		if sinceRaw != "" {
			s, err := syntax.ParseTID(sinceRaw)
			if err != nil {
				return nil, fmt.Errorf("%w: event since: %v", ErrCorruptRepo, err)
			}
			sincePtr = &s
		}
		commit, err := parseCID(commitRaw)
		if err != nil {
			return nil, fmt.Errorf("%w: event commit: %v", ErrCorruptRepo, err)
		}
		data, err := parseCID(dataRaw)
		if err != nil {
			return nil, fmt.Errorf("%w: event data: %v", ErrCorruptRepo, err)
		}
		prevData, err := parseCIDPtr(prevRaw)
		if err != nil {
			return nil, fmt.Errorf("%w: event prev data: %v", ErrCorruptRepo, err)
		}
		ops, err := decodeOps(opsBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: event ops: %v", ErrCorruptRepo, err)
		}
		blobs, err := decodeCIDs(blobBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: event blobs: %v", ErrCorruptRepo, err)
		}
		out = append(out, &CommitEvent{
			Repo:     d,
			Seq:      seq,
			Commit:   commit,
			Rev:      rev,
			Since:    sincePtr,
			Data:     data,
			PrevData: prevData,
			Blocks:   carBytes,
			Ops:      ops,
			Blobs:    blobs,
			Time:     time.UnixMicro(created).UTC(),
		})
	}
	return out, rows.Err()
}

func (s *SQLiteStore) headRoot(ctx context.Context, did syntax.DID) (cid.Cid, *CommitRecord, error) {
	head, err := s.Head(ctx, did)
	if err != nil {
		return cid.Undef, nil, err
	}
	return head.Data, head, nil
}

func (s *SQLiteStore) GetRecord(ctx context.Context, did syntax.DID, collection syntax.NSID, rkey syntax.RecordKey) (*Record, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	nsid, err := syntax.ParseNSID(string(collection))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	rk, err := syntax.ParseRecordKey(string(rkey))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	root, _, err := s.headRoot(ctx, d)
	if err != nil {
		return nil, err
	}
	br := &sqlBlockReader{db: s.db, did: d.String()}
	tree, err := loadFullTree(ctx, br, root)
	if err != nil {
		return nil, err
	}
	atr := &indigorepo.Repo{
		DID:         d,
		Clock:       syntax.NewTIDClock(0),
		RecordStore: br,
		MST:         *tree,
	}
	data, rc, err := atr.GetRecordBytes(ctx, nsid, rk)
	if errors.Is(err, indigorepo.ErrNotFound) {
		return nil, ErrRecordNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repo: reading record: %w", err)
	}
	value, err := decodeRecord(data)
	if err != nil {
		return nil, fmt.Errorf("%w: record data: %v", ErrCorruptRepo, err)
	}
	return &Record{
		Collection: nsid,
		Rkey:       rk,
		CID:        *rc,
		Bytes:      data,
		Value:      value,
	}, nil
}

func (s *SQLiteStore) ListRecords(ctx context.Context, did syntax.DID, collection syntax.NSID, cursor syntax.RecordKey, limit int) ([]Record, *syntax.RecordKey, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	nsid, err := syntax.ParseNSID(string(collection))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	limit = min(opt.Positive(limit, DefaultList), MaxList)
	cursorRaw := ""
	if cursor != "" {
		rk, err := syntax.ParseRecordKey(string(cursor))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		cursorRaw = rk.String()
	}
	root, _, err := s.headRoot(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	br := &sqlBlockReader{db: s.db, did: d.String()}
	tree, err := loadFullTree(ctx, br, root)
	if err != nil {
		return nil, nil, err
	}
	prefix := nsid.String() + "/"
	out := make([]Record, 0, limit)
	var last syntax.RecordKey
	more := false
	stop := errors.New("stop")
	err = tree.Walk(func(key []byte, val cid.Cid) error {
		path := string(key)
		if !strings.HasPrefix(path, prefix) {
			return nil
		}
		rkeyRaw := strings.TrimPrefix(path, prefix)
		rk, err := syntax.ParseRecordKey(rkeyRaw)
		if err != nil {
			return fmt.Errorf("%w: mst key: %v", ErrCorruptRepo, err)
		}
		if cursorRaw != "" && rk.String() <= cursorRaw {
			return nil
		}
		if len(out) == limit {
			more = true
			return stop
		}
		blk, err := br.Get(ctx, val)
		if err != nil {
			return err
		}
		value, err := decodeRecord(blk.RawData())
		if err != nil {
			return fmt.Errorf("%w: record data: %v", ErrCorruptRepo, err)
		}
		out = append(out, Record{
			Collection: nsid,
			Rkey:       rk,
			CID:        val,
			Bytes:      blk.RawData(),
			Value:      value,
		})
		last = rk
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return nil, nil, err
	}
	var next *syntax.RecordKey
	if more {
		next = &last
	}
	return out, next, nil
}

func (s *SQLiteStore) CAR(ctx context.Context, did syntax.DID, since *syntax.TID) ([]byte, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	head, err := s.Head(ctx, d)
	if err != nil {
		return nil, err
	}
	if since == nil || *since == "" {
		return s.fullCAR(ctx, d, head)
	}
	sinceRev, err := syntax.ParseTID(string(*since))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if _, err := s.GetCommit(ctx, d, sinceRev); errors.Is(err, ErrCommitNotFound) {
		return s.fullCAR(ctx, d, head)
	} else if err != nil {
		return nil, err
	}
	if sinceRev == head.Rev {
		blk, err := readBlock(ctx, s.db, d.String(), head.CID)
		if err != nil {
			return nil, err
		}
		return WriteCAR(head.CID, []blocks.Block{blk})
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT blocks FROM atrepo_events WHERE did = ? AND rev > ? ORDER BY seq ASC",
		d.String(), sinceRev.String())
	if err != nil {
		return nil, err
	}
	seen := make(map[cid.Cid]bool)
	var ordered []blocks.Block
	headBlk, err := readBlock(ctx, s.db, d.String(), head.CID)
	if err != nil {
		rows.Close()
		return nil, err
	}
	seen[head.CID] = true
	ordered = append(ordered, headBlk)
	for rows.Next() {
		var carBytes []byte
		if err := rows.Scan(&carBytes); err != nil {
			rows.Close()
			return nil, err
		}
		_, blks, err := ReadCARBytes(ctx, carBytes)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("%w: event car: %v", ErrCorruptRepo, err)
		}
		for _, blk := range blks {
			if seen[blk.Cid()] {
				continue
			}
			seen[blk.Cid()] = true
			ordered = append(ordered, blk)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return WriteCAR(head.CID, ordered)
}

func (s *SQLiteStore) fullCAR(ctx context.Context, did syntax.DID, head *CommitRecord) ([]byte, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT cid, data FROM atrepo_blocks WHERE did = ? ORDER BY cid", did.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[cid.Cid]bool)
	var ordered []blocks.Block
	var headBlk blocks.Block
	for rows.Next() {
		var raw string
		var data []byte
		if err := rows.Scan(&raw, &data); err != nil {
			return nil, err
		}
		c, err := parseCID(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: stored block cid: %v", ErrCorruptRepo, err)
		}
		if err := verifyBlock(c, data); err != nil {
			return nil, err
		}
		blk, err := blocks.NewBlockWithCid(data, c)
		if err != nil {
			return nil, err
		}
		if c == head.CID {
			headBlk = blk
			seen[c] = true
			continue
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		ordered = append(ordered, blk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if headBlk == nil {
		return nil, fmt.Errorf("%w: head commit block missing", ErrCorruptRepo)
	}
	ordered = append([]blocks.Block{headBlk}, ordered...)
	return WriteCAR(head.CID, ordered)
}

func (s *SQLiteStore) CommitCAR(ctx context.Context, did syntax.DID, commit cid.Cid) ([]byte, error) {
	d, err := syntax.ParseDID(string(did))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if !commit.Defined() {
		return nil, fmt.Errorf("%w: commit cid required", ErrInvalidRequest)
	}
	var carBytes []byte
	err = s.db.QueryRowContext(ctx,
		"SELECT blocks FROM atrepo_events WHERE did = ? AND commit_cid = ? ORDER BY seq DESC LIMIT 1",
		d.String(), commit.String()).Scan(&carBytes)
	if err == nil {
		return carBytes, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	blk, err := readBlock(ctx, s.db, d.String(), commit)
	if err != nil {
		return nil, err
	}
	return WriteCAR(commit, []blocks.Block{blk})
}

var _ RepoStore = (*SQLiteStore)(nil)
