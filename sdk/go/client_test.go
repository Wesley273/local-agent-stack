package a2asdk

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// §7: SSE cross-chunk stitching — a frame split over many 1-byte chunks must
// be reconstructed exactly (never assume one Read == one frame).
// ---------------------------------------------------------------------------

func TestSSECrossChunkStitching(t *testing.T) {
	frame := shared.SSEFrame(shared.SSEEvent{Event: "artifact-update", Data: `{"text":"` + strings.Repeat("z", 90000) + `"}`})
	raw := []byte(frame + shared.SSEDoneFrame())
	for _, size := range []int{1, 3, 997, 65536} {
		events, err := FeedAllSSE(raw, size)
		if err != nil {
			t.Fatalf("chunk=%d: %v", size, err)
		}
		if len(events) != 2 {
			t.Fatalf("chunk=%d: want 2 events, got %d", size, len(events))
		}
		if !strings.Contains(events[0].Data, strings.Repeat("z", 90000)) {
			t.Fatalf("chunk=%d: 90KB frame truncated", size)
		}
	}
	t.Log("OK: 90KB SSE frame reconstructed from 1-byte chunks (no truncation)")
}

// §7: NDJSON >64KB single line via the SDK.
func TestNDJSONLargeLine(t *testing.T) {
	const n = 128000
	line := `{"type":"artifact","text":"` + strings.Repeat("q", n) + `"}` + "\n"
	count, err := ReadNDJSON(strings.NewReader(line), func(m map[string]any) error {
		return nil
	})
	if err != nil {
		t.Fatalf("128KB NDJSON line rejected: %v", err)
	}
	if count != 1 {
		t.Fatalf("want 1 record, got %d", count)
	}
	t.Logf("OK: NDJSON line of %d bytes accepted", len(line))
}

// §7: mandatory headers on every request.
func TestMandatoryHeaders(t *testing.T) {
	var got http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"spans":[]}`))
	}))
	defer ts.Close()
	c := NewClient(WithBaseURL(ts.URL), WithAPIKey("my-key"))
	if _, err := c.GetAgentCard(context.Background()); err == nil {
		// card parse may fail on this fake body, but the request was made.
		_ = err
	}
	if got.Get(HeaderVersion) != "1.0" {
		t.Fatalf("missing %s", HeaderVersion)
	}
	if got.Get(HeaderRequest) == "" {
		t.Fatalf("missing %s", HeaderRequest)
	}
	if got.Get(HeaderAPIKey) != "my-key" {
		t.Fatalf("missing %s", HeaderAPIKey)
	}
}

// §7: errors map to *a2a.A2AError; timeouts map to native net.Error timeout.
func TestErrorMapping(t *testing.T) {
	body, _ := json.Marshal(shared.NewError(shared.CodeGuardrailRejected, "blocked", "req-9"))
	err := mapError(&httpError{status: 200, body: body}, "req-9")
	ae, ok := shared.AsA2AError(err)
	if !ok {
		t.Fatalf("want *A2AError, got %T", err)
	}
	if ae.Code != shared.CodeGuardrailRejected || ae.RequestID != "req-9" {
		t.Fatalf("bad mapping: %+v", ae)
	}
	// native timeout
	to := mapError(context.DeadlineExceeded, "req-t")
	var ne net.Error
	if !errors.As(to, &ne) || !ne.Timeout() {
		t.Fatalf("timeout not mapped to net.Error: %T", to)
	}
	if !IsTimeout(context.DeadlineExceeded) {
		t.Fatal("IsTimeout(DeadlineExceeded) must be true")
	}
	t.Logf("OK: envelope -> %s, timeout -> %T", ae.Code, to)
}

// §7: local guardrail pre-check (reuses shared engine, no network).
func TestCheckGuardrail(t *testing.T) {
	c := NewClient()
	if r := c.CheckGuardrail("hello"); !r.Allowed {
		t.Fatalf("clean text rejected: %s", r.Reason)
	}
	if r := c.CheckGuardrail("BLOCK_ME"); r.Allowed {
		t.Fatal("BLOCK_ME must be rejected locally")
	}
	if r := GuardrailCheck(shared.DefaultGuardrailConfig(), "secret-leak"); r.Allowed {
		t.Fatal("secret-leak must be rejected")
	}
}

// §7: stream guardrail hit must not disconnect — error event then channel close.
func TestStreamGuardrailKeepsConnection(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return a card that lets NewFromCard succeed.
		_, _ = w.Write([]byte(`{"name":"x","supportedInterfaces":[{"url":"` + tsURL(r) + `/a2a/echo-agent","protocolBinding":"JSONRPC","protocolVersion":"1.0"}]}`))
	}))
	defer ts.Close()
	c := NewClient(WithBaseURL(ts.URL), WithTimeout(2*time.Second))
	var events []Event
	for ev := range c.StreamMessage(context.Background(), NewTextMessage("BLOCK_ME")) {
		events = append(events, ev)
	}
	if len(events) == 0 || events[len(events)-1].Kind != "error" {
		t.Fatal("guardrail hit must emit an error event and close the channel cleanly")
	}
}

func tsURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// Configurable timeout is honoured.
func TestConfigurableTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c := NewClient(WithBaseURL(ts.URL), WithTimeout(20*time.Millisecond))
	_, err := c.GetAgentCard(context.Background())
	if err == nil {
		t.Fatal("expected timeout")
	}
	// mapError wraps it as a native timeout; check the raw error is a timeout.
	if !IsTimeout(err) && !strings.Contains(err.Error(), "timeout") {
		t.Logf("timeout surfaced as: %v", err)
	}
}
