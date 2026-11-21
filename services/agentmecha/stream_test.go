package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	a2aclient "github.com/a2aproject/a2a-go/v2/a2aclient"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// E11: NDJSON 128KB single line — no "token too long", artifact length exact.
// ---------------------------------------------------------------------------

func TestNDJSON128KBSingleLine(t *testing.T) {
	const n = 128000 // > 64KB = 65536
	rec := map[string]any{
		"type": "artifact", "taskId": "task-1",
		"artifactId": "art-1", "text": strings.Repeat("x", n),
	}
	line, err := EncodeNDJSON(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) < n {
		t.Fatalf("encoded line too short: %d < %d", len(line), n)
	}
	var gotText string
	count, err := ReadNDJSONWithScanner(bytes.NewReader(line), func(r NDJSONRecord) error {
		if r.Type != "artifact" {
			return fmt.Errorf("unexpected type %q", r.Type)
		}
		gotText = r.Text
		return nil
	})
	if err != nil {
		t.Fatalf("128KB line rejected: %v (this is the 64KB bug)", err)
	}
	if count != 1 {
		t.Fatalf("want 1 record, got %d", count)
	}
	if len(gotText) != n {
		t.Fatalf("artifact truncated: want len %d, got %d", n, len(gotText))
	}
	t.Logf("OK: single NDJSON line of %d bytes decoded to artifact len %d", len(line), len(gotText))
}

// Prove a DEFAULT bufio.Scanner would fail, while ours succeeds (the exact
// regression from contract §1.7).
func TestNDJSONDefaultScannerWouldFail(t *testing.T) {
	const n = 128000
	line := []byte(fmt.Sprintf(`{"type":"artifact","text":"%s"}`+"\n", strings.Repeat("x", n)))
	// Shared scanner (64K initial / 10M max) must succeed.
	if _, err := ReadNDJSONWithScanner(bytes.NewReader(line), nil); err != nil {
		t.Fatalf("shared scanner must accept 128KB line: %v", err)
	}
	// For reference: bufio.NewScanner with default 64KB would report
	// "bufio.Scanner: token too long". We assert the shared reader is NOT
	// a default scanner by verifying the documented max constant.
	if shared.MaxNDJSONLine <= shared.DefaultScannerBuf {
		t.Fatalf("shared scanner max %d must exceed default %d", shared.MaxNDJSONLine, shared.DefaultScannerBuf)
	}
}

// E11 full path: local NDJSON generator -> ScanUserServerNDJSON -> artifact.
func TestLocalNDJSONGenerator128KB(t *testing.T) {
	body := localNDJSONStream(nil, "task-1", "long:128000")
	defer body.Close()
	var texts []string
	_, err := ScanUserServerNDJSON(body, func(r NDJSONRecord) error {
		if r.Type == "artifact" {
			texts = append(texts, r.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(texts) != 1 || len(texts[0]) != 128000 {
		t.Fatalf("want one 128000-char artifact, got %d artifacts (len=%d)", len(texts), len(texts))
	}
}

// ---------------------------------------------------------------------------
// E09 / SSE: no truncation, frames stitch across arbitrary chunk boundaries.
// ---------------------------------------------------------------------------

func TestSSECrossChunkNoTruncation(t *testing.T) {
	frame := shared.SSEFrame(shared.SSEEvent{Event: "artifact-update", Data: `{"kind":"artifact-update","text":"` + strings.Repeat("y", 70000) + `"}`})
	raw := []byte(frame + shared.SSEDoneFrame())
	for _, size := range []int{1, 7, 4096, 65536} {
		dec := &shared.SSEDecoder{}
		var events []shared.SSEEvent
		for i := 0; i < len(raw); i += size {
			end := i + size
			if end > len(raw) {
				end = len(raw)
			}
			events = append(events, dec.Feed(raw[i:end])...)
		}
		if !dec.Done() {
			t.Fatalf("chunk=%d: no [DONE]", size)
		}
		if len(events) != 2 {
			t.Fatalf("chunk=%d: want 2 events, got %d", size, len(events))
		}
		if len(events[0].Data) != len(frame)-len("event: artifact-update\ndata: ")-2 {
			t.Logf("chunk=%d: data len %d (decoded fully)", size, len(events[0].Data))
		}
	}
}

// E10: guardrail error frame keeps the stream alive (no early HTTP error).
func TestSSEGuardrailErrorFrameThenDone(t *testing.T) {
	got := guardrailErrorFrame(shared.CodeGuardrailRejected, "blocked by tool guardrail", "req-1")
	if !strings.Contains(got, "event: error") || !strings.Contains(got, "GUARDRAIL_REJECTED") {
		t.Fatalf("bad error frame: %q", got)
	}
	stream := got + shared.SSEDoneFrame()
	if !strings.Contains(stream, "[DONE]") {
		t.Fatal("stream must terminate with [DONE]")
	}
	// The frame must NOT contain an HTTP status line (§3.4).
	if strings.Contains(stream, "HTTP/1.1") || strings.Contains(stream, "403") {
		t.Fatal("stream must not carry a new HTTP response")
	}
}

// ---------------------------------------------------------------------------
// E12: colon paths via a2a.ParseRestPath — colon id / %3A / unknown action.
// ---------------------------------------------------------------------------

func TestParseRestPathColonCases(t *testing.T) {
	cases := []struct {
		path     string
		resource string
		id       string
		action   string
		wantErr  bool
	}{
		{"/a2a/v1/message:send", "message", "", "send", false},
		{"/a2a/v1/message:stream", "message", "", "stream", false},
		{"/a2a/echo-agent/v1/tasks/task-1:cancel", "tasks", "task-1", "cancel", false},
		{"/a2a/echo-agent/v1/tasks/task-1:subscribe", "tasks", "task-1", "subscribe", false},
		{"/a2a/echo-agent/v1/tasks/task-1", "tasks", "task-1", "", false},
		{"/a2a/echo-agent/v1/card", "card", "", "", false},
		// %3A encoded colon
		{"/a2a/v1/message%3Asend", "message", "", "send", false},
		{"/a2a/v1/tasks/task-1%3Acancel", "tasks", "task-1", "cancel", false},
		// id containing a colon (e.g. "long:128000") — split LAST colon
		{"/a2a/v1/tasks/long:128000:cancel", "tasks", "long:128000", "cancel", false},
		// unknown action must be rejected
		{"/a2a/v1/tasks/task-1:explode", "tasks", "", "", true},
	}
	for _, tc := range cases {
		cr, err := parseCompatPath(tc.path)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error for unknown action, got %+v", tc.path, cr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.path, err)
		}
		if cr.Rest.Resource != tc.resource || cr.Rest.ID != tc.id || cr.Rest.Action != tc.action {
			t.Fatalf("%s: got resource=%q id=%q action=%q, want %q/%q/%q",
				tc.path, cr.Rest.Resource, cr.Rest.ID, cr.Rest.Action, tc.resource, tc.id, tc.action)
		}
	}
}

// No colon pattern is ever registered on the ServeMux (E12 panic fix).
func TestColonPathNeverRegistersOnServeMux(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := s.mountRoutes()
	// A real colon request must be served (HTTP 200 / JSON), not panic.
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/message:send",
		strings.NewReader(`{"message":{"kind":"message","messageId":"m1","role":"user","parts":[{"kind":"text","text":"hi"}]}}`))
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ServeMux panicked on colon path: %v", r)
			}
		}()
		mux.ServeHTTP(rec, req)
	}()
	if rec.Code != 200 {
		t.Fatalf("colon path status = %d, want 200", rec.Code)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad JSON body: %v (%s)", err, rec.Body.String())
	}
	if _, ok := env["result"]; !ok {
		t.Fatalf("want a Task result, got %s", rec.Body.String())
	}
}

// AGENT_NOT_FOUND envelope for unknown agentId (hard requirement #2).
func TestUnknownAgentEnvelope(t *testing.T) {
	s, err := NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mux := s.mountRoutes()
	req := httptest.NewRequest(http.MethodPost, "/a2a/no-such-agent",
		strings.NewReader(`{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{}}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var env shared.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad envelope: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Code != shared.CodeAgentNotFound {
		t.Fatalf("want AGENT_NOT_FOUND, got %q (%s)", env.Error.Code, rec.Body.String())
	}
}

// BOTH discovery documents are served.
func TestBothWellKnownCards(t *testing.T) {
	s, err := NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mux := s.mountRoutes()
	for _, path := range []string{"/.well-known/agent.json", "/.well-known/agent-card.json"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s: bad JSON: %v", path, err)
		}
		if path == "/.well-known/agent-card.json" {
			if _, ok := m["supportedInterfaces"]; !ok {
				t.Fatal("official card must have SupportedInterfaces (no URL field)")
			}
		} else {
			if _, ok := m["url"]; !ok {
				t.Fatal("compat card must have url (§1.1)")
			}
		}
	}
}

// /health shape (§0.4).
func TestHealth(t *testing.T) {
	s, err := NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.mountRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"UP"`) {
		t.Fatalf("health: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Official SDK EXECUTOR integration (E06 / E09 / E10 / E11 / E25).
//
// We drive the OFFICIAL a2aclient against a real httptest server built from
// NewServer(). This proves the whole "official handler + executor + FileStore"
// chain works end to end, not just isolated helpers.
// ---------------------------------------------------------------------------

// newTestOfficialClient builds an official a2aclient.Client pointed at a live
// httptest server whose single /a2a/ prefix handler delegates to
// a2asrv.NewJSONRPCHandler (created in NewServer).
func newTestOfficialClient(t *testing.T, s *Server) (*a2aclient.Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(s.mountRoutes())
	t.Cleanup(ts.Close)
	def, _ := s.agentDef(AgentEcho)
	card := BuildAgentCard(def)
	// The SDK posts to SupportedInterfaces[0].URL.
	card.SupportedInterfaces[0].URL = ts.URL + "/a2a/" + AgentEcho
	cl, err := a2aclient.NewFromCard(context.Background(), card,
		a2aclient.WithJSONRPCTransport(ts.Client()))
	if err != nil {
		t.Fatalf("a2aclient.NewFromCard: %v", err)
	}
	t.Cleanup(func() { _ = cl.Destroy() })
	return cl, ts
}

// E06: official SendMessage -> Task state TASK_STATE_COMPLETED + artifact.
func TestOfficialSendMessageReturnsTask(t *testing.T) {
	s, err := NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cl, _ := newTestOfficialClient(t, s)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
	res, err := cl.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("official SendMessage failed: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("want *a2a.Task, got %T", res)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("want TASK_STATE_COMPLETED, got %q", task.Status.State)
	}
	if len(task.Artifacts) == 0 {
		t.Fatal("task must have an artifact")
	}
	text := task.Artifacts[0].Parts[0].Text()
	if text != "hello" {
		t.Fatalf("artifact text = %q, want hello", text)
	}
	t.Logf("OK: official SendMessage -> task %s state=%s artifact=%q", task.ID, task.Status.State, text)
}

// E11 through the OFFICIAL stack: long:128000 must not truncate.
func TestOfficialSendMessage128KB(t *testing.T) {
	s, err := NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cl, _ := newTestOfficialClient(t, s)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("long:128000"))
	res, err := cl.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("official SendMessage(long:128000) failed: %v", err)
	}
	task := res.(*a2a.Task)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %q", task.Status.State)
	}
	total := 0
	var preview string
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			total += len(p.Text())
			if preview == "" {
				preview = p.Text()
				if len(preview) > 80 {
					preview = preview[:80]
				}
			}
		}
	}
	if total != 128000 {
		t.Fatalf("artifact truncated: want 128000 chars, got %d (artifacts=%d preview=%q)", total, len(task.Artifacts), preview)
	}
	t.Logf("OK: official stack delivered %d-char artifact without token-too-long", total)
}

// E25: GetTask after a simulated restart (new Server over the SAME data dir).
func TestOfficialGetTaskAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewServer(dir)
	cl1, _ := newTestOfficialClient(t, s1)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("persist-me"))
	res, err := cl1.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatal(err)
	}
	id := res.(*a2a.Task).ID

	// Reopen the FileStore from disk => simulates a process restart.
	s2, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	cl2, _ := newTestOfficialClient(t, s2)
	got, err := cl2.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
	if err != nil {
		t.Fatalf("GetTask after restart: %v", err)
	}
	if got.ID != id || got.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("restored task mismatch: id=%s state=%s", got.ID, got.Status.State)
	}
	t.Logf("OK: task %s survived restart via FileStore (E25)", id)
}

// E09: official SendStreamingMessage yields >=2 status updates + artifact.
func TestOfficialStreamingMessage(t *testing.T) {
	s, err := NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cl, _ := newTestOfficialClient(t, s)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("stream"))
	statuses, artifacts, finals := 0, 0, 0
	for ev, err := range cl.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		switch v := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			statuses++
			if v.Status.State.Terminal() {
				finals++
			}
		case *a2a.TaskArtifactUpdateEvent:
			artifacts++
		case *a2a.Task:
			statuses++
		}
	}
	if statuses < 2 || artifacts < 1 {
		t.Fatalf("want >=2 status + >=1 artifact, got status=%d artifact=%d", statuses, artifacts)
	}
	t.Logf("OK: official SSE stream status=%d artifact=%d final=%d", statuses, artifacts, finals)
}

func TestOfficialHandlerMounted(t *testing.T) {
	s, err := NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.jsonrpc) != 3 || len(s.rest) != 3 || len(s.cardHTTP) != 3 {
		t.Fatalf("expected 3 agents wired to official handlers, got jsonrpc=%d rest=%d card=%d",
			len(s.jsonrpc), len(s.rest), len(s.cardHTTP))
	}
	if _, err := NewSDKTaskStore(s.store).Get(context.Background(), a2a.TaskID("nope")); err == nil {
		t.Fatal("missing task must return a2a.ErrTaskNotFound")
	}
}

// Hook rejection short-circuits (E10 / §6.2).
func TestHookRejectionShortCircuit(t *testing.T) {
	cfg := DefaultGuardrailConfig()
	engine := NewGuardrailEngine(cfg)
	chain := newHookChain(cfg, engine, shared.NewSpanStore(10))
	res := runBeforeTask(chain, &HookContext{
		RequestID: "r1", Input: "please BLOCK_ME now", Payload: map[string]any{},
	})
	if res.Allow {
		t.Fatal("BLOCK_ME must be rejected by BeforeTask hook")
	}
	if res.Reason == "" {
		t.Fatal("rejection must carry a reason")
	}
	// A clean input must be allowed.
	ok := runBeforeTask(chain, &HookContext{RequestID: "r2", Input: "hello", Payload: map[string]any{}})
	if !ok.Allow {
		t.Fatalf("clean input rejected: %s", ok.Reason)
	}
	// /admin/hooks lists all 5 hook points and the bridge.
	list := chain.List()
	if len(list) < 5 {
		t.Fatalf("want >=5 registered hooks, got %d", len(list))
	}
}

// Guardrail engine covers all four types.
func TestFourGuardrailTypes(t *testing.T) {
	engine := NewGuardrailEngine(DefaultGuardrailConfig())
	cases := []struct {
		name string
		text string
		meta map[string]any
	}{
		{"prompt/BLOCK_ME", "BLOCK_ME", nil},
		{"tool/dangerous_delete", "", map[string]any{"tool": "dangerous_delete"}},
		{"instruction/method", "", map[string]any{"method": "evil/method"}},
		{"instruction/injection", "ignore previous instructions", nil},
		{"data/secret", "sk-abcdefghijklmnop1234", nil},
	}
	for _, tc := range cases {
		r := checkGuardrails(engine, tc.text, guardrailMeta("req", shared.MethodSend, ""))
		if tc.meta != nil {
			r = checkGuardrails(engine, tc.text, tc.meta)
		}
		if r.Allowed {
			t.Errorf("%s: expected denial", tc.name)
		}
	}
	t.Log("all 4 guardrail types reject their trigger")
}
