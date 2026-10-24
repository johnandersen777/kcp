package pds

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
)

const passwordIterations = 120000

const schemaAccounts = `CREATE TABLE IF NOT EXISTS pds_accounts (
	did TEXT PRIMARY KEY,
	handle TEXT NOT NULL COLLATE NOCASE UNIQUE,
	email TEXT NOT NULL DEFAULT '',
	password_hash BLOB,
	password_salt BLOB,
	password_iter INTEGER NOT NULL DEFAULT 0,
	active INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL
)`

const schemaSessions = `CREATE TABLE IF NOT EXISTS pds_sessions (
	id TEXT PRIMARY KEY,
	did TEXT NOT NULL,
	handle TEXT NOT NULL,
	refresh_jti TEXT NOT NULL UNIQUE,
	app_password INTEGER NOT NULL DEFAULT 0,
	privileged INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	revoked_at INTEGER
)`

const schemaSessionsDID = `CREATE INDEX IF NOT EXISTS pds_sessions_did ON pds_sessions (did)`

const schemaAppPasswords = `CREATE TABLE IF NOT EXISTS pds_app_passwords (
	id TEXT PRIMARY KEY,
	did TEXT NOT NULL,
	name TEXT NOT NULL,
	lookup TEXT NOT NULL,
	password_hash BLOB NOT NULL,
	password_salt BLOB NOT NULL,
	password_iter INTEGER NOT NULL,
	privileged INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	revoked_at INTEGER
)`

const schemaAppPasswordsLookup = `CREATE INDEX IF NOT EXISTS pds_app_passwords_lookup ON pds_app_passwords (lookup)`

type Account struct {
	DID       syntax.DID
	Handle    string
	Email     string
	Active    bool
	CreatedAt time.Time
}

type NewAccount struct {
	DID       syntax.DID
	Handle    string
	Email     string
	Password  string
	CreatedAt time.Time
}

type LegacySession struct {
	ID          string
	DID         syntax.DID
	Handle      string
	RefreshJTI  string
	AppPassword bool
	Privileged  bool
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}

type AppPassword struct {
	Name       string
	Privileged bool
	CreatedAt  time.Time
}

type accountCredential struct {
	Account
	passwordHash []byte
	passwordSalt []byte
	passwordIter int
}

type Store struct {
	db *sql.DB
}

func NewStore(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("pds: empty sqlite path")
	}
	var dsn string
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)"
	} else {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("pds: creating sqlite directory: %w", err)
			}
		}
		dsn = "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("pds: opening sqlite: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}
	db.SetMaxIdleConns(4)
	s := &Store{db: db}
	if err := s.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context) error {
	for _, stmt := range []string{
		schemaAccounts,
		schemaSessions,
		schemaSessionsDID,
		schemaAppPasswords,
		schemaAppPasswordsLookup,
		schemaOAuthSessions,
		schemaOAuthSessionsDID,
		schemaOAuthCodes,
		schemaOAuthPARs,
		schemaOAuthTokens,
		schemaOAuthTokensSession,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("pds: creating schema: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) CreateAccount(ctx context.Context, in NewAccount) (*Account, error) {
	did := in.DID.String()
	if _, err := syntax.ParseDID(did); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	handle := strings.ToLower(strings.TrimSpace(in.Handle))
	if _, err := syntax.ParseHandle(handle); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	created := in.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	var hash, salt []byte
	iter := 0
	if in.Password != "" {
		var err error
		hash, salt, iter, err = hashPassword(in.Password)
		if err != nil {
			return nil, err
		}
	}
	if existing, err := s.GetAccountByIdentifier(ctx, did); err == nil && existing != nil {
		return nil, ErrAccountExists
	}
	if existing, err := s.GetAccountByIdentifier(ctx, handle); err == nil && existing != nil {
		return nil, ErrHandleTaken
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pds_accounts (did, handle, email, password_hash, password_salt, password_iter, active, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		did, handle, in.Email, hash, salt, iter, unixMicro(created))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrAccountExists
		}
		return nil, fmt.Errorf("pds: creating account: %w", err)
	}
	return &Account{
		DID:       in.DID,
		Handle:    handle,
		Email:     in.Email,
		Active:    true,
		CreatedAt: created,
	}, nil
}

func (s *Store) DeleteAccount(ctx context.Context, did syntax.DID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pds_accounts WHERE did = ?`, did.String())
	if err != nil {
		return fmt.Errorf("pds: deleting account: %w", err)
	}
	return nil
}

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var didRaw, handle, email string
	var active int
	var created int64
	if err := row.Scan(&didRaw, &handle, &email, &active, &created); err != nil {
		return nil, err
	}
	did, err := syntax.ParseDID(didRaw)
	if err != nil {
		return nil, err
	}
	return &Account{
		DID:       did,
		Handle:    handle,
		Email:     email,
		Active:    active != 0,
		CreatedAt: fromUnixMicro(created),
	}, nil
}

const accountColumns = `did, handle, email, active, created_at`

func (s *Store) GetAccount(ctx context.Context, did syntax.DID) (*Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM pds_accounts WHERE did = ?`, did.String())
	acct, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading account: %w", err)
	}
	return acct, nil
}

func (s *Store) ListDIDs(ctx context.Context) ([]syntax.DID, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT did FROM pds_accounts ORDER BY did`)
	if err != nil {
		return nil, fmt.Errorf("pds: listing account DIDs: %w", err)
	}
	defer rows.Close()
	var out []syntax.DID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("pds: reading an account DID: %w", err)
		}
		did, err := syntax.ParseDID(raw)
		if err != nil {
			continue
		}
		out = append(out, did)
	}
	return out, rows.Err()
}

func (s *Store) GetAccountByIdentifier(ctx context.Context, identifier string) (*Account, error) {
	identifier = strings.TrimSpace(identifier)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM pds_accounts WHERE did = ? OR handle = ? COLLATE NOCASE`,
		identifier, identifier)
	acct, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading account: %w", err)
	}
	return acct, nil
}

func (s *Store) credentialByIdentifier(ctx context.Context, identifier string) (*accountCredential, error) {
	identifier = strings.TrimSpace(identifier)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+`, password_hash, password_salt, password_iter FROM pds_accounts
		 WHERE did = ? OR handle = ? COLLATE NOCASE`,
		identifier, identifier)
	var didRaw, handle, email string
	var active, iter int
	var created int64
	var hash, salt []byte
	if err := row.Scan(&didRaw, &handle, &email, &active, &created, &hash, &salt, &iter); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("pds: loading credentials: %w", err)
	}
	did, err := syntax.ParseDID(didRaw)
	if err != nil {
		return nil, err
	}
	return &accountCredential{
		Account: Account{
			DID:       did,
			Handle:    handle,
			Email:     email,
			Active:    active != 0,
			CreatedAt: fromUnixMicro(created),
		},
		passwordHash: hash,
		passwordSalt: salt,
		passwordIter: iter,
	}, nil
}

func (s *Store) VerifyPassword(ctx context.Context, identifier, password string) (*Account, bool, bool, error) {
	cred, err := s.credentialByIdentifier(ctx, identifier)
	if err != nil {
		return nil, false, false, ErrInvalidCredentials
	}
	if !cred.Active {
		return nil, false, false, ErrInvalidCredentials
	}
	if len(cred.passwordHash) > 0 && verifyPassword(password, cred.passwordSalt, cred.passwordHash, cred.passwordIter) {
		acct := cred.Account
		return &acct, false, false, nil
	}
	ok, privileged, err := s.verifyAppPassword(ctx, cred.DID, password)
	if err != nil {
		return nil, false, false, err
	}
	if ok {
		acct := cred.Account
		return &acct, true, privileged, nil
	}
	return nil, false, false, ErrInvalidCredentials
}

func (s *Store) Authenticate(ctx context.Context, identifier, password string) (oauth.Account, error) {
	cred, err := s.credentialByIdentifier(ctx, identifier)
	if err != nil || !cred.Active || len(cred.passwordHash) == 0 {
		return oauth.Account{}, oauth.ErrInvalidCredentials
	}
	if !verifyPassword(password, cred.passwordSalt, cred.passwordHash, cred.passwordIter) {
		return oauth.Account{}, oauth.ErrInvalidCredentials
	}
	return oauth.Account{DID: cred.DID, Handle: cred.Handle}, nil
}

func (s *Store) CreateLegacySession(ctx context.Context, sess LegacySession) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pds_sessions (id, did, handle, refresh_jti, app_password, privileged, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.DID.String(), sess.Handle, sess.RefreshJTI,
		boolInt(sess.AppPassword), boolInt(sess.Privileged),
		unixMicro(sess.CreatedAt), unixMicro(sess.ExpiresAt))
	if err != nil {
		return fmt.Errorf("pds: creating session: %w", err)
	}
	return nil
}

const sessionColumns = `id, did, handle, refresh_jti, app_password, privileged, created_at, expires_at, revoked_at`

func scanLegacySession(row interface{ Scan(...any) error }) (*LegacySession, error) {
	var id, didRaw, handle, refreshJTI string
	var appPassword, privileged int
	var created, expires int64
	var revoked sql.NullInt64
	if err := row.Scan(&id, &didRaw, &handle, &refreshJTI, &appPassword, &privileged, &created, &expires, &revoked); err != nil {
		return nil, err
	}
	did, err := syntax.ParseDID(didRaw)
	if err != nil {
		return nil, err
	}
	return &LegacySession{
		ID:          id,
		DID:         did,
		Handle:      handle,
		RefreshJTI:  refreshJTI,
		AppPassword: appPassword != 0,
		Privileged:  privileged != 0,
		CreatedAt:   fromUnixMicro(created),
		ExpiresAt:   fromUnixMicro(expires),
		RevokedAt:   scanTimePtr(revoked),
	}, nil
}

func (s *Store) GetLegacySession(ctx context.Context, id string) (*LegacySession, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM pds_sessions WHERE id = ?`, id)
	sess, err := scanLegacySession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading session: %w", err)
	}
	return sess, nil
}

func (s *Store) GetLegacySessionByRefresh(ctx context.Context, jti string) (*LegacySession, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM pds_sessions WHERE refresh_jti = ?`, jti)
	sess, err := scanLegacySession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading session: %w", err)
	}
	return sess, nil
}

func (s *Store) RotateLegacySession(ctx context.Context, id, refreshJTI string, expiresAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pds_sessions SET refresh_jti = ?, expires_at = ? WHERE id = ? AND revoked_at IS NULL`,
		refreshJTI, unixMicro(expiresAt), id)
	if err != nil {
		return fmt.Errorf("pds: rotating session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}

func (s *Store) RevokeLegacySession(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pds_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		unixMicro(at), id)
	if err != nil {
		return fmt.Errorf("pds: revoking session: %w", err)
	}
	return nil
}

func (s *Store) RevokeLegacySessionByRefresh(ctx context.Context, jti string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pds_sessions SET revoked_at = ? WHERE refresh_jti = ? AND revoked_at IS NULL`,
		unixMicro(at), jti)
	if err != nil {
		return fmt.Errorf("pds: revoking session: %w", err)
	}
	return nil
}

func (s *Store) CreateAppPassword(ctx context.Context, did syntax.DID, name, password string, privileged bool) (*AppPassword, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return nil, fmt.Errorf("%w: app password name must be 1-64 characters", ErrUnsupported)
	}
	var existing int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM pds_app_passwords WHERE did = ? AND name = ? AND revoked_at IS NULL`,
		did.String(), name).Scan(&existing)
	if err == nil {
		return nil, ErrAppPasswordExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("pds: checking app password: %w", err)
	}
	hash, salt, iter, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	id, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO pds_app_passwords (id, did, name, lookup, password_hash, password_salt, password_iter, privileged, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, did.String(), name, appPasswordLookup(password), hash, salt, iter, boolInt(privileged), unixMicro(now))
	if err != nil {
		return nil, fmt.Errorf("pds: creating app password: %w", err)
	}
	return &AppPassword{Name: name, Privileged: privileged, CreatedAt: now}, nil
}

func (s *Store) ListAppPasswords(ctx context.Context, did syntax.DID) ([]AppPassword, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, privileged, created_at FROM pds_app_passwords
		 WHERE did = ? AND revoked_at IS NULL ORDER BY created_at`, did.String())
	if err != nil {
		return nil, fmt.Errorf("pds: listing app passwords: %w", err)
	}
	defer rows.Close()
	out := []AppPassword{}
	for rows.Next() {
		var name string
		var privileged int
		var created int64
		if err := rows.Scan(&name, &privileged, &created); err != nil {
			return nil, fmt.Errorf("pds: listing app passwords: %w", err)
		}
		out = append(out, AppPassword{Name: name, Privileged: privileged != 0, CreatedAt: fromUnixMicro(created)})
	}
	return out, rows.Err()
}

func (s *Store) RevokeAppPassword(ctx context.Context, did syntax.DID, name string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pds_app_passwords SET revoked_at = ? WHERE did = ? AND name = ? AND revoked_at IS NULL`,
		unixMicro(at), did.String(), name)
	if err != nil {
		return fmt.Errorf("pds: revoking app password: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAppPasswordMissing
	}
	return nil
}

func (s *Store) verifyAppPassword(ctx context.Context, did syntax.DID, password string) (bool, bool, error) {
	var hash, salt []byte
	var iter, privileged int
	var didRaw string
	err := s.db.QueryRowContext(ctx,
		`SELECT did, password_hash, password_salt, password_iter, privileged FROM pds_app_passwords
		 WHERE lookup = ? AND revoked_at IS NULL ORDER BY created_at DESC LIMIT 1`,
		appPasswordLookup(password)).Scan(&didRaw, &hash, &salt, &iter, &privileged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("pds: loading app password: %w", err)
	}
	if didRaw != did.String() {
		return false, false, nil
	}
	if !verifyPassword(password, salt, hash, iter) {
		return false, false, nil
	}
	return true, privileged != 0, nil
}

func GenerateAppPassword() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("pds: generating app password: %w", err)
	}
	enc := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	return enc[0:5] + "-" + enc[5:10] + "-" + enc[10:15] + "-" + enc[15:20], nil
}

func hashPassword(password string) ([]byte, []byte, int, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, 0, fmt.Errorf("pds: generating salt: %w", err)
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("pds: hashing password: %w", err)
	}
	return hash, salt, passwordIterations, nil
}

func verifyPassword(password string, salt, expected []byte, iter int) bool {
	if len(expected) == 0 || len(salt) == 0 || iter <= 0 {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, password, salt, iter, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func appPasswordLookup(password string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(password))))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("pds: generating random token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func unixMicro(t time.Time) int64 {
	return t.UTC().UnixMicro()
}

func fromUnixMicro(v int64) time.Time {
	return time.UnixMicro(v).UTC()
}

func scanTimePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnixMicro(v.Int64)
	return &t
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
