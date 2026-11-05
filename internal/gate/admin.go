package gate

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

type manageHandler func(http.ResponseWriter, *http.Request, connection)

func (s *Server) manage(next manageHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, word := bearer(r), r.Header.Get("X-TabsGate-Keyword")
		if !validSecret(key) || !validWord(word) {
			fail(w, problem(401, "invalid_credentials", "Provider key and keyword are required."))
			return
		}
		c, err := s.store.getConnection(r.Context(), r.PathValue("id"))
		if err != nil || !equal(c.Fingerprint, s.vault.fingerprint(c.Provider, key)) {
			fail(w, problem(401, "invalid_credentials", "Invalid management credentials."))
			return
		}
		if !s.hashSlot() {
			fail(w, problem(429, "rate_limited", "Management is busy; retry later."))
			return
		}
		ok := checkWord(word, c.WordSalt, c.WordHash)
		<-s.argon
		if !ok {
			fail(w, problem(401, "invalid_credentials", "Invalid management credentials."))
			return
		}
		next(w, r, c)
	}
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		Keyword  string `json:"keyword"`
	}
	if err := decodeJSON(w, r, &input, 16<<10); err != nil {
		fail(w, err)
		return
	}
	if _, ok := s.providers[input.Provider]; !ok {
		fail(w, problem(400, "invalid_provider", "Unsupported provider."))
		return
	}
	if !validSecret(input.APIKey) || !validWord(input.Keyword) {
		fail(w, problem(400, "invalid_credentials", "A valid provider key and nonempty keyword are required."))
		return
	}
	fp := s.vault.fingerprint(input.Provider, input.APIKey)
	if !s.hashSlot() {
		fail(w, problem(429, "rate_limited", "Management is busy; retry later."))
		return
	}
	defer func() { <-s.argon }()
	c, err := scanConnection(s.store.db.QueryRowContext(r.Context(), "SELECT "+connectionColumns+" FROM connections WHERE provider=? AND fingerprint=?", input.Provider, fp))
	if err == nil {
		if !checkWord(input.Keyword, c.WordSalt, c.WordHash) {
			fail(w, problem(401, "invalid_credentials", "Invalid management credentials."))
			return
		}
		s.connectionJSON(w, r, c, 200)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		fail(w, err)
		return
	}
	c = connection{ID: randomToken("con_", 18), Provider: input.Provider, Fingerprint: fp, Revision: 1, CreatedAt: s.now().Unix()}
	c.Secret = s.vault.encrypt(c.ID, c.Provider, input.APIKey)
	c.WordSalt, c.WordHash = hashWord(input.Keyword)
	tx, err := s.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow("SELECT COUNT(*) FROM connections").Scan(&count); err != nil {
		fail(w, err)
		return
	}
	if count >= s.cfg.MaxConnections {
		fail(w, problem(503, "capacity_reached", "Connection capacity reached."))
		return
	}
	var existing int
	if err = tx.QueryRow("SELECT COUNT(*) FROM connections WHERE provider=? AND fingerprint=?", c.Provider, fp).Scan(&existing); err != nil {
		fail(w, err)
		return
	}
	if existing != 0 {
		fail(w, problem(409, "connection_exists", "Connection was registered concurrently; repeat the request to open it."))
		return
	}
	_, err = tx.Exec("INSERT INTO connections("+connectionColumns+") VALUES(?,?,?,?,?,?,?,?)", c.ID, c.Provider, c.Fingerprint, c.Secret, c.WordSalt, c.WordHash, c.Revision, c.CreatedAt)
	if err != nil {
		fail(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	s.connectionJSON(w, r, c, 201)
}

func (s *Server) connectionJSON(w http.ResponseWriter, r *http.Request, c connection, status int) {
	var count, requests, tokens int64
	err := s.store.db.QueryRowContext(r.Context(), "SELECT COUNT(*),COALESCE(SUM(requests_used),0),COALESCE(SUM(tokens_used),0) FROM virtual_keys WHERE connection_id=?", c.ID).Scan(&count, &requests, &tokens)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, status, map[string]any{"id": c.ID, "provider": c.Provider, "created_at": stamp(c.CreatedAt), "revision": c.Revision, "key_count": count, "requests_used": requests, "tokens_used": tokens})
}
func (s *Server) readConnection(w http.ResponseWriter, r *http.Request, c connection) {
	s.connectionJSON(w, r, c, 200)
}

func (s *Server) patchConnection(w http.ResponseWriter, r *http.Request, c connection) {
	var input struct {
		APIKey  *string `json:"api_key"`
		Keyword *string `json:"keyword"`
	}
	if err := decodeJSON(w, r, &input, 16<<10); err != nil {
		fail(w, err)
		return
	}
	if input.APIKey == nil && input.Keyword == nil {
		fail(w, problem(400, "empty_patch", "Supply api_key or keyword."))
		return
	}
	if input.APIKey != nil {
		if !validSecret(*input.APIKey) {
			fail(w, problem(400, "invalid_key", "Invalid provider key."))
			return
		}
		c.Fingerprint = s.vault.fingerprint(c.Provider, *input.APIKey)
		c.Secret = s.vault.encrypt(c.ID, c.Provider, *input.APIKey)
	}
	if input.Keyword != nil {
		if !validWord(*input.Keyword) {
			fail(w, problem(400, "invalid_keyword", "A nonempty keyword is required."))
			return
		}
		if !s.hashSlot() {
			fail(w, problem(429, "rate_limited", "Management is busy; retry later."))
			return
		}
		c.WordSalt, c.WordHash = hashWord(*input.Keyword)
		<-s.argon
	}
	tx, err := s.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	var duplicate int
	if err = tx.QueryRow("SELECT COUNT(*) FROM connections WHERE provider=? AND fingerprint=? AND id<>?", c.Provider, c.Fingerprint, c.ID).Scan(&duplicate); err != nil {
		fail(w, err)
		return
	}
	if duplicate != 0 {
		fail(w, problem(409, "connection_exists", "The replacement key already belongs to a connection."))
		return
	}
	res, err := tx.Exec("UPDATE connections SET fingerprint=?,secret=?,word_salt=?,word_hash=?,revision=revision+1 WHERE id=? AND revision=?", c.Fingerprint, c.Secret, c.WordSalt, c.WordHash, c.ID, c.Revision)
	if err = changed(res, err); err != nil {
		fail(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	c.Revision++
	s.connectionJSON(w, r, c, 200)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request, c connection) {
	res, err := s.store.db.ExecContext(r.Context(), "DELETE FROM connections WHERE id=? AND revision=?", c.ID, c.Revision)
	if err = changed(res, err); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

type keyInput struct {
	Name            *string    `json:"name"`
	MaxRequests     *int64     `json:"max_requests"`
	TokenBudget     *int64     `json:"token_budget"`
	TTLSeconds      *int64     `json:"ttl_seconds"`
	ExpiresAt       *time.Time `json:"expires_at"`
	AllowedModels   *[]string  `json:"allowed_models"`
	MaxOutputTokens *int64     `json:"max_output_tokens"`
}

func (s *Server) applyKeyInput(k *virtualKey, in keyInput, create bool) error {
	bad := func() error {
		return problem(400, "invalid_limits", "Require positive max_requests, token_budget, TTL/expiry and a nonempty allowed_models list. See API limits in the README.")
	}
	if create && (in.MaxRequests == nil || in.TokenBudget == nil || in.AllowedModels == nil || (in.TTLSeconds == nil && in.ExpiresAt == nil)) {
		return bad()
	}
	if in.Name != nil {
		if len(*in.Name) > 160 {
			return bad()
		}
		k.Name = *in.Name
	}
	if in.MaxRequests != nil {
		if *in.MaxRequests < 1 || *in.MaxRequests > 1_000_000 {
			return bad()
		}
		k.MaxRequests = *in.MaxRequests
	}
	if in.TokenBudget != nil {
		if *in.TokenBudget < 1 || *in.TokenBudget > 1_000_000_000_000 {
			return bad()
		}
		k.TokenBudget = *in.TokenBudget
	}
	if in.TTLSeconds != nil && in.ExpiresAt != nil {
		return bad()
	}
	if in.TTLSeconds != nil {
		if *in.TTLSeconds < 1 || *in.TTLSeconds > 365*86400 {
			return bad()
		}
		k.ExpiresAt = s.now().Unix() + *in.TTLSeconds
	}
	if in.ExpiresAt != nil {
		k.ExpiresAt = in.ExpiresAt.Unix()
		if k.ExpiresAt <= s.now().Unix() || k.ExpiresAt > s.now().Unix()+365*86400 {
			return bad()
		}
	}
	if in.AllowedModels != nil {
		models := *in.AllowedModels
		if len(models) < 1 || len(models) > 100 {
			return bad()
		}
		seen := map[string]bool{}
		for _, m := range models {
			if len(m) < 1 || len(m) > 200 || strings.TrimSpace(m) != m || seen[m] {
				return bad()
			}
			for _, ch := range m {
				if ch < 33 || ch > 126 {
					return bad()
				}
			}
			seen[m] = true
		}
		b, _ := json.Marshal(models)
		k.Models = string(b)
	}
	if in.MaxOutputTokens != nil {
		k.MaxOutputTokens = *in.MaxOutputTokens
	}
	if k.MaxOutputTokens < 1 || k.MaxOutputTokens > s.cfg.MaxOutputTokens {
		return bad()
	}
	return nil
}

func stamp(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }
func modelList(k virtualKey) []string {
	var m []string
	_ = json.Unmarshal([]byte(k.Models), &m)
	return m
}
func allowedModel(k virtualKey, model string) bool { return slices.Contains(modelList(k), model) }
func keyView(k virtualKey, now int64) map[string]any {
	state := "active"
	if err := keyBlock(k, now); err != nil {
		state = err.(*apiError).Code
	}
	return map[string]any{"id": k.ID, "name": k.Name, "prefix": k.Prefix, "max_requests": k.MaxRequests, "token_budget": k.TokenBudget, "expires_at": stamp(k.ExpiresAt), "allowed_models": modelList(k), "max_output_tokens": k.MaxOutputTokens, "requests_used": k.RequestsUsed, "tokens_used": k.TokensUsed, "requests_remaining": max(int64(0), k.MaxRequests-k.RequestsUsed), "tokens_remaining": max(int64(0), k.TokenBudget-k.TokensUsed), "estimated_requests": k.EstimatedRequests, "state": state, "in_flight": k.InFlight.Valid, "created_at": stamp(k.CreatedAt), "revision": k.Revision}
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, c connection) {
	rows, err := s.store.db.QueryContext(r.Context(), "SELECT "+keyColumns+" FROM virtual_keys WHERE connection_id=? ORDER BY created_at,id", c.ID)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		k, e := scanKey(rows)
		if e != nil {
			fail(w, e)
			return
		}
		list = append(list, keyView(k, s.now().Unix()))
	}
	if err = rows.Err(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, c connection) {
	var in keyInput
	if err := decodeJSON(w, r, &in, 32<<10); err != nil {
		fail(w, err)
		return
	}
	k := virtualKey{ID: randomToken("key_", 18), ConnectionID: c.ID, MaxOutputTokens: min(int64(4096), s.cfg.MaxOutputTokens), Revision: 1, CreatedAt: s.now().Unix()}
	if err := s.applyKeyInput(&k, in, true); err != nil {
		fail(w, err)
		return
	}
	token := randomToken("tg_", 32)
	k.Hash = tokenHash(token)
	k.Prefix = token[:11]
	tx, err := s.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	var count int
	var revision int64
	if err = tx.QueryRow("SELECT revision FROM connections WHERE id=?", c.ID).Scan(&revision); err != nil || revision != c.Revision {
		fail(w, problem(409, "configuration_changed", "Connection changed; retry with current credentials."))
		return
	}
	if err = tx.QueryRow("SELECT COUNT(*) FROM virtual_keys WHERE connection_id=?", c.ID).Scan(&count); err != nil {
		fail(w, err)
		return
	}
	if count >= s.cfg.MaxKeysPerConnection {
		fail(w, problem(409, "key_capacity_reached", "Virtual key capacity reached for this connection."))
		return
	}
	_, err = tx.Exec("INSERT INTO virtual_keys(id,connection_id,token_hash,prefix,name,max_requests,token_budget,expires_at,allowed_models,max_output_tokens,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", k.ID, c.ID, k.Hash, k.Prefix, k.Name, k.MaxRequests, k.TokenBudget, k.ExpiresAt, k.Models, k.MaxOutputTokens, k.CreatedAt)
	if err != nil {
		fail(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	view := keyView(k, s.now().Unix())
	view["key"] = token
	writeJSON(w, 201, view)
}

func (s *Server) patchKey(w http.ResponseWriter, r *http.Request, c connection) {
	var in keyInput
	if err := decodeJSON(w, r, &in, 32<<10); err != nil {
		fail(w, err)
		return
	}
	k, err := s.store.getKey(r.Context(), r.PathValue("keyId"))
	if err != nil || k.ConnectionID != c.ID {
		fail(w, problem(404, "key_not_found", "Virtual key not found."))
		return
	}
	if k.Revoked {
		fail(w, problem(409, "key_revoked", "Revoked keys cannot be reactivated."))
		return
	}
	if err = s.applyKeyInput(&k, in, false); err != nil {
		fail(w, err)
		return
	}
	res, err := s.store.db.ExecContext(r.Context(), `UPDATE virtual_keys SET name=?,max_requests=?,token_budget=?,expires_at=?,allowed_models=?,max_output_tokens=?,revision=revision+1 WHERE id=? AND revision=? AND revoked=0 AND EXISTS(SELECT 1 FROM connections WHERE id=? AND revision=?)`, k.Name, k.MaxRequests, k.TokenBudget, k.ExpiresAt, k.Models, k.MaxOutputTokens, k.ID, k.Revision, c.ID, c.Revision)
	if err = changed(res, err); err != nil {
		fail(w, err)
		return
	}
	k, err = s.store.getKey(r.Context(), k.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, keyView(k, s.now().Unix()))
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, c connection) {
	res, err := s.store.db.ExecContext(r.Context(), "UPDATE virtual_keys SET revoked=1,revision=revision+1 WHERE id=? AND connection_id=? AND EXISTS(SELECT 1 FROM connections WHERE id=? AND revision=?)", r.PathValue("keyId"), c.ID, c.ID, c.Revision)
	if err = changed(res, err); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func changed(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return problem(409, "configuration_changed", "Resource changed or no longer exists; refresh and retry.")
	}
	return nil
}
