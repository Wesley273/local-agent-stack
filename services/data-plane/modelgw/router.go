// Package modelgw implements the OpenAI-compatible model gateway:
// unified routing, priority scheduling, token limiting, request/response
// conversion and independent upstream timeouts (contract §2).
package modelgw

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/config"
)

// Backend is one selectable upstream for a model.
type Backend struct {
	URL    string
	Weight int
}

// RouteResult is the outcome of model resolution.
type RouteResult struct {
	Model    config.Model
	Backends []Backend
}

// Router resolves a model name (or prefix) to a weighted set of backends.
// Weighted round-robin is smooth and concurrency-safe; the snapshot is read
// from config.Provider on every call so hot-reload takes effect immediately.
type Router struct {
	cfg config.Provider
	mu  sync.Mutex
	// rr[model] accumulates weight for smooth weighted round robin.
	rr map[string]int
}

// NewRouter builds a Router.
func NewRouter(cfg config.Provider) *Router {
	return &Router{cfg: cfg, rr: map[string]int{}}
}

// Resolve maps a model name to its enabled backends. Strategy:
//  1. exact name match;
//  2. longest matching prefix (contract §2.4 "按 model 名或前缀");
//  3. otherwise no backend -> caller returns MODEL_NOT_FOUND (404).
func (r *Router) Resolve(name string) (RouteResult, bool) {
	snap := r.cfg.Current()
	exact := make([]config.Model, 0)
	prefix := make([]config.Model, 0)
	bestPrefixLen := -1
	for _, m := range snap.Models {
		if !m.Enabled || m.Backend == "" {
			continue
		}
		if m.Name == name {
			exact = append(exact, m)
		} else if len(name) >= len(m.Name) && m.Name != "" &&
			name[:len(m.Name)] == m.Name {
			if len(m.Name) > bestPrefixLen {
				bestPrefixLen = len(m.Name)
				prefix = append(prefix[:0], m)
			} else if len(m.Name) == bestPrefixLen {
				prefix = append(prefix, m)
			}
		}
	}
	chosen := exact
	if len(chosen) == 0 {
		chosen = prefix
	}
	if len(chosen) == 0 {
		return RouteResult{}, false
	}
	// Deterministic order so tests/E2E are reproducible.
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].Name < chosen[j].Name })
	bs := make([]Backend, 0, len(chosen))
	for _, m := range chosen {
		w := m.Weight
		if w <= 0 {
			w = 1
		}
		bs = append(bs, Backend{URL: m.Backend, Weight: w})
	}
	return RouteResult{Model: chosen[0], Backends: bs}, true
}

// Pick selects one backend with smooth weighted round-robin.
func (r *Router) Pick(name string, bs []Backend) string {
	if len(bs) == 0 {
		return ""
	}
	if len(bs) == 1 {
		return bs[0].URL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, b := range bs {
		total += b.Weight
	}
	idx := r.rr[name] % total
	r.rr[name]++
	acc := 0
	for _, b := range bs {
		acc += b.Weight
		if idx < acc {
			return b.URL
		}
	}
	return bs[len(bs)-1].URL
}

// ListModels renders the GET /v1/models response (contract §2.3).
func ListModels(snap *config.Snapshot) []byte {
	type item struct {
		ID       string `json:"id"`
		Object   string `json:"object"`
		OwnedBy  string `json:"owned_by"`
		Priority int    `json:"priority"`
		Backend  string `json:"backend"`
		Enabled  bool   `json:"enabled"`
	}
	data := make([]item, 0, len(snap.Models))
	for _, m := range snap.Models {
		data = append(data, item{
			ID: m.Name, Object: "model", OwnedBy: "local",
			Priority: m.Priority, Backend: m.Backend, Enabled: m.Enabled,
		})
	}
	b, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	return b
}

// WriteModels serves GET /v1/models.
func WriteModels(w http.ResponseWriter, snap *config.Snapshot) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ListModels(snap))
}

// ModelNotFound writes the MODEL_NOT_FOUND error envelope (§0.3).
func ModelNotFound(w http.ResponseWriter, requestID, name string) {
	writeError(w, a2a.CodeModelNotFound, "model not found or no backend available: "+name, requestID, 0)
}

func writeError(w http.ResponseWriter, code a2a.ErrorCode, msg, requestID string, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", itoa(retryAfter))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(a2a.HTTPStatus(code))
	b, _ := json.Marshal(a2a.NewError(code, msg, requestID))
	_, _ = w.Write(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
