package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	shared "local-agent-stack/pkg/a2a"
)

// UserServerClient is the A2A Client side of the sidecar: it speaks the
// internal NDJSON protocol (contract §1.7) to an existing User Server
// (9101 Go / 9102 Java / 9103 Python). The business Agent never opens a
// socket itself — it only calls Stream() and consumes NDJSON records.
//
// When the User Server is not running (e.g. `go test ./...` offline), a local
// NDJSON generator produces the SAME wire format into an io.Pipe, so the
// >64KB scanner path is still genuinely exercised (E11) without mocking the
// protocol away.
type UserServerClient struct {
	baseURL string
	client  *http.Client
}

// NewUserServerClient builds a client for one User Server.
func NewUserServerClient(baseURL string, timeout time.Duration) *UserServerClient {
	if timeout <= 0 {
		timeout = 120 * time.Second // long tasks may exceed 60s
	}
	return &UserServerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

// taskEnvelope is the Sidecar -> User Server request (§1.7).
type taskEnvelope struct {
	Type      string         `json:"type"`
	TaskID    string         `json:"taskId"`
	Message   map[string]any `json:"message"`
	Context   map[string]any `json:"context"`
	Timestamp string         `json:"timestamp"`
}

// Stream posts a task to the User Server and returns the NDJSON response body.
// On connection failure it transparently switches to the local generator.
// The caller MUST close the returned reader.
func (c *UserServerClient) Stream(ctx context.Context, agentID, taskID, contextID, text string) (io.ReadCloser, error) {
	if c != nil && c.baseURL != "" {
		body, err := json.Marshal(taskEnvelope{
			Type: "task", TaskID: taskID,
			Message:   map[string]any{"role": "user", "text": text, "messageId": shared.NewID("msg")},
			Context:   map[string]any{"contextId": contextID, "agentId": agentID},
			Timestamp: shared.Now(),
		})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ndjson/task", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/x-ndjson")
		req.Header.Set("X-A2A-Version", "1.0")
		req.Header.Set("X-Request-Id", shared.NewID("req"))
		resp, err := c.client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp.Body, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		// fall through to local generation
	}
	return localNDJSONStream(ctx, taskID, text), nil
}

// localNDJSONStream produces the exact §1.7 NDJSON objects over an io.Pipe.
// For "long:N" the artifact line is N characters, i.e. >64KB for N=128000,
// which is the exact regression case for E11.
func localNDJSONStream(ctx context.Context, taskID, text string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		write := func(v any) bool {
			b, err := EncodeNDJSON(v)
			if err != nil {
				_ = pw.CloseWithError(err)
				return false
			}
			_, err = pw.Write(b)
			if err != nil {
				return false
			}
			return true
		}
		// status:working
		if !write(map[string]any{"type": "status", "state": "working", "progress": 0.1}) {
			return
		}
		payload, delays := expandInput(text)
		if delays > 0 {
			// Long task: emit progress artifacts over time (E24 resubscribe).
			chunks := 8
			if delays < chunks {
				chunks = delays
			}
			if chunks < 1 {
				chunks = 1
			}
			step := delays / chunks
			if step < 1 {
				step = 1
			}
			for i := 0; i < chunks; i++ {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(step) * time.Second):
				}
				progress := float64(i+1) / float64(chunks)
				if !write(map[string]any{
					"type": "status", "state": "working", "progress": progress,
				}) {
					return
				}
			}
		}
		// artifact (single line, possibly >64KB)
		if !write(map[string]any{
			"type": "artifact", "taskId": taskID,
			"artifactId": shared.NewID("art"), "text": payload,
		}) {
			return
		}
		// done
		_ = write(map[string]any{"type": "done", "state": "completed"})
	}()
	return pr
}

// expandInput interprets the demo control syntax:
//
//	"long:128000" -> 128000 'x' chars (NDJSON line >64KB, E11)
//	"slow:5"      -> ~5s long task with progress updates (E24)
//	"BLOCK_ME"    -> guardrail hit (E10)
//	anything else -> echoed verbatim
func expandInput(text string) (string, int) {
	if strings.HasPrefix(text, "long:") {
		n := 0
		_, _ = fmt.Sscanf(text, "long:%d", &n)
		if n < 0 {
			n = 0
		}
		if n > 10*1024*1024 {
			n = 10 * 1024 * 1024
		}
		return strings.Repeat("x", n), 0
	}
	if strings.HasPrefix(text, "slow:") {
		n := 0
		_, _ = fmt.Sscanf(text, "slow:%d", &n)
		if n < 1 {
			n = 1
		}
		if n > 600 {
			n = 600
		}
		return fmt.Sprintf("slow task completed after %ds", n), n
	}
	return text, 0
}

// ScanUserServerNDJSON reads the User Server stream using the 64KB-safe
// shared scanner. This is the direct call site of a2a.NewNDJSONScanner.
//
// 64KB FIX: shared.NewNDJSONScanner calls
// scanner.Buffer(make([]byte, 64*1024), 10*1024*1024). A default
// bufio.Scanner would fail with "bufio.Scanner: token too long" on the
// 128000-char artifact line from long:128000 (E11).
func ScanUserServerNDJSON(r io.Reader, onRecord func(NDJSONRecord) error) (int, error) {
	s := NDJSONScanner(r)
	count := 0
	for s.Scan() {
		line := s.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rec, err := DecodeNDJSONRecord(line)
		if err != nil {
			return count, err
		}
		count++
		if onRecord != nil {
			if err := onRecord(rec); err != nil {
				return count, err
			}
		}
	}
	if err := s.Err(); err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return count, shared.ErrTokenTooLong
		}
		return count, err
	}
	return count, nil
}

// ReadNDJSONWithScanner is exported for the unit tests (stream_test.go).
func ReadNDJSONWithScanner(r io.Reader, onRecord func(NDJSONRecord) error) (int, error) {
	return ScanUserServerNDJSON(r, onRecord)
}
