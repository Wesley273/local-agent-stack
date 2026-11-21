// Package config implements the control-plane configuration snapshot (contract §4)
// and the hot-reload poller (contract §3.5).
//
// IMPORTANT (§3.5): the data plane NEVER reads a hand-written local runtime
// config file. The only runtime source of truth is GET {CP}/v1/config. The
// bootstrap default below is a compiled-in fallback used only until the first
// successful control-plane fetch (and in unit tests); it is not a config file.
package config

import (
	"sync/atomic"

	"local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// Snapshot — strictly aligned with contract §4 "GET /v1/config" payload.
// ---------------------------------------------------------------------------

// Snapshot is one full control-plane configuration version.
type Snapshot struct {
	Revision    int64                    `json:"revision"`
	GeneratedAt string                   `json:"generatedAt"`
	Agents      map[string]Agent         `json:"agents"`
	Policies    map[string]Policy        `json:"policies"`
	Grants      []Grant                  `json:"grants"`
	Models      []Model                  `json:"models"`
	RateLimits  map[string]a2a.LimitRule `json:"rateLimits"`
	Guardrails  Guardrails               `json:"guardrails"`
}

// Agent is an entry of the agents map (contract §4).
type Agent struct {
	AgentID    string            `json:"agentId"`
	URL        string            `json:"url"`
	Enabled    bool              `json:"enabled"`
	Version    string            `json:"version"`
	Skills     []string          `json:"skills"`
	AuthPolicy string            `json:"authPolicy"`
	Webhooks   []string          `json:"webhooks"`
	Metadata   map[string]string `json:"metadata"`
}

// Policy is an authentication policy (contract §4).
type Policy struct {
	Type           string            `json:"type"`
	Keys           map[string]string `json:"keys"`
	AllowAnonymous bool              `json:"allowAnonymous"`
}

// Grant authorises an app for an agent with a set of scopes.
type Grant struct {
	AppID   string   `json:"appId"`
	AgentID string   `json:"agentId"`
	Scopes  []string `json:"scopes"`
}

// Model maps a model name (or prefix) to a weighted backend.
type Model struct {
	Name     string `json:"name"`
	Backend  string `json:"backend"`
	Priority int    `json:"priority"`
	Weight   int    `json:"weight"`
	Enabled  bool   `json:"enabled"`
}

// Guardrails is the guardrails section of the snapshot.
type Guardrails struct {
	BlockedWords   []string `json:"blockedWords"`
	BlockedTools   []string `json:"blockedTools"`
	MaxPromptChars int      `json:"maxPromptChars"`
	EnableInput    bool     `json:"enableInput"`
	EnableOutput   bool     `json:"enableOutput"`
}

// GuardrailConfig converts the snapshot into the shared a2a.GuardrailConfig,
// adding the built-in secret-leak regexes used by the DataGuardrail.
func (s *Snapshot) GuardrailConfig() a2a.GuardrailConfig {
	g := s.Guardrails
	words := append([]string(nil), g.BlockedWords...)
	tools := append([]string(nil), g.BlockedTools...)
	return a2a.GuardrailConfig{
		BlockedWords:   words,
		BlockedTools:   tools,
		MaxPromptChars: g.MaxPromptChars,
		EnableInput:    g.EnableInput,
		EnableOutput:   g.EnableOutput,
		SecretPatterns: []string{`sk-[A-Za-z0-9]{16,}`, `-----BEGIN [A-Z ]*PRIVATE KEY-----`},
	}
}

// RateRuleFor returns the per-app rate rule, falling back to "default".
func (s *Snapshot) RateRuleFor(appID string) a2a.LimitRule {
	if r, ok := s.RateLimits[appID]; ok {
		return r
	}
	if r, ok := s.RateLimits["default"]; ok {
		return r
	}
	return a2a.LimitRule{RequestsPerMinute: 120, TokensPerMinute: 60000, Burst: 20}
}

// AppForKey resolves X-API-Key to an appId using the "default" policy (§0.2).
func (s *Snapshot) AppForKey(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	p, ok := s.Policies["default"]
	if !ok {
		return "", false
	}
	app, ok := p.Keys[key]
	return app, ok
}

// Allowed reports whether appID has a grant covering agentID (§0.3 FORBIDDEN).
// A wildcard agentId "*" or an exact match both satisfy the check.
func (s *Snapshot) Allowed(appID, agentID string) bool {
	for _, g := range s.Grants {
		if g.AppID != appID {
			continue
		}
		if g.AgentID == "*" || g.AgentID == "" || g.AgentID == agentID {
			return true
		}
	}
	return false
}

// Sanitized is the脱敏 view returned by GET /v1/admin/config. It never
// contains API-key plaintext (§3.5).
type Sanitized struct {
	Revision    int64                    `json:"revision"`
	GeneratedAt string                   `json:"generatedAt"`
	Agents      map[string]any           `json:"agents"`
	Policies    map[string]any           `json:"policies"`
	Grants      []Grant                  `json:"grants"`
	Models      []Model                  `json:"models"`
	RateLimits  map[string]a2a.LimitRule `json:"rateLimits"`
	Guardrails  Guardrails               `json:"guardrails"`
}

// Sanitize returns a key-free snapshot for the admin endpoint.
func (s *Snapshot) Sanitize() Sanitized {
	agents := map[string]any{}
	for id, a := range s.Agents {
		agents[id] = map[string]any{
			"agentId": a.AgentID, "url": a.URL, "enabled": a.Enabled,
			"version": a.Version, "skills": a.Skills, "authPolicy": a.AuthPolicy,
		}
	}
	policies := map[string]any{}
	for name, p := range s.Policies {
		// Keys are shown as app-id list + count, NEVER as key->app plaintext.
		apps := make([]string, 0, len(p.Keys))
		seen := map[string]bool{}
		for _, app := range p.Keys {
			if !seen[app] {
				seen[app] = true
				apps = append(apps, app)
			}
		}
		policies[name] = map[string]any{
			"type": p.Type, "keyCount": len(p.Keys), "apps": apps,
			"allowAnonymous": p.AllowAnonymous,
		}
	}
	return Sanitized{
		Revision: s.Revision, GeneratedAt: s.GeneratedAt,
		Agents: agents, Policies: policies, Grants: s.Grants,
		Models: s.Models, RateLimits: s.RateLimits, Guardrails: s.Guardrails,
	}
}

// DefaultSnapshot is the compiled-in bootstrap fallback (NOT a runtime config
// file). It mirrors contract §4 so the gateway is functional even before the
// first control-plane response.
func DefaultSnapshot() *Snapshot {
	return &Snapshot{
		Revision:    1,
		GeneratedAt: a2a.Now(),
		Agents: map[string]Agent{
			"echo-agent":   {AgentID: "echo-agent", URL: "http://127.0.0.1:9100/a2a/echo-agent", Enabled: true, Version: "1.0.0", Skills: []string{"echo"}, AuthPolicy: "default", Metadata: map[string]string{"userServer": "http://127.0.0.1:9101"}},
			"java-agent":   {AgentID: "java-agent", URL: "http://127.0.0.1:9100/a2a/java-agent", Enabled: true, Version: "1.0.0", Skills: []string{"echo"}, AuthPolicy: "default"},
			"python-agent": {AgentID: "python-agent", URL: "http://127.0.0.1:9100/a2a/python-agent", Enabled: true, Version: "1.0.0", Skills: []string{"echo"}, AuthPolicy: "default"},
		},
		Policies: map[string]Policy{
			"default": {Type: "apiKey", Keys: map[string]string{"demo-key": "app-demo", "admin-key": "app-admin"}, AllowAnonymous: false},
		},
		Grants: []Grant{
			{AppID: "app-demo", AgentID: "*", Scopes: []string{"*"}},
			{AppID: "app-admin", AgentID: "*", Scopes: []string{"*"}},
		},
		Models: []Model{
			{Name: "mock-1", Backend: "http://127.0.0.1:9200", Priority: 10, Weight: 100, Enabled: true},
			{Name: "bge-m3", Backend: "http://127.0.0.1:9200", Priority: 5, Weight: 100, Enabled: true},
		},
		RateLimits: map[string]a2a.LimitRule{
			"default":  {RequestsPerMinute: 120, TokensPerMinute: 60000, Burst: 20},
			"app-demo": {RequestsPerMinute: 30, TokensPerMinute: 10000, Burst: 5},
		},
		Guardrails: Guardrails{
			BlockedWords:   []string{"BLOCK_ME", "secret-leak"},
			BlockedTools:   []string{"dangerous_delete", "drop_database"},
			MaxPromptChars: 8000, EnableInput: true, EnableOutput: true,
		},
	}
}

// ---------------------------------------------------------------------------
// Store — atomic.Value hot replacement (§3.5).
// ---------------------------------------------------------------------------

// Provider is the read interface consumed by filters / model gateway.
type Provider interface {
	Current() *Snapshot
}

// Store holds the current *Snapshot behind an atomic.Value.
type Store struct {
	v atomic.Value // always holds a non-nil *Snapshot
}

// NewStore builds a Store seeded with init (nil => DefaultSnapshot()).
func NewStore(init *Snapshot) *Store {
	if init == nil {
		init = DefaultSnapshot()
	}
	s := &Store{}
	s.v.Store(init)
	return s
}

// Current returns the live snapshot (never nil).
func (s *Store) Current() *Snapshot { return s.v.Load().(*Snapshot) }

// Set atomically replaces the snapshot.
func (s *Store) Set(snap *Snapshot) {
	if snap != nil {
		s.v.Store(snap)
	}
}

// Revision returns the live revision number (used by /health and the poller).
func (s *Store) Revision() int64 { return s.Current().Revision }
