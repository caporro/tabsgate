package gate

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type store struct {
	db   *sql.DB
	lock *os.File
}
type connection struct {
	ID, Provider, Fingerprint  string
	Secret, WordSalt, WordHash []byte
	Revision, CreatedAt        int64
}
type virtualKey struct {
	ID, ConnectionID, Hash, Prefix, Name                         string
	MaxRequests, TokenBudget, ExpiresAt                          int64
	Models                                                       string
	MaxOutputTokens, RequestsUsed, TokensUsed, EstimatedRequests int64
	Revoked                                                      bool
	InFlight                                                     sql.NullString
	Revision, CreatedAt                                          int64
}

const connectionColumns = "id,provider,fingerprint,secret,word_salt,word_hash,revision,created_at"
const keyColumns = "id,connection_id,token_hash,prefix,name,max_requests,token_budget,expires_at,allowed_models,max_output_tokens,requests_used,tokens_used,estimated_requests,revoked,in_flight,revision,created_at"

type scanner interface{ Scan(...any) error }

func scanConnection(r scanner) (c connection, err error) {
	err = r.Scan(&c.ID, &c.Provider, &c.Fingerprint, &c.Secret, &c.WordSalt, &c.WordHash, &c.Revision, &c.CreatedAt)
	return
}
func scanKey(r scanner) (k virtualKey, err error) {
	err = r.Scan(&k.ID, &k.ConnectionID, &k.Hash, &k.Prefix, &k.Name, &k.MaxRequests, &k.TokenBudget, &k.ExpiresAt, &k.Models, &k.MaxOutputTokens, &k.RequestsUsed, &k.TokensUsed, &k.EstimatedRequests, &k.Revoked, &k.InFlight, &k.Revision, &k.CreatedAt)
	return
}

func openStore(path, marker string) (_ *store, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("database already owned by another Tabs Gate process")
	}
	s := &store{lock: f}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	fdb, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	fdb.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	for _, p := range []string{"foreign_keys(1)", "busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "secure_delete(1)"} {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	s.db, err = sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	if err = s.db.Ping(); err != nil {
		return nil, err
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version > 1 {
		return nil, errors.New("database schema is newer than this binary")
	}
	if version == 0 {
		var tx *sql.Tx
		tx, err = s.db.Begin()
		if err != nil {
			return nil, err
		}
		migration, _ := migrations.ReadFile("migrations/001.sql")
		if _, err = tx.Exec(string(migration)); err == nil {
			_, err = tx.Exec("PRAGMA user_version=1")
		}
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	var existing string
	err = s.db.QueryRow("SELECT value FROM metadata WHERE name='master_key_check'").Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.Exec("INSERT INTO metadata(name,value) VALUES('master_key_check',?)", marker)
	} else if err == nil && !equal(existing, marker) {
		return nil, errors.New("master key does not match this database")
	}
	if err != nil {
		return nil, err
	}
	if err = s.recoverPending(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *store) close() {
	if s.db != nil {
		s.db.Close()
	}
	if s.lock != nil {
		unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
		s.lock.Close()
	}
}

func (s *store) recoverPending() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE virtual_keys SET
 tokens_used=tokens_used+COALESCE((SELECT SUM(fallback_tokens) FROM requests WHERE key_id=virtual_keys.id AND state='in_progress'),0),
 estimated_requests=estimated_requests+(SELECT COUNT(*) FROM requests WHERE key_id=virtual_keys.id AND state='in_progress'),in_flight=NULL`)
	if err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE requests SET state='interrupted',tokens=fallback_tokens,estimated=1,finished_at=? WHERE state='in_progress'", time.Now().Unix())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) getConnection(ctx context.Context, id string) (connection, error) {
	return scanConnection(s.db.QueryRowContext(ctx, "SELECT "+connectionColumns+" FROM connections WHERE id=?", id))
}
func (s *store) getKey(ctx context.Context, id string) (virtualKey, error) {
	return scanKey(s.db.QueryRowContext(ctx, "SELECT "+keyColumns+" FROM virtual_keys WHERE id=?", id))
}

type apiError struct {
	Status        int
	Code, Message string
}

func (e *apiError) Error() string                    { return e.Code }
func problem(status int, code, message string) error { return &apiError{status, code, message} }

func keyBlock(k virtualKey, now int64) error {
	switch {
	case k.Revoked:
		return problem(403, "key_revoked", "This virtual key has been revoked.")
	case now >= k.ExpiresAt:
		return problem(403, "key_expired", "This virtual key has expired.")
	case k.RequestsUsed >= k.MaxRequests:
		return problem(403, "request_limit_reached", "The request limit has been reached.")
	case k.TokensUsed >= k.TokenBudget:
		return problem(403, "token_budget_exhausted", "The token budget has been exhausted.")
	}
	return nil
}

// Admission and accounting use short transactions; no transaction spans network I/O.
func (s *store) admit(ctx context.Context, c connection, k virtualKey, model string, fallback, now int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	current, err := scanKey(tx.QueryRowContext(ctx, "SELECT "+keyColumns+" FROM virtual_keys WHERE id=?", k.ID))
	if err != nil {
		return "", problem(401, "invalid_key", "Invalid virtual key.")
	}
	var revision int64
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM connections WHERE id=?", c.ID).Scan(&revision); err != nil {
		return "", err
	}
	if revision != c.Revision || current.Revision != k.Revision {
		return "", problem(409, "configuration_changed", "Configuration changed; retry the request.")
	}
	if err = keyBlock(current, now); err != nil {
		return "", err
	}
	if current.InFlight.Valid {
		return "", problem(409, "key_busy", "This virtual key already has a request in progress.")
	}
	id := randomToken("req_", 18)
	_, err = tx.ExecContext(ctx, "UPDATE virtual_keys SET requests_used=requests_used+1,in_flight=? WHERE id=?", id, k.ID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO requests(id,key_id,model,state,fallback_tokens,started_at) VALUES(?,?,?,'in_progress',?,?)", id, k.ID, model, fallback, now)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *store) settle(id string, tokens int64, estimated bool, state string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var keyID string
	err = tx.QueryRowContext(ctx, "SELECT key_id FROM requests WHERE id=? AND state='in_progress'", id).Scan(&keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if tokens < 0 || tokens > 1_000_000_000_000 {
		return fmt.Errorf("invalid usage value")
	}
	_, err = tx.ExecContext(ctx, "UPDATE requests SET tokens=?,estimated=?,state=?,finished_at=? WHERE id=?", tokens, estimated, state, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	est := 0
	if estimated {
		est = 1
	}
	_, err = tx.ExecContext(ctx, "UPDATE virtual_keys SET tokens_used=tokens_used+?,estimated_requests=estimated_requests+?,in_flight=NULL WHERE id=? AND in_flight=?", tokens, est, keyID, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
