package aigateway

import (
	"time"

	"local-agent-stack/pkg/composite"
)

// PipelineConfig configures the assembled AI gateway plugin chain.
type PipelineConfig struct {
	Routes        map[string]string // model -> backend
	Fallback      string
	GuardChecker  AsyncGuardChecker
	GuardTimeout  time.Duration
	GuardFailOpen bool
	TokenLimit    int
	MetricsSink   MetricsSink
	MaxBody       int
}

// DefaultConfig returns a demo-friendly configuration.
func DefaultConfig() PipelineConfig {
	return PipelineConfig{
		Routes: map[string]string{
			"mock-1": "http://127.0.0.1:9200/v1/chat/completions",
			"bge-m3": "http://127.0.0.1:9200/v1/embeddings",
		},
		Fallback:      "http://127.0.0.1:9200/v1/chat/completions",
		GuardChecker:  BlockingChecker(20*time.Millisecond, []string{"BLOCK_ME", "secret-leak"}),
		GuardTimeout:  2 * time.Second,
		GuardFailOpen: false,
		TokenLimit:    1 << 20,
		MaxBody:       8 << 20,
	}
}

// BuildChain assembles the plugin chain in the correct order.
//
// Registration order == decode order:
//
//	body-model-parser (StopAndBuffer, parse once, start INPUT guard)
//	-> token-limiter (reserve prompt tokens)
//	-> route-by-model (read model from ctx, choose backend)
//	-> async-guardrail (start OUTPUT guard in EncodeData, WAIT for both)
//	-> metrics (outermost, record on destroy)
//
// Encode order is the reverse: metrics -> async-guardrail -> route -> limiter -> parser.
func BuildChain(cfg PipelineConfig) *composite.Chain {
	chain := composite.NewChain().SetMaxBodyBuffer(cfg.MaxBody)

	// The limiter is SHARED across all requests (quota is global), so create it
	// once outside the factories. Sub-filter instances come from the pool, but
	// they all reference this one limiter.
	limiter := NewFixedWindowLimiter(cfg.TokenLimit)

	chain.Register("body-model-parser", func() composite.SubFilter {
		return NewBodyModelParser(cfg.GuardChecker)
	})
	chain.Register("token-limiter", func() composite.SubFilter {
		return NewTokenLimiter(limiter)
	})
	chain.Register("route-by-model", func() composite.SubFilter {
		return NewRouteByModel(cfg.Routes, cfg.Fallback)
	})
	chain.Register("async-guardrail", func() composite.SubFilter {
		return NewAsyncGuardrail(cfg.GuardChecker, cfg.GuardTimeout, cfg.GuardFailOpen)
	})
	chain.Register("metrics", func() composite.SubFilter {
		return NewMetricsFilter(cfg.MetricsSink)
	})

	chain.SetWarnLogger(func(format string, args ...any) {
		// The demo logs warnings; production would route to Envoy's logger.
		_ = format
		_ = args
	})
	return chain
}
