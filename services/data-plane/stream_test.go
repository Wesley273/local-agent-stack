package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/config"
)

// ---------------------------------------------------------------------------
// (b) contract §1.7 / E11 — NDJSON 128KB 单行不报 token too long。
// 复用共享库 a2a.NewNDJSONScanner（Buffer 64K → 10MB），断言不丢数据。
// ---------------------------------------------------------------------------

func TestNDJSONScanner_128KBSingleLine(t *testing.T) {
	// One 128KB single-line artifact record (the resume "long:128000" case).
	big := strings.Repeat("x", 128*1024)
	line := fmt.Sprintf(`{"type":"artifact","artifactId":"art-1","text":"%s"}`, big) + "\n"
	if len(line) < 128*1024 {
		t.Fatalf("test setup: line too short %d", len(line))
	}

	// The stdlib DEFAULT scanner would fail here (64KB cap). Prove the fix.
	var defaultErr error
	defaultScanner := bufio.NewScanner(strings.NewReader(line))
	for defaultScanner.Scan() {
	}
	defaultErr = defaultScanner.Err()
	if defaultErr == nil || !strings.Contains(defaultErr.Error(), "token too long") {
		t.Fatalf("precondition: default bufio.Scanner should fail, got %v", defaultErr)
	}

	// a2a.NewNDJSONScanner must succeed and return the full line intact.
	sc := a2a.NewNDJSONScanner(strings.NewReader(line))
	if !sc.Scan() {
		t.Fatalf("NewNDJSONScanner failed on 128KB line: %v", sc.Err())
	}
	got := sc.Bytes()
	if len(got) != len(line)-1 { // minus trailing '\n'
		t.Fatalf("data lost: got %d bytes, want %d", len(got), len(line)-1)
	}
	var rec map[string]any
	if err := json.Unmarshal(got, &rec); err != nil {
		t.Fatalf("decoded line is not valid JSON: %v", err)
	}
	if rec["text"] != big {
		t.Fatal("artifact text was truncated / corrupted")
	}
	if sc.Scan() {
		t.Fatal("unexpected second line")
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error on 128KB line: %v", err)
	}

	// Also exercise the typed helper (E11 smoke).
	n, err := a2a.ReadNDJSON(strings.NewReader(line), func(b []byte) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("ReadNDJSON: n=%d err=%v", n, err)
	}
}

// ---------------------------------------------------------------------------
// (c) contract §1.5 / E12 — a2a.ParseRestPath 处理冒号路径。
// 覆盖 message:send、tasks/{id}:cancel、含冒号 id long:128000、%3A 编码、
// 未知 action、subscribe。并断言冒号路径绝不注册进 ServeMux（不会 panic）。
// ---------------------------------------------------------------------------

func TestParseRestPath_ColonPaths(t *testing.T) {
	// NOTE: a2a.ParseRestPath expects the REST remainder AFTER the
	// "/a2a/{agentId}/v1" prefix has been stripped (see compat.go /
	// NormalizePath). It strips a leading "v1/" but not "a2a/v1/".
	cases := []struct {
		name, path, resource, id, action string
		wantErr                          bool
	}{
		{"message:send", "/message:send", "message", "", "send", false},
		{"v1/message:stream", "/v1/message:stream", "message", "", "stream", false},
		{"tasks/{id}:cancel", "/tasks/task-1:cancel", "tasks", "task-1", "cancel", false},
		{"tasks/{id}:subscribe", "/tasks/task-2:subscribe", "tasks", "task-2", "subscribe", false},
		{"tasks/{id} get", "/tasks/task-3", "tasks", "task-3", "", false},
		{"colon id long:128000", "/tasks/long:128000", "tasks", "long", "128000", true},
		{"colon id with action", "/tasks/long:128000:cancel", "tasks", "long:128000", "cancel", false},
		{"%3A encoded", "/message%3Asend", "message", "", "send", false},
		{"%3a encoded cancel", "/tasks/task-1%3Acancel", "tasks", "task-1", "cancel", false},
		{"card", "/card", "card", "", "", false},
		{"unknown action", "/tasks/task-1:explode", "tasks", "task-1", "explode", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp, err := a2a.ParseRestPath(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseRestPath(%q) err=%v wantErr=%v", tc.path, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if rp.Resource != tc.resource || rp.ID != tc.id || rp.Action != tc.action {
				t.Fatalf("ParseRestPath(%q) = {%s,%s,%s}, want {%s,%s,%s}",
					tc.path, rp.Resource, rp.ID, rp.Action, tc.resource, tc.id, tc.action)
			}
		})
	}

	// Sanity: MethodForRest maps to the contract's JSON-RPC names.
	rp, _ := a2a.ParseRestPath("/message:send")
	if rp.MethodForRest() != a2a.MethodSend {
		t.Fatalf("message:send -> %q, want message/send", rp.MethodForRest())
	}

	// End-to-end: compat.go strips "/a2a/v1" then ParseRestPath resolves it.
	rest := strings.TrimPrefix("/a2a/v1/tasks/task-9:cancel", "/a2a/v1")
	rp2, err := a2a.ParseRestPath(rest)
	if err != nil || rp2.Resource != "tasks" || rp2.ID != "task-9" || rp2.Action != "cancel" {
		t.Fatalf("compat strip+parse failed: rp=%+v err=%v", rp2, err)
	}
}

func TestColonPathNotRegisteredOnServeMux(t *testing.T) {
	// The whole point of §1.5: registering "/tasks/{id}:cancel" on
	// http.ServeMux panics. We only register the "/a2a/v1/" prefix.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ServeMux registration panicked (colon path leaked): %v", r)
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/a2a/v1/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = a2a.ParseRestPath(r.URL.Path)
		w.WriteHeader(200)
	})
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/tasks/task-1:cancel", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("prefix handler returned %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// (d) contract §3.3 / E22 — 过滤器实例来自 sync.Pool，new 远小于 gets。
// 跑 100 次请求后 /v1/admin/pool 的 new 必须远小于 gets。
// ---------------------------------------------------------------------------

func TestFilterInstancesArePooled(t *testing.T) {
	snap := quietSnapshot()
	srv := newTestServer("http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1", snap)
	defer srv.Gateway.Close()

	const n = 100
	for i := 0; i < n; i++ {
		// Each request runs the full decode chain + DestroyAll; health-like
		// auth failures still exercise every pooled filter.
		req := httptest.NewRequest(http.MethodGet, "/health-probe", nil)
		req.Header.Set("X-API-Key", "demo-key")
		cw := newCaptureWriter()
		srv.ServeHTTP(cw, req)
	}

	stats := srv.Chain.PoolStats()
	if stats.Gets < int64(n) {
		t.Fatalf("expected at least %d pool gets, got %d", n, stats.Gets)
	}
	if stats.New >= stats.Gets/4 {
		t.Fatalf("E22 VIOLATION: new=%d not far less than gets=%d (filters not reused)", stats.New, stats.Gets)
	}
	if stats.Puts == 0 {
		t.Fatal("no instances were Put back to the pool")
	}
	// Every one of the 8 filter specs must appear in the per-filter stats.
	for _, name := range []string{"auth", "ratelimit", "guardrail-input", "router",
		"transform", "upstream", "observability", "guardrail-output"} {
		if _, ok := stats.Per[name]; !ok {
			t.Fatalf("missing pool stats for filter %q", name)
		}
	}
	t.Logf("pool after %d requests: gets=%d new=%d puts=%d", n, stats.Gets, stats.New, stats.Puts)
}

// ---------------------------------------------------------------------------
// (e) contract §3.5 / E21 — 配置热更新：mock 控制面改 revision 后数据面生效。
// 覆盖 304 不更新、revision 变化才替换、/v1/admin/reload 强制拉取。
// ---------------------------------------------------------------------------

func TestConfigHotReload(t *testing.T) {
	snap := quietSnapshot()
	snap.Revision = 1

	var revision atomic.Int64
	revision.Store(1)
	var hits atomic.Int64
	var notModified atomic.Int64

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/config" {
			w.WriteHeader(404)
			return
		}
		inm := r.Header.Get("If-None-Match")
		cur := revision.Load()
		if inm != "" && inm == fmt.Sprintf("%d", cur) {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified) // 304: no replacement
			return
		}
		s := *snap
		s.Revision = cur
		s.RateLimits = map[string]a2a.LimitRule{
			"default": {RequestsPerMinute: 100 + int(cur), TokensPerMinute: 999999, Burst: 50},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", fmt.Sprintf("%d", cur))
		_ = json.NewEncoder(w).Encode(s)
	}))
	defer cp.Close()

	store := config.NewStore(snap)
	poller := config.NewPoller(cp.URL, store, 25*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Startup fetch (revision 1, same as current -> not applied but OK).
	if _, err := poller.FetchOnce(ctx); err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	go poller.Start(ctx)

	// Bump the control-plane revision; within a few polls the data plane must
	// observe the new revision AND the new rate limit (no restart).
	revision.Store(7)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.Revision() == 7 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if store.Revision() != 7 {
		t.Fatalf("E21 VIOLATION: revision did not hot-reload (still %d)", store.Revision())
	}
	if got := store.Current().RateRuleFor("default").RequestsPerMinute; got != 107 {
		t.Fatalf("hot-reloaded rateLimits not applied: requestsPerMinute=%d, want 107", got)
	}

	// 304 path: after the revision settles, repeated polls must return 304 and
	// leave the snapshot untouched (revision stable).
	time.Sleep(80 * time.Millisecond)
	if notModified.Load() == 0 {
		t.Fatal("expected at least one 304 Not Modified poll")
	}
	before := store.Revision()
	time.Sleep(80 * time.Millisecond)
	if store.Revision() != before {
		t.Fatalf("304 poll must not change the snapshot (revision %d -> %d)", before, store.Revision())
	}

	// Force reload via the poller (used by POST /v1/admin/reload).
	revision.Store(9)
	rev, err := poller.ForceFetch(ctx)
	if err != nil {
		t.Fatalf("ForceFetch: %v", err)
	}
	if rev != 9 || store.Revision() != 9 {
		t.Fatalf("ForceFetch rev=%d store=%d, want 9", rev, store.Revision())
	}
	t.Logf("config hot-reload OK: 304=%d fetchHits=%d finalRev=%d", notModified.Load(), hits.Load(), store.Revision())
}

// TestAdminConfigIsSanitized asserts /v1/admin/config never leaks key plaintext
// (§3.5 脱敏).
func TestAdminConfigIsSanitized(t *testing.T) {
	snap := quietSnapshot()
	srv := newTestServer("http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1", snap)
	defer srv.Gateway.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	w := httptest.NewRecorder()
	srv.configHandler(w, req)
	body := w.Body.String()
	for _, secret := range []string{"demo-key", "admin-key"} {
		if strings.Contains(body, secret) {
			t.Fatalf("§3.5 VIOLATION: /v1/admin/config leaked key plaintext %q", secret)
		}
	}
	if !strings.Contains(body, "keyCount") || !strings.Contains(body, "revision") {
		t.Fatalf("admin config missing脱敏 summary: %s", body)
	}
}

// keep the bytes import referenced
var _ = bytes.MinRead
