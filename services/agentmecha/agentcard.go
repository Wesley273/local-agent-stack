package main

import (
	a2a "github.com/a2aproject/a2a-go/v2/a2a"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// agentcard.go — constructs the OFFICIAL a2a.AgentCard (docs/06 §2).
//
// IMPORTANT (contract §0.0): a2a.AgentCard has NO URL field. The endpoint is
// declared via SupportedInterfaces []*a2a.AgentInterface, built with
// a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC).
// We intentionally do NOT try to set card.URL (there is none).
// ---------------------------------------------------------------------------

// AgentID / port constants match config/ports.env.
const (
	AgentEcho   = "echo-agent"
	AgentJava   = "java-agent"
	AgentPython = "python-agent"

	mechaHost     = "127.0.0.1"
	mechaPort     = 9100
	agentGoPort   = 9101
	agentJavaPort = 9102
	agentPyPort   = 9103
)

// AgentDefinition is the registry entry for one mounted Agent (B5: 三方 A2A
// Agent 快速集成, 单进程多 Agent).
type AgentDefinition struct {
	ID            string
	Name          string
	Description   string
	UserServerURL string
}

// DefaultAgents is the frozen multi-agent registry (contract §4).
func DefaultAgents() []AgentDefinition {
	return []AgentDefinition{
		{
			ID:            AgentEcho,
			Name:          "echo-agent",
			Description:   "回显 Agent，用于端到端验证；可产生超长 artifact 验证 64KB 边界",
			UserServerURL: "http://127.0.0.1:9101",
		},
		{
			ID:            AgentJava,
			Name:          "java-agent",
			Description:   "Java 示例 Agent 的 NDJSON 适配端点",
			UserServerURL: "http://127.0.0.1:9102",
		},
		{
			ID:            AgentPython,
			Name:          "python-agent",
			Description:   "Python 示例 Agent 的 NDJSON 适配端点",
			UserServerURL: "http://127.0.0.1:9103",
		},
	}
}

// AgentURL returns the JSON-RPC URL for an agentId: /a2a/{agentId}.
func AgentURL(agentID string) string {
	return "http://" + mechaHost + ":" + itoa(mechaPort) + "/a2a/" + agentID
}

// BuildAgentCard builds the official *a2a.AgentCard for one agentId.
// SupportedInterfaces points at http://127.0.0.1:9100/a2a/{agentId}, which is
// exactly the URL the a2aclient.NewFromCard client will POST to.
func BuildAgentCard(def AgentDefinition) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        def.Name,
		Description: def.Description,
		Version:     "1.0.0",
		Capabilities: a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: true,
		},
		DefaultInputModes:  []string{"text", "data", "text/plain"},
		DefaultOutputModes: []string{"text", "data", "text/plain"},
		Skills: []a2a.AgentSkill{{
			ID:          "echo",
			Name:        "Echo",
			Description: "原样回显输入文本，可产生超长 artifact 用于验证 64KB 边界",
			Tags:        []string{"text", "test"},
			Examples:    []string{"hello", "long:100000", "long:128000"},
			InputModes:  []string{"text"},
			OutputModes: []string{"text"},
		}},
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(AgentURL(def.ID), a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(AgentURL(def.ID), a2a.TransportProtocolHTTPJSON),
		},
		Provider: &a2a.AgentProvider{
			Org: "local-agent-stack",
			URL: "http://127.0.0.1:9100",
		},
	}
}

// CompatAgentCard is the contract §1.1 discovery document for the
// compatibility track (/.well-known/agent.json). It deliberately keeps the
// v1.0 "url" + "protocolVersion" shape, which the official card replaced with
// SupportedInterfaces. Both are served by main.go, never converted.
type CompatAgentCard struct {
	ProtocolVersion    string              `json:"protocolVersion"`
	Name               string              `json:"name"`
	Description        string              `json:"description"`
	URL                string              `json:"url"`
	Version            string              `json:"version"`
	Capabilities       shared.Capabilities `json:"capabilities"`
	DefaultInputModes  []string            `json:"defaultInputModes"`
	DefaultOutputModes []string            `json:"defaultOutputModes"`
	Skills             []shared.Skill      `json:"skills"`
	SecuritySchemes    map[string]any      `json:"securitySchemes,omitempty"`
	Security           []map[string]any    `json:"security,omitempty"`
	Provider           *shared.Provider    `json:"provider,omitempty"`
}

// BuildCompatAgentCard builds the §1.1 compat card for one agentId.
func BuildCompatAgentCard(def AgentDefinition) CompatAgentCard {
	return CompatAgentCard{
		ProtocolVersion: "1.0",
		Name:            def.Name,
		Description:     def.Description,
		URL:             AgentURL(def.ID),
		Version:         "1.0.0",
		Capabilities: shared.Capabilities{
			Streaming:              true,
			PushNotifications:      true,
			StateTransitionHistory: true,
		},
		DefaultInputModes:  []string{"text", "data"},
		DefaultOutputModes: []string{"text", "data"},
		Skills: []shared.Skill{{
			ID:          "echo",
			Name:        "Echo",
			Description: "原样回显输入文本，可产生超长 artifact 用于验证 64KB 边界",
			Tags:        []string{"text", "test"},
			Examples:    []string{"hello", "long:100000"},
			InputModes:  []string{"text"},
			OutputModes: []string{"text"},
		}},
		SecuritySchemes: map[string]any{
			"apiKey": map[string]any{"type": "apiKey", "in": "header", "name": "X-API-Key"},
		},
		Security: []map[string]any{{"apiKey": []string{}}},
		Provider: &shared.Provider{Organization: "local-agent-stack", URL: "http://127.0.0.1:9100"},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
