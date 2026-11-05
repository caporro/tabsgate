package gate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual built-in profiles through registration, model discovery,
// non-streaming and SSE. All upstreams are replaced by local protocol fixtures.
func TestAdditionalProviderProfiles(t *testing.T) {
	for _, id := range []string{"groq", "mistral", "together", "xai"} {
		t.Run(id, func(t *testing.T) {
			cfg := DefaultConfig()
			var profile Provider
			for _, candidate := range cfg.Providers {
				if candidate.ID == id {
					profile = candidate
				}
			}
			if profile.ID == "" {
				t.Fatal("built-in provider missing")
			}
			endpoint, err := url.Parse(profile.BaseURL)
			if err != nil || endpoint.Scheme != "https" {
				t.Fatal("invalid public endpoint")
			}
			prefix := strings.TrimRight(endpoint.Path, "/")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+originalKey || r.Header.Get("X-TabsGate-Keyword") != "" {
					t.Error("upstream credentials not isolated")
				}
				if r.Method == "GET" && r.URL.Path == prefix+"/models" {
					writeJSON(w, 200, map[string]any{"data": []map[string]string{{"id": "test-model"}}})
					return
				}
				if r.Method != "POST" || r.URL.Path != prefix+"/chat/completions" {
					t.Error("wrong upstream endpoint")
					w.WriteHeader(404)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if body[profile.OutputLimitField] != float64(100) {
					t.Error("output cap missing")
				}
				other := "max_tokens"
				if profile.OutputLimitField == other {
					other = "max_completion_tokens"
				}
				if _, exists := body[other]; exists {
					t.Error("unwanted output-limit alias")
				}
				if body["stream"] == true {
					if profile.OmitStreamOptions {
						if _, exists := body["stream_options"]; exists {
							t.Error("unsupported stream options forwarded")
						}
					} else if options, ok := body["stream_options"].(map[string]any); !ok || options["include_usage"] != true {
						t.Error("stream usage not requested")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
					return
				}
				writeJSON(w, 200, map[string]any{"choices": []any{}, "usage": map[string]int{"total_tokens": 11}})
			}))
			cfg.DBPath = filepath.Join(t.TempDir(), "provider.db")
			cfg.MasterKey = bytes.Repeat([]byte{9}, 32)
			cfg.AllowPrivateUpstreams = true
			profile.BaseURL = upstream.URL + prefix
			cfg.Providers = []Provider{profile}
			server, err := New(cfg)
			if err != nil {
				upstream.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() { server.Close(); upstream.Close() })
			fixture := &fixture{t: t, s: server, cfg: cfg, upstream: upstream}
			registered := fixture.request("POST", "/api/v1/connections", "", false, map[string]string{"provider": id, "api_key": originalKey, "keyword": keyword})
			checkStatus(t, registered, 201)
			fixture.id = decode(t, registered)["id"].(string)
			fixture.create(10, 1000)
			checkStatus(t, fixture.request("GET", "/api/v1/connections/"+fixture.id+"/models", "", true, nil), 200)
			checkStatus(t, fixture.chat(), 200)
			stream := fixture.request("POST", "/v1/chat/completions", fixture.token, false, chatBody(true))
			checkStatus(t, stream, 200)
			if !strings.Contains(stream.Body.String(), "[DONE]") {
				t.Fatal("stream not completed")
			}
			usage := fixture.key()
			if usage["requests_used"] != float64(2) || usage["tokens_used"] != float64(22) || usage["estimated_requests"] != float64(0) {
				t.Fatal("incorrect provider accounting", usage)
			}
		})
	}
}
