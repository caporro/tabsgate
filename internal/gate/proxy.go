package gate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const responseLimit = 4 << 20

func upstreamTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil, ForceAttemptHTTP2: true, MaxIdleConns: 64, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 64,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			var last error
			for _, ip := range ips {
				if !allowPrivate && !publicIP(ip.Unmap()) {
					last = errors.New("private upstream address denied")
					continue
				}
				conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if e == nil {
					return conn, nil
				}
				last = e
			}
			if last == nil {
				last = errors.New("upstream has no usable address")
			}
			return nil, last
		},
	}
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}

func (s *Server) virtualAuth(r *http.Request) (virtualKey, connection, error) {
	var k virtualKey
	var c connection
	token := bearer(r)
	if !strings.HasPrefix(token, "tg_") || len(token) != 46 {
		return k, c, problem(401, "invalid_key", "Invalid virtual key.")
	}
	var err error
	k, err = scanKey(s.store.db.QueryRowContext(r.Context(), "SELECT "+keyColumns+" FROM virtual_keys WHERE token_hash=?", tokenHash(token)))
	if err != nil {
		return k, c, problem(401, "invalid_key", "Invalid virtual key.")
	}
	if !s.limiter.allow("key:"+k.ID, 2, 30) {
		return k, c, problem(429, "rate_limited", "Too many requests for this key.")
	}
	c, err = s.store.getConnection(r.Context(), k.ConnectionID)
	if err != nil {
		return k, c, problem(401, "invalid_key", "Invalid virtual key.")
	}
	return k, c, nil
}

func (s *Server) keyInfo(w http.ResponseWriter, r *http.Request) {
	k, _, err := s.virtualAuth(r)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, keyView(k, s.now().Unix()))
}

func (s *Server) connectionModels(w http.ResponseWriter, r *http.Request, c connection) {
	s.models(w, r, c, nil)
}
func (s *Server) virtualModels(w http.ResponseWriter, r *http.Request) {
	k, c, err := s.virtualAuth(r)
	if err != nil {
		fail(w, err)
		return
	}
	if err = keyBlock(k, s.now().Unix()); err != nil {
		fail(w, err)
		return
	}
	s.models(w, r, c, &k)
}

func (s *Server) upstreamRequest(ctx context.Context, c connection, method, path string, body []byte) (*http.Request, error) {
	p, ok := s.providers[c.Provider]
	if !ok {
		return nil, problem(503, "provider_unavailable", "Provider is no longer configured.")
	}
	key, err := s.vault.decrypt(c)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "TabsGate/0.1")
	return req, nil
}

func (s *Server) upstreamSlot() bool {
	select {
	case s.upstreamSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) models(w http.ResponseWriter, r *http.Request, c connection, k *virtualKey) {
	if !s.limiter.allow("models:"+c.ID, 0.2, 5) {
		fail(w, problem(429, "rate_limited", "Model discovery is rate limited."))
		return
	}
	if !s.upstreamSlot() {
		fail(w, problem(503, "busy", "Gateway capacity reached."))
		return
	}
	defer func() { <-s.upstreamSlots }()
	req, err := s.upstreamRequest(r.Context(), c, http.MethodGet, "/models", nil)
	if err != nil {
		fail(w, err)
		return
	}
	res, err := s.client.Do(req)
	if err != nil {
		fail(w, problem(502, "upstream_unavailable", "Unable to reach the provider."))
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		fail(w, problem(502, "upstream_error", "Provider rejected model discovery."))
		return
	}
	body, err := readBounded(res.Body, 4<<20)
	if err != nil {
		fail(w, problem(502, "invalid_upstream_response", "Invalid provider model list."))
		return
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil || list.Data == nil {
		fail(w, problem(502, "invalid_upstream_response", "Invalid provider model list."))
		return
	}
	items := []map[string]any{}
	for _, m := range list.Data {
		if m.ID != "" && (k == nil || allowedModel(*k, m.ID)) {
			items = append(items, map[string]any{"id": m.ID, "object": "model", "owned_by": c.Provider})
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": items})
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("upstream response too large")
	}
	return b, nil
}

var chatFields = map[string]bool{}

func init() {
	for _, f := range strings.Fields("model messages stream stream_options max_tokens max_completion_tokens n tools tool_choice parallel_tool_calls temperature top_p top_k min_p frequency_penalty presence_penalty stop seed response_format logit_bias logprobs top_logprobs reasoning_effort reasoning verbosity include_reasoning user store") {
		chatFields[f] = true
	}
}

func prepareChat(body map[string]json.RawMessage, k virtualKey, p Provider) (out []byte, model string, stream bool, fallback int64, err error) {
	bad := func(message string) error { return problem(400, "invalid_request", message) }
	for name := range body {
		if !chatFields[name] {
			return nil, "", false, 0, bad("Unsupported field: " + name)
		}
	}
	if json.Unmarshal(body["model"], &model) != nil || !allowedModel(k, model) {
		return nil, "", false, 0, problem(403, "model_not_allowed", "This model is not allowed by the virtual key.")
	}
	if raw, ok := body["stream"]; ok && (string(raw) == "null" || json.Unmarshal(raw, &stream) != nil) {
		return nil, "", false, 0, bad("stream must be boolean.")
	}
	if raw, ok := body["n"]; ok {
		var n int
		if json.Unmarshal(raw, &n) != nil || n != 1 {
			return nil, "", false, 0, bad("Only n=1 is supported.")
		}
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 || len(messages) > 1000 {
		return nil, "", false, 0, bad("messages must contain 1 to 1000 text messages.")
	}
	for _, m := range messages {
		for f := range m {
			if !strings.Contains(" role content name tool_calls tool_call_id reasoning_content ", " "+f+" ") {
				return nil, "", false, 0, bad("Unsupported message field.")
			}
		}
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if !strings.Contains(" system developer user assistant tool ", " "+role+" ") || role == "" {
			return nil, "", false, 0, bad("Unsupported message role.")
		}
		raw := m["content"]
		if len(raw) == 0 || string(raw) == "null" {
			if role != "assistant" {
				return nil, "", false, 0, bad("Only assistant tool-call messages may omit content.")
			}
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			continue
		}
		var parts []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&parts) != nil || len(parts) == 0 {
			return nil, "", false, 0, bad("Only text message content is supported.")
		}
		for _, part := range parts {
			if part.Type != "text" || part.Text == nil {
				return nil, "", false, 0, bad("Only text message content is supported.")
			}
		}
	}
	if raw, ok := body["tools"]; ok {
		var tools []struct {
			Type     string          `json:"type"`
			Function json.RawMessage `json:"function"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&tools) != nil || len(tools) > 100 {
			return nil, "", false, 0, bad("Invalid tools list.")
		}
		for _, tool := range tools {
			if tool.Type != "function" || len(tool.Function) == 0 {
				return nil, "", false, 0, bad("Only client-executed function tools are supported.")
			}
		}
	}
	limit := k.MaxOutputTokens
	for _, f := range []string{"max_tokens", "max_completion_tokens"} {
		if raw, ok := body[f]; ok {
			var requested int64
			if json.Unmarshal(raw, &requested) != nil || requested < 1 {
				return nil, "", false, 0, bad("Output token limits must be positive integers.")
			}
			limit = min(limit, requested)
		}
		delete(body, f)
	}
	body[p.OutputLimitField] = json.RawMessage(strconv.FormatInt(limit, 10))
	// Never allow a client to opt into provider-side response storage.
	if _, ok := body["store"]; ok {
		if p.ID == "openai" {
			body["store"] = json.RawMessage("false")
		} else {
			delete(body, "store")
		}
	}
	if stream && !p.OmitStreamOptions {
		body["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		delete(body, "stream_options")
	}
	out, err = json.Marshal(body)
	// This is intentionally conservative, not a tokenizer: on unknown usage charge
	// one token per UTF-8 payload byte plus the permitted output (including reasoning).
	fallback = int64(len(out)) + limit
	return
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		fail(w, problem(503, "not_ready", "Accounting is unavailable."))
		return
	}
	k, c, err := s.virtualAuth(r)
	if err != nil {
		fail(w, err)
		return
	}
	if err = keyBlock(k, s.now().Unix()); err != nil {
		fail(w, err)
		return
	}
	p, ok := s.providers[c.Provider]
	if !ok {
		fail(w, problem(503, "provider_unavailable", "Provider is no longer configured."))
		return
	}
	var body map[string]json.RawMessage
	if err = decodeJSON(w, r, &body, s.cfg.MaxBodyBytes); err != nil {
		fail(w, err)
		return
	}
	out, model, stream, fallback, err := prepareChat(body, k, p)
	if err != nil {
		fail(w, err)
		return
	}
	if !s.upstreamSlot() {
		fail(w, problem(503, "busy", "Gateway capacity reached."))
		return
	}
	defer func() { <-s.upstreamSlots }()
	req, err := s.upstreamRequest(r.Context(), c, http.MethodPost, "/chat/completions", out)
	if err != nil {
		fail(w, err)
		return
	}
	id, err := s.store.admit(r.Context(), c, k, model, fallback, s.now().Unix())
	if err != nil {
		fail(w, err)
		return
	}
	settled := false
	finish := func(tokens int64, estimated bool, state string) bool {
		if settled {
			return true
		}
		if e := s.store.settle(id, tokens, estimated, state); e != nil {
			s.ready.Store(false)
			slog.Error("accounting unavailable", "request_id", id)
			return false
		}
		settled = true
		return true
	}
	defer func() {
		if !settled {
			finish(fallback, true, "interrupted")
		}
	}()
	res, err := s.client.Do(req)
	if err != nil {
		finish(fallback, true, "upstream_error")
		fail(w, problem(502, "upstream_unavailable", "Provider request failed; the attempt was counted."))
		return
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// An explicit HTTP rejection consumed an attempt, but no generation tokens.
		if !finish(0, false, "rejected") {
			fail(w, problem(503, "accounting_unavailable", "Unable to record usage."))
			return
		}
		fail(w, problem(502, "upstream_error", "Provider rejected the request (HTTP "+strconv.Itoa(res.StatusCode)+")."))
		return
	}
	if stream {
		media, _, e := mime.ParseMediaType(res.Header.Get("Content-Type"))
		if e != nil || media != "text/event-stream" {
			finish(fallback, true, "invalid_response")
			fail(w, problem(502, "invalid_upstream_response", "Expected an SSE response."))
			return
		}
		s.stream(w, res.Body, fallback, finish)
		return
	}
	data, err := readBounded(res.Body, responseLimit)
	if err != nil || !json.Valid(data) {
		finish(fallback, true, "invalid_response")
		fail(w, problem(502, "invalid_upstream_response", "Invalid or oversized provider response."))
		return
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil || envelope["error"] != nil || envelope["choices"] == nil {
		finish(fallback, true, "upstream_error")
		fail(w, problem(502, "invalid_upstream_response", "Provider did not return a chat completion."))
		return
	}
	tokens, ok := usage(data)
	if !ok {
		tokens = fallback
	}
	if !finish(tokens, !ok, "completed") {
		fail(w, problem(503, "accounting_unavailable", "Unable to record usage."))
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func usage(data []byte) (int64, bool) {
	var envelope struct {
		Usage *struct {
			Total      *int64 `json:"total_tokens"`
			Prompt     *int64 `json:"prompt_tokens"`
			Completion *int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Usage == nil {
		return 0, false
	}
	u := envelope.Usage
	for _, n := range []*int64{u.Total, u.Prompt, u.Completion} {
		if n != nil && (*n < 0 || *n > 1_000_000_000_000) {
			return 0, false
		}
	}
	if u.Total != nil && *u.Total > 0 {
		return *u.Total, true
	}
	if u.Prompt != nil && u.Completion != nil && *u.Prompt+*u.Completion > 0 && *u.Prompt+*u.Completion <= 1_000_000_000_000 {
		return *u.Prompt + *u.Completion, true
	}
	return 0, false
}

func (s *Server) stream(w http.ResponseWriter, body io.Reader, fallback int64, finish func(int64, bool, string) bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	controller := http.NewResponseController(w)
	send := func(b []byte) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, e := w.Write(b); e != nil {
			return false
		}
		return controller.Flush() == nil
	}
	scanner := bufio.NewScanner(io.LimitReader(body, responseLimit+1))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var event bytes.Buffer
	var dataLines []string
	total := 0
	tokens := int64(0)
	measured := false
	hadError := false
	process := func() (done, ok bool) {
		data := strings.Join(dataLines, "\n")
		if strings.TrimSpace(data) == "[DONE]" {
			if !measured || hadError {
				tokens = max(tokens, fallback)
			}
			state := "completed"
			if hadError {
				state = "upstream_error"
			}
			if !finish(tokens, !measured || hadError, state) {
				send([]byte("data: {\"error\":{\"code\":\"accounting_unavailable\",\"message\":\"Usage accounting failed.\"}}\n\n"))
				return true, false
			}
			return true, send(event.Bytes())
		}
		if data != "" {
			var e struct {
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal([]byte(data), &e) != nil {
				return false, false
			}
			if len(e.Error) > 0 && string(e.Error) != "null" {
				hadError = true
				// Provider error text may contain credentials or internal details.
				event.Reset()
				event.WriteString("data: {\"error\":{\"code\":\"upstream_error\",\"message\":\"Provider reported a streaming error.\"}}\n\n")
			}
			if n, ok := usage([]byte(data)); ok {
				tokens = max(tokens, n)
				measured = true
			}
		}
		return false, send(event.Bytes())
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > responseLimit || event.Len()+len(line) > 1<<20 {
			break
		}
		event.WriteString(line)
		event.WriteByte('\n')
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if line == "" {
			done, ok := process()
			if done {
				return
			}
			if !ok {
				break
			}
			event.Reset()
			dataLines = nil
		}
	}
	// EOF, timeout, malformed SSE, a size limit, or a disconnected client cannot
	// release the budget for free, even if no visible output was received.
	finish(max(tokens, fallback), true, "interrupted")
	send([]byte("data: {\"error\":{\"code\":\"upstream_stream_interrupted\",\"message\":\"Stream interrupted; estimated usage was charged.\"}}\n\ndata: [DONE]\n\n"))
}
