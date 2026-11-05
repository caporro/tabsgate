package gate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Provider struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	BaseURL          string `json:"base_url"`
	OutputLimitField string `json:"output_limit_field"`
	// Some compatible providers return stream usage automatically and do not
	// accept OpenAI's stream_options parameter.
	OmitStreamOptions bool `json:"omit_stream_options,omitempty"`
}

type Config struct {
	Addr, DBPath                         string
	MasterKey                            []byte
	Providers                            []Provider
	Origins                              []string
	TrustedProxies                       []netip.Prefix
	AllowPrivateUpstreams                bool
	RequestTimeout                       time.Duration
	MaxBodyBytes                         int64
	MaxOutputTokens                      int64
	MaxConnections, MaxKeysPerConnection int
}

func DefaultConfig() Config {
	return Config{Addr: ":8080", DBPath: "data/tabsgate.db", Origins: []string{"*"},
		RequestTimeout: 2 * time.Minute, MaxBodyBytes: 1 << 20, MaxOutputTokens: 16384,
		MaxConnections: 10000, MaxKeysPerConnection: 1000,
		Providers: []Provider{
			{ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", OutputLimitField: "max_completion_tokens"},
			{ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", OutputLimitField: "max_tokens"},
			{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", OutputLimitField: "max_tokens"},
			{ID: "groq", Name: "Groq", BaseURL: "https://api.groq.com/openai/v1", OutputLimitField: "max_completion_tokens"},
			{ID: "mistral", Name: "Mistral", BaseURL: "https://api.mistral.ai/v1", OutputLimitField: "max_tokens", OmitStreamOptions: true},
			{ID: "together", Name: "Together AI", BaseURL: "https://api.together.ai/v1", OutputLimitField: "max_tokens", OmitStreamOptions: true},
			{ID: "xai", Name: "xAI", BaseURL: "https://api.x.ai/v1", OutputLimitField: "max_tokens"},
		},
	}
}

func ConfigFromEnv() (Config, error) {
	c := DefaultConfig()
	if v := os.Getenv("TABSGATE_ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("TABSGATE_DB_PATH"); v != "" {
		c.DBPath = v
	}
	secret := os.Getenv("TABSGATE_MASTER_KEY")
	if file := os.Getenv("TABSGATE_MASTER_KEY_FILE"); file != "" {
		if secret != "" {
			return c, errors.New("configure MASTER_KEY or MASTER_KEY_FILE, not both")
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return c, fmt.Errorf("read master key file: %w", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	var err error
	c.MasterKey, err = base64.StdEncoding.DecodeString(secret)
	if err != nil || len(c.MasterKey) != 32 {
		return c, errors.New("a base64-encoded 32-byte TABSGATE_MASTER_KEY or MASTER_KEY_FILE is required")
	}
	if file := os.Getenv("TABSGATE_PROVIDERS_FILE"); file != "" {
		b, e := os.ReadFile(file)
		if e != nil {
			return c, e
		}
		if e = json.Unmarshal(b, &c.Providers); e != nil {
			return c, fmt.Errorf("provider configuration: %w", e)
		}
	}
	if v := os.Getenv("TABSGATE_CORS_ORIGINS"); v != "" {
		c.Origins = strings.Split(v, ",")
	}
	if v := os.Getenv("TABSGATE_REQUEST_TIMEOUT"); v != "" {
		c.RequestTimeout, err = time.ParseDuration(v)
		if err != nil {
			return c, err
		}
	}
	if v := os.Getenv("TABSGATE_ALLOW_PRIVATE_UPSTREAMS"); v != "" {
		c.AllowPrivateUpstreams, err = strconv.ParseBool(v)
		if err != nil {
			return c, err
		}
	}
	if v := os.Getenv("TABSGATE_TRUSTED_PROXIES"); v != "" {
		for _, s := range strings.Split(v, ",") {
			p, e := netip.ParsePrefix(strings.TrimSpace(s))
			if e != nil {
				return c, e
			}
			c.TrustedProxies = append(c.TrustedProxies, p)
		}
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if len(c.MasterKey) != 32 {
		return errors.New("master key must be 32 bytes")
	}
	if c.DBPath == "" || c.DBPath == ":memory:" {
		return errors.New("a persistent SQLite file path is required")
	}
	if c.RequestTimeout < time.Second || c.RequestTimeout > 10*time.Minute {
		return errors.New("request timeout must be between 1s and 10m")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 4<<20 || c.MaxOutputTokens < 1 || c.MaxOutputTokens > 65536 {
		return errors.New("invalid request limits")
	}
	if c.MaxConnections < 1 || c.MaxKeysPerConnection < 1 {
		return errors.New("invalid capacity limits")
	}
	if len(c.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if !validID(p.ID) || seen[p.ID] {
			return errors.New("provider IDs must be unique lowercase identifiers")
		}
		seen[p.ID] = true
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(c.AllowPrivateUpstreams && u.Scheme == "http")) {
			return fmt.Errorf("invalid upstream URL for provider %s", p.ID)
		}
		if p.OutputLimitField != "max_tokens" && p.OutputLimitField != "max_completion_tokens" {
			return fmt.Errorf("invalid output limit field for %s", p.ID)
		}
	}
	return nil
}

func validID(s string) bool {
	if len(s) < 1 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
