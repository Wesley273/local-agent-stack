package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// Poller — contract §3.5 "配置下发闭环".
//
//   - Startup: GET {CP}/v1/config full snapshot.
//   - Every 5s: GET with If-None-Match: <revision>; a 304 leaves the snapshot
//     untouched. Only a changed revision replaces the atomic.Value.
//   - ForceFetch (POST /v1/admin/reload): an out-of-band immediate fetch.
//   - The data plane never reads a hand-written local runtime config file.
// ---------------------------------------------------------------------------

// Poller periodically reconciles the Store with the control plane.
type Poller struct {
	cpURL      string
	client     *http.Client
	store      *Store
	interval   time.Duration
	forceCh    chan struct{}
	mu         sync.Mutex
	lastErr    error
	lastETag   string
	revisionCh chan int64 // notifies listeners of applied revisions (tests/admin)
}

// NewPoller builds a Poller. cpURL is the control-plane base URL such as
// "http://127.0.0.1:9000"; interval defaults to 5s when <= 0.
func NewPoller(cpURL string, store *Store, interval time.Duration) *Poller {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Poller{
		cpURL:      cpURL,
		client:     &http.Client{Timeout: 3 * time.Second},
		store:      store,
		interval:   interval,
		forceCh:    make(chan struct{}, 1),
		revisionCh: make(chan int64, 16),
	}
}

// RevisionChanges exposes a channel that receives every applied revision.
func (p *Poller) RevisionChanges() <-chan int64 { return p.revisionCh }

// LastError returns the most recent fetch error (nil after a success).
func (p *Poller) LastError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

// ForceFetch triggers an immediate reconciliation (used by /v1/admin/reload).
// It returns the resulting revision and any error from the fetch.
func (p *Poller) ForceFetch(ctx context.Context) (int64, error) {
	rev, applied, err := p.fetch(ctx)
	if err != nil {
		return p.store.Revision(), err
	}
	_ = applied
	return rev, nil
}

// FetchOnce performs a single unconditional fetch (no ticker). It is the
// startup fetch and is also handy in unit tests.
func (p *Poller) FetchOnce(ctx context.Context) (int64, error) {
	rev, _, err := p.fetch(ctx)
	return rev, err
}

// Start runs the 5s polling loop until ctx is cancelled. It performs an
// initial fetch immediately, then ticks. A signal on forceCh triggers an
// immediate extra fetch.
func (p *Poller) Start(ctx context.Context) {
	p.fetchAndLog(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.fetchAndLog(ctx)
		case <-p.forceCh:
			p.fetchAndLog(ctx)
		}
	}
}

// Trigger enqueues an async force fetch (non-blocking).
func (p *Poller) Trigger() {
	select {
	case p.forceCh <- struct{}{}:
	default:
	}
}

func (p *Poller) fetchAndLog(ctx context.Context) {
	if _, _, err := p.fetch(ctx); err != nil {
		log.Printf("[config] WARN fetch /v1/config failed: %v", err)
	}
}

// fetch is the single reconciliation step.
//   - sends If-None-Match: <current revision>
//   - 304 => no change, no replacement
//   - 200 => decode; only replace when revision differs (§3.5)
//
// Returns (revision, applied, error).
func (p *Poller) fetch(ctx context.Context) (int64, bool, error) {
	cur := p.store.Current()
	etag := strconv.FormatInt(cur.Revision, 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cpURL+"/v1/config", nil)
	if err != nil {
		return cur.Revision, false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		p.setErr(err)
		return cur.Revision, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		// 304: unchanged, keep the current atomic.Value untouched.
		p.mu.Lock()
		p.lastErr = nil
		p.lastETag = resp.Header.Get("ETag")
		p.mu.Unlock()
		return cur.Revision, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("control plane /v1/config returned HTTP %d", resp.StatusCode)
		_, _ = io.Copy(io.Discard, resp.Body)
		p.setErr(err)
		return cur.Revision, false, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		p.setErr(err)
		return cur.Revision, false, err
	}
	var snap Snapshot
	if err = json.Unmarshal(body, &snap); err != nil {
		p.setErr(err)
		return cur.Revision, false, fmt.Errorf("decode snapshot: %w", err)
	}
	// Defensive defaults so a partial snapshot never panics downstream.
	if snap.Agents == nil {
		snap.Agents = map[string]Agent{}
	}
	if snap.Policies == nil {
		snap.Policies = map[string]Policy{}
	}
	if snap.RateLimits == nil {
		snap.RateLimits = map[string]a2a.LimitRule{}
	}

	p.mu.Lock()
	p.lastErr = nil
	p.lastETag = resp.Header.Get("ETag")
	p.mu.Unlock()

	// revision 变化才替换 (§3.5). Force reload with an unchanged revision is a
	// no-op, which is exactly the desired "reconcile, don't hand-edit" behaviour.
	if snap.Revision == cur.Revision {
		return cur.Revision, false, nil
	}
	p.store.Set(&snap)
	log.Printf("[config] snapshot applied revision %d -> %d", cur.Revision, snap.Revision)
	select {
	case p.revisionCh <- snap.Revision:
	default:
	}
	return snap.Revision, true, nil
}

func (p *Poller) setErr(err error) {
	p.mu.Lock()
	p.lastErr = err
	p.mu.Unlock()
}
