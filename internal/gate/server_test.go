package gate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	t                *testing.T
	s                *Server
	cfg              Config
	upstream         *httptest.Server
	id, token, keyID string
}

const originalKey = "sk-test-upstream-secret-never-return"
const keyword = "test-word-never-return"

func newFixture(t *testing.T, handler http.HandlerFunc) *fixture {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				writeJSON(w, 200, map[string]any{"data": []map[string]string{{"id": "test-model"}, {"id": "other-model"}}})
				return
			}
			writeJSON(w, 200, map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 40, "completion_tokens": 35, "total_tokens": 75}})
		}
	}
	up := httptest.NewServer(handler)
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "gate.db")
	cfg.MasterKey = bytes.Repeat([]byte{7}, 32)
	cfg.AllowPrivateUpstreams = true
	cfg.Providers = []Provider{{ID: "test", Name: "Test", BaseURL: up.URL + "/v1", OutputLimitField: "max_completion_tokens"}}
	s, err := New(cfg)
	if err != nil {
		up.Close()
		t.Fatal(err)
	}
	f := &fixture{t: t, s: s, cfg: cfg, upstream: up}
	t.Cleanup(func() { f.s.Close(); up.Close() })
	res := f.request("POST", "/api/v1/connections", "", false, map[string]any{"provider": "test", "api_key": originalKey, "keyword": keyword})
	checkStatus(t, res, 201)
	f.id = decode(t, res)["id"].(string)
	return f
}

func (f *fixture) request(method, path, token string, admin bool, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if admin {
		req.Header.Set("Authorization", "Bearer "+originalKey)
		req.Header.Set("X-TabsGate-Keyword", keyword)
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	return rec
}
func checkStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status %d, want %d: %s", r.Code, want, r.Body.String())
	}
}
func decode(t *testing.T, r *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func errorCode(t *testing.T, r *httptest.ResponseRecorder) string {
	t.Helper()
	return decode(t, r)["error"].(map[string]any)["code"].(string)
}
func (f *fixture) create(requests, budget int64) {
	f.t.Helper()
	res := f.request("POST", "/api/v1/connections/"+f.id+"/keys", "", true, map[string]any{"name": "Interview", "max_requests": requests, "token_budget": budget, "ttl_seconds": 3600, "allowed_models": []string{"test-model"}, "max_output_tokens": 100})
	checkStatus(f.t, res, 201)
	v := decode(f.t, res)
	f.token = v["key"].(string)
	f.keyID = v["id"].(string)
}
func chatBody(stream bool) map[string]any {
	return map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "Hello"}}, "stream": stream}
}
func (f *fixture) chat() *httptest.ResponseRecorder {
	return f.request("POST", "/v1/chat/completions", f.token, false, chatBody(false))
}
func (f *fixture) key() map[string]any {
	r := f.request("GET", "/api/v1/key", f.token, false, nil)
	checkStatus(f.t, r, 200)
	return decode(f.t, r)
}

func TestRegistrationAndCredentialSeparation(t *testing.T) {
	f := newFixture(t, nil)
	f.create(30, 1_000_000)
	r := f.request("POST", "/api/v1/connections", "", false, map[string]string{"provider": "test", "api_key": originalKey, "keyword": "wrong"})
	checkStatus(t, r, 401)
	r = f.request("POST", "/api/v1/connections", "", false, map[string]string{"provider": "test", "api_key": originalKey, "keyword": keyword})
	checkStatus(t, r, 200)
	if decode(t, r)["id"] != f.id {
		t.Fatal("connection duplicated")
	}
	r = f.request("GET", "/api/v1/connections/"+f.id, f.token, false, nil)
	checkStatus(t, r, 401)
	r = f.request("GET", "/api/v1/connections/"+f.id+"/keys", "", true, nil)
	checkStatus(t, r, 200)
	for _, secret := range []string{originalKey, keyword, f.token} {
		if strings.Contains(r.Body.String(), secret) {
			t.Fatal("secret in response")
		}
	}
	var encrypted, salt, hash []byte
	if err := f.s.store.db.QueryRow("SELECT secret,word_salt,word_hash FROM connections").Scan(&encrypted, &salt, &hash); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(originalKey)) || bytes.Contains(hash, []byte(keyword)) {
		t.Fatal("plaintext credentials stored")
	}
	var stored string
	_ = f.s.store.db.QueryRow("SELECT token_hash FROM virtual_keys").Scan(&stored)
	if stored == f.token || !equal(stored, tokenHash(f.token)) {
		t.Fatal("invalid key storage")
	}
	for _, p := range []string{f.cfg.DBPath, f.cfg.DBPath + "-wal"} {
		b, e := os.ReadFile(p)
		if e != nil {
			continue
		}
		for _, secret := range []string{originalKey, keyword, f.token} {
			if bytes.Contains(b, []byte(secret)) {
				t.Fatalf("plaintext secret in %s", p)
			}
		}
	}
}

func TestSoftBudgetAndRequestLimit(t *testing.T) {
	t.Run("token threshold permits last response", func(t *testing.T) {
		f := newFixture(t, nil)
		f.create(30, 100)
		checkStatus(t, f.chat(), 200)
		checkStatus(t, f.chat(), 200)
		if v := f.key(); v["tokens_used"] != float64(150) || v["requests_used"] != float64(2) || v["state"] != "token_budget_exhausted" {
			t.Fatal(v)
		}
		r := f.chat()
		checkStatus(t, r, 403)
		if errorCode(t, r) != "token_budget_exhausted" {
			t.Fatal(r.Body.String())
		}
		if f.key()["requests_used"] != float64(2) {
			t.Fatal("rejected request charged")
		}
	})
	t.Run("request cap and increasing limits preserves usage", func(t *testing.T) {
		f := newFixture(t, nil)
		f.create(1, 1_000_000)
		checkStatus(t, f.chat(), 200)
		r := f.chat()
		checkStatus(t, r, 403)
		if errorCode(t, r) != "request_limit_reached" {
			t.Fatal(r.Body.String())
		}
		r = f.request("PATCH", "/api/v1/connections/"+f.id+"/keys/"+f.keyID, "", true, map[string]any{"max_requests": 2})
		checkStatus(t, r, 200)
		if decode(t, r)["requests_used"] != float64(1) {
			t.Fatal("patch reset usage")
		}
		checkStatus(t, f.chat(), 200)
		checkStatus(t, f.chat(), 403)
	})
}

func TestProxyReplacesCredentialsAndEnforcesOutput(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+originalKey || r.Header.Get("X-TabsGate-Keyword") != "" || r.Header.Get("X-Client-Secret") != "" {
			t.Error("wrong upstream headers")
		}
		if r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "" {
			t.Error("wrong upstream path")
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["max_completion_tokens"] != float64(100) || b["max_tokens"] != nil {
			t.Errorf("output limit not imposed: %v", b)
		}
		writeJSON(w, 200, map[string]any{"choices": []any{}, "usage": map[string]int{"total_tokens": 30}})
	})
	f.create(30, 1_000_000)
	b := chatBody(false)
	b["max_tokens"] = 999999
	raw, _ := json.Marshal(b)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-TabsGate-Keyword", "leak")
	req.Header.Set("X-Client-Secret", "leak")
	r := httptest.NewRecorder()
	f.s.ServeHTTP(r, req)
	checkStatus(t, r, 200)
	if calls.Load() != 1 {
		t.Fatal("unexpected retry")
	}
}

func TestRejectedPayloadsNeverReachProvider(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	f.create(100, 1_000_000)
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"model", func(b map[string]any) { b["model"] = "other-model" }},
		{"fallback models", func(b map[string]any) { b["models"] = []string{"other-model"} }},
		{"override URL", func(b map[string]any) { b["base_url"] = "http://127.0.0.1" }},
		{"multiple choices", func(b map[string]any) { b["n"] = 2 }},
		{"negative output", func(b map[string]any) { b["max_tokens"] = -1 }},
		{"image", func(b map[string]any) {
			b["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]string{"url": "https://example.com/image.png"}}}}}
		}},
		{"server tool", func(b map[string]any) { b["tools"] = []any{map[string]string{"type": "web_search"}} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := chatBody(false)
			tc.change(b)
			r := f.request("POST", "/v1/chat/completions", f.token, false, b)
			if r.Code != 400 && r.Code != 403 {
				t.Fatal(r.Code, r.Body.String())
			}
		})
	}
	if calls.Load() != 0 || f.key()["requests_used"] != float64(0) {
		t.Fatal("invalid calls were charged or forwarded")
	}
}

func TestPerKeyConcurrency(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		writeJSON(w, 200, map[string]any{"choices": []any{}, "usage": map[string]int{"total_tokens": 5}})
	})
	f.create(30, 1000)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- f.chat() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider not called")
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.chat()
			if r.Code != 409 || errorCode(t, r) != "key_busy" {
				t.Error(r.Code, r.Body.String())
			}
		}()
	}
	wg.Wait()
	close(release)
	checkStatus(t, <-first, 200)
	if v := f.key(); v["requests_used"] != float64(1) || v["in_flight"] != false {
		t.Fatal(v)
	}
}

func TestTTLRevocationAndDeletion(t *testing.T) {
	t.Run("TTL", func(t *testing.T) {
		f := newFixture(t, nil)
		f.create(30, 1000)
		future := time.Now().Add(2 * time.Hour)
		f.s.now = func() time.Time { return future }
		r := f.chat()
		checkStatus(t, r, 403)
		if errorCode(t, r) != "key_expired" || f.key()["requests_used"] != float64(0) {
			t.Fatal(r.Body.String())
		}
	})
	t.Run("revocation", func(t *testing.T) {
		f := newFixture(t, nil)
		f.create(30, 1000)
		checkStatus(t, f.request("DELETE", "/api/v1/connections/"+f.id+"/keys/"+f.keyID, "", true, nil), 204)
		checkStatus(t, f.chat(), 403)
		if f.key()["state"] != "key_revoked" {
			t.Fatal("revocation missing")
		}
		checkStatus(t, f.request("PATCH", "/api/v1/connections/"+f.id+"/keys/"+f.keyID, "", true, map[string]int{"max_requests": 50}), 409)
	})
	t.Run("connection deletion", func(t *testing.T) {
		f := newFixture(t, nil)
		f.create(30, 1000)
		checkStatus(t, f.chat(), 200)
		checkStatus(t, f.request("DELETE", "/api/v1/connections/"+f.id, "", true, nil), 204)
		checkStatus(t, f.chat(), 401)
		for _, table := range []string{"connections", "virtual_keys", "requests"} {
			var n int
			if err := f.s.store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
				t.Fatal(table, n, err)
			}
		}
	})
}

func TestConnectionRotationAndOwnership(t *testing.T) {
	f := newFixture(t, nil)
	f.create(30, 1000)
	other := f.request("POST", "/api/v1/connections", "", false, map[string]string{"provider": "test", "api_key": "another-secret", "keyword": keyword})
	checkStatus(t, other, 201)
	otherID := decode(t, other)["id"].(string)
	// Valid credentials for one connection do not authorize another connection.
	checkStatus(t, f.request("PATCH", "/api/v1/connections/"+otherID, "", true, map[string]string{"keyword": "hijack"}), 401)
	// A key ID from another connection is never enough for an edit.
	otherReq := httptest.NewRequest("PATCH", "/api/v1/connections/"+otherID+"/keys/"+f.keyID, strings.NewReader(`{"max_requests":999}`))
	otherReq.Header.Set("Content-Type", "application/json")
	otherReq.Header.Set("Authorization", "Bearer another-secret")
	otherReq.Header.Set("X-TabsGate-Keyword", keyword)
	otherRec := httptest.NewRecorder()
	f.s.ServeHTTP(otherRec, otherReq)
	checkStatus(t, otherRec, 404)
	checkStatus(t, f.request("PATCH", "/api/v1/connections/"+f.id, "", true, map[string]string{"api_key": "new-secret", "keyword": "new-word"}), 200)
	checkStatus(t, f.request("GET", "/api/v1/connections/"+f.id, "", true, nil), 401)
	req := httptest.NewRequest("GET", "/api/v1/connections/"+f.id, nil)
	req.Header.Set("Authorization", "Bearer new-secret")
	req.Header.Set("X-TabsGate-Keyword", "new-word")
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	checkStatus(t, rec, 200)
	checkStatus(t, f.chat(), 200)
}

func TestStreamingPassThroughAndUsageBeforeDone(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["stream_options"].(map[string]any)["include_usage"] != true {
			t.Error("usage not requested")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
	})
	f.create(30, 1000)
	server := httptest.NewServer(f.s)
	defer server.Close()
	b, _ := json.Marshal(chatBody(true))
	req, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, "hello") {
		close(release)
		t.Fatal("first event was not streamed", first, err)
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || !bytes.Contains(rest, []byte("[DONE]")) {
		t.Fatal(string(rest), err)
	}
	v := f.key()
	if v["tokens_used"] != float64(12) || v["estimated_requests"] != float64(0) || v["in_flight"] != false {
		t.Fatal(v)
	}
}

func TestInterruptedOrMissingUsageIsCharged(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[]}`)
				}
			})
			f.create(30, 1000)
			r := f.request("POST", "/v1/chat/completions", f.token, false, chatBody(stream))
			checkStatus(t, r, 200)
			v := f.key()
			if v["estimated_requests"] != float64(1) || v["tokens_used"].(float64) <= 100 || v["in_flight"] != false {
				t.Fatal(v)
			}
		})
	}
}

func TestCancellationSettlesAndReleasesSlot(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	f.create(30, 1000)
	server := httptest.NewServer(f.s)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, _ := json.Marshal(chatBody(true))
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bufio.NewReader(res.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	res.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		k, err := f.s.store.getKey(context.Background(), f.keyID)
		if err != nil {
			t.Fatal(err)
		}
		if !k.InFlight.Valid {
			if k.EstimatedRequests != 1 || k.TokensUsed <= 100 {
				t.Fatal(k)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot not settled after cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPersistenceRecoveryAndSingleProcess(t *testing.T) {
	f := newFixture(t, nil)
	f.create(30, 1000)
	if second, err := New(f.cfg); err == nil {
		second.Close()
		t.Fatal("second process admitted")
	}
	c, _ := f.s.store.getConnection(context.Background(), f.id)
	k, _ := f.s.store.getKey(context.Background(), f.keyID)
	id, err := f.s.store.admit(context.Background(), c, k, "test-model", 250, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	f.s, err = New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	v := f.key()
	if v["requests_used"] != float64(1) || v["tokens_used"] != float64(250) || v["estimated_requests"] != float64(1) || v["in_flight"] != false {
		t.Fatal(v)
	}
	if err = f.s.store.settle(id, 10, false, "completed"); err != nil {
		t.Fatal(err)
	}
	if f.key()["tokens_used"] != float64(250) {
		t.Fatal("duplicate settlement")
	}
	checkStatus(t, f.chat(), 200)
	f.s.Close()
	bad := f.cfg
	bad.MasterKey = bytes.Repeat([]byte{8}, 32)
	if s, err := New(bad); err == nil {
		s.Close()
		t.Fatal("wrong master key accepted")
	}
	f.s, err = New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if f.key()["requests_used"] != float64(2) {
		t.Fatal("restart lost usage")
	}
}

func TestModelsErrorsAndRedirects(t *testing.T) {
	t.Run("model filter", func(t *testing.T) {
		f := newFixture(t, nil)
		f.create(30, 1000)
		r := f.request("GET", "/v1/models", f.token, false, nil)
		checkStatus(t, r, 200)
		if strings.Contains(r.Body.String(), "other-model") {
			t.Fatal("unallowed model listed")
		}
		if f.key()["requests_used"] != float64(0) {
			t.Fatal("models charged")
		}
		checkStatus(t, f.request("GET", "/api/v1/connections/"+f.id+"/models", "", true, nil), 200)
	})
	t.Run("provider errors redacted", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); fmt.Fprint(w, originalKey) })
		f.create(30, 1000)
		r := f.chat()
		checkStatus(t, r, 502)
		if strings.Contains(r.Body.String(), originalKey) {
			t.Fatal("provider key leaked")
		}
		v := f.key()
		if v["requests_used"] != float64(1) || v["tokens_used"] != float64(0) {
			t.Fatal(v)
		}
	})
	t.Run("redirects not followed", func(t *testing.T) {
		var leaks atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks.Add(1) }))
		defer target.Close()
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) })
		f.create(30, 1000)
		checkStatus(t, f.chat(), 502)
		if leaks.Load() != 0 {
			t.Fatal("followed redirect")
		}
	})
}

func TestHTTPBoundaries(t *testing.T) {
	f := newFixture(t, nil)
	f.create(30, 1000)
	for _, path := range []string{"/healthz", "/readyz", "/api/v1/providers"} {
		checkStatus(t, f.request("GET", path, "", false, nil), 200)
	}
	checkStatus(t, f.request("GET", "/api/v1/key?key="+f.token, f.token, false, nil), 400)
	checkStatus(t, f.request("GET", "/v1/anything", f.token, false, nil), 404)
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "null")
	r := httptest.NewRecorder()
	f.s.ServeHTTP(r, req)
	checkStatus(t, r, 204)
	if r.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("standalone CORS missing")
	}
	f.s.cfg.Origins = []string{"https://tabs.chat"}
	req = httptest.NewRequest("GET", "/api/v1/providers", nil)
	req.Header.Set("Origin", "https://evil.example")
	r = httptest.NewRecorder()
	f.s.ServeHTTP(r, req)
	checkStatus(t, r, 403)
	b := chatBody(false)
	b["messages"] = []any{map[string]string{"role": "user", "content": strings.Repeat("x", int(f.cfg.MaxBodyBytes))}}
	checkStatus(t, f.request("POST", "/v1/chat/completions", f.token, false, b), 413)
}

func TestIPAndRateLimits(t *testing.T) {
	f := newFixture(t, nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if f.s.clientIP(req) != "203.0.113.10" {
		t.Fatal("trusted spoofed forwarding header")
	}
	f.s.cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	req.RemoteAddr = "127.0.0.1:80"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.10")
	if f.s.clientIP(req) != "203.0.113.10" {
		t.Fatal("did not use rightmost untrusted hop")
	}
	l := &limiter{entries: map[string]bucket{}}
	if !l.allow("test", 0, 2) || !l.allow("test", 0, 2) || l.allow("test", 0, 2) {
		t.Fatal("limiter failed")
	}
	for _, ip := range []string{"127.0.0.1", "::1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "::ffff:127.0.0.1"} {
		if publicIP(netip.MustParseAddr(ip).Unmap()) {
			t.Fatal("private address accepted", ip)
		}
	}
	if !publicIP(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public address rejected")
	}
}

func TestAccountingFailureStopsNewRequests(t *testing.T) {
	var server *Server
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		server.store.db.Close()
		fmt.Fprint(w, `{"choices":[],"usage":{"total_tokens":75}}`)
	})
	server = f.s
	f.create(30, 1000)
	checkStatus(t, f.chat(), 503)
	if f.s.ready.Load() {
		t.Fatal("failed accounting left service ready")
	}
	checkStatus(t, f.chat(), 503)
	checkStatus(t, f.request("GET", "/readyz", "", false, nil), 503)
	f.s.Close()
	var err error
	f.s, err = New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v := f.key(); v["requests_used"] != float64(1) || v["estimated_requests"] != float64(1) || v["tokens_used"].(float64) <= 100 {
		t.Fatal(v)
	}
}
