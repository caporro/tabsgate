package gate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Server struct {
	cfg           Config
	store         *store
	vault         *vault
	client        *http.Client
	providers     map[string]Provider
	mux           *http.ServeMux
	argon         chan struct{}
	upstreamSlots chan struct{}
	limiter       *limiter
	ready         atomic.Bool
	now           func() time.Time
}

func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	v, err := newVault(cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	st, err := openStore(cfg.DBPath, v.fingerprint("master-key-check", "v1"))
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, vault: v, providers: map[string]Provider{}, mux: http.NewServeMux(), argon: make(chan struct{}, 2), upstreamSlots: make(chan struct{}, 16), limiter: &limiter{entries: map[string]bucket{}}, now: time.Now}
	for _, p := range cfg.Providers {
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		s.providers[p.ID] = p
	}
	s.client = &http.Client{Transport: upstreamTransport(cfg.AllowPrivateUpstreams), Timeout: cfg.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s.ready.Store(true)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("GET /readyz", s.readiness)
	s.mux.HandleFunc("GET /api/v1/providers", s.listProviders)
	s.mux.HandleFunc("POST /api/v1/connections", s.register)
	s.mux.HandleFunc("GET /api/v1/connections/{id}", s.manage(s.readConnection))
	s.mux.HandleFunc("PATCH /api/v1/connections/{id}", s.manage(s.patchConnection))
	s.mux.HandleFunc("DELETE /api/v1/connections/{id}", s.manage(s.deleteConnection))
	s.mux.HandleFunc("GET /api/v1/connections/{id}/models", s.manage(s.connectionModels))
	s.mux.HandleFunc("GET /api/v1/connections/{id}/keys", s.manage(s.listKeys))
	s.mux.HandleFunc("POST /api/v1/connections/{id}/keys", s.manage(s.createKey))
	s.mux.HandleFunc("PATCH /api/v1/connections/{id}/keys/{keyId}", s.manage(s.patchKey))
	s.mux.HandleFunc("DELETE /api/v1/connections/{id}/keys/{keyId}", s.manage(s.revokeKey))
	s.mux.HandleFunc("GET /api/v1/key", s.keyInfo)
	s.mux.HandleFunc("GET /v1/models", s.virtualModels)
	s.mux.HandleFunc("POST /v1/chat/completions", s.chat)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, problem(404, "not_found", "Endpoint not found."))
	})
	return s, nil
}

func (s *Server) Close() { s.client.CloseIdleConnections(); s.store.close() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Request-ID", randomToken("", 12))
	if origin := r.Header.Get("Origin"); origin != "" {
		allowed := false
		for _, a := range s.cfg.Origins {
			if a == "*" || strings.TrimSpace(a) == origin {
				allowed = true
				if a == "*" {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Add("Vary", "Origin")
				}
				break
			}
		}
		if !allowed {
			fail(w, problem(403, "origin_denied", "Origin is not allowed."))
			return
		}
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-TabsGate-Keyword")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(204)
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, problem(400, "query_not_supported", "Send credentials in headers, never in the URL. Query parameters are not supported."))
		return
	}
	if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
		ip := s.clientIP(r)
		if !s.limiter.allow("ip:"+ip, 5, 60) {
			fail(w, problem(429, "rate_limited", "Too many requests."))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/connections") && (!s.limiter.allow("admin:"+ip, 0.5, 10) || !s.limiter.allow("admin:global", 10, 20)) {
			fail(w, problem(429, "rate_limited", "Too many management requests."))
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if !s.ready.Load() || s.store.db.PingContext(ctx) != nil {
		fail(w, problem(503, "not_ready", "Storage is unavailable."))
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	e := &apiError{500, "internal_error", "The request could not be completed."}
	var known *apiError
	if errors.As(err, &known) {
		e = known
	} else {
		slog.Error("request failed", "category", "internal")
	}
	if e.Status == 429 || e.Code == "key_busy" {
		w.Header().Set("Retry-After", "2")
	}
	writeJSON(w, e.Status, map[string]any{"error": map[string]string{"type": "tabsgate_error", "code": e.Code, "message": e.Message}})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return problem(415, "invalid_content_type", "Content-Type must be application/json.")
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			return problem(413, "body_too_large", "Request body is too large.")
		}
		return problem(400, "invalid_json", "Invalid JSON or unsupported fields.")
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return problem(400, "invalid_json", "Exactly one JSON object is required.")
	}
	return nil
}

func bearer(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func (s *Server) hashSlot() bool {
	select {
	case s.argon <- struct{}{}:
		return true
	default:
		return false
	}
}
func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	list := make([]map[string]any, 0, len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		list = append(list, map[string]any{"id": p.ID, "name": p.Name, "protocol": "openai-chat-completions", "streaming": true, "tool_calling": true, "modalities": []string{"text"}})
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	addr = addr.Unmap()
	trusted := func(a netip.Addr) bool {
		for _, p := range s.cfg.TrustedProxies {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	if !trusted(addr) {
		return addr.String()
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(parts) > 20 {
		return addr.String()
	}
	for i := len(parts) - 1; i >= 0; i-- {
		next, e := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if e != nil {
			return addr.String()
		}
		addr = next.Unmap()
		if !trusted(addr) {
			break
		}
	}
	return addr.String()
}

type bucket struct {
	tokens float64
	at     time.Time
}
type limiter struct {
	mu      sync.Mutex
	entries map[string]bucket
}

func (l *limiter) allow(key string, rate, burst float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= 10000 {
			for k, v := range l.entries {
				if now.Sub(v.at) > 5*time.Minute {
					delete(l.entries, k)
				}
			}
			if len(l.entries) >= 10000 {
				return false
			}
		}
		b = bucket{tokens: burst, at: now}
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
	b.at = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	l.entries[key] = b
	return allowed
}
