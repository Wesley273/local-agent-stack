package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/filters"
)

// ---------------------------------------------------------------------------
// Compat track (contract §0.0 / §1.5) — simplified validation endpoints used
// by E12 回归. These endpoints are NEVER registered as colon patterns on
// http.ServeMux (Go 1.26 panics: bad wildcard segment). A single "/a2a/v1/"
// prefix handler is registered in main.go; handleCompat parses the remainder
// with a2a.ParseRestPath.
//
// This is the ONLY place the data plane "speaks" A2A shapes (Task / SSE), and
// it does so purely for the compat regression track. The transparent proxy
// path (§0.0) still forwards A2A requests verbatim to the Sidecar.
// ---------------------------------------------------------------------------

// handleCompat resolves a /a2a/v1/... path and renders a contract §1 response.
func (s *Server) handleCompat(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/a2a/v1")
	rp, err := a2a.ParseRestPath(rest)
	if err != nil {
		s.writeEnvelope(w, a2a.NewError(a2a.CodeInvalidRequest, err.Error(), ""))
		return
	}
	requestID := r.Header.Get(filters.HdrRequestID)

	switch rp.MethodForRest() {
	case a2a.MethodSend:
		s.compatSend(w, r, requestID)
	case a2a.MethodStream:
		s.compatStream(w, r, requestID)
	case a2a.MethodTasksGet:
		s.compatTaskGet(w, rp.ID, requestID)
	case a2a.MethodTasksCancel:
		s.compatTaskCancel(w, rp.ID, requestID)
	case a2a.MethodSubscribe:
		s.compatSubscribe(w, rp.ID, requestID)
	case a2a.MethodCard:
		s.compatCard(w)
	default:
		s.writeEnvelope(w, a2a.NewError(a2a.CodeInvalidRequest,
			"unknown compat action "+rp.Action, requestID))
	}
}

func (s *Server) compatSend(w http.ResponseWriter, r *http.Request, requestID string) {
	msg := readMessage(r)
	task := a2a.NewTask("task-"+a2a.NewID(""), "ctx-1", msg)
	task.Status = a2a.TaskStatus{State: a2a.StateCompleted, Timestamp: a2a.Now(),
		Message: &a2a.Message{Kind: "message", MessageID: "msg-agent", Role: "agent",
			Parts: []a2a.Part{{Kind: "text", Text: msg.Text()}}}}
	task.Artifacts = []a2a.Artifact{{
		ArtifactID: "art-1", Name: "result",
		Parts: []a2a.Part{{Kind: "text", Text: msg.Text()}},
	}}
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": requestID, "result": task,
	})
}

func (s *Server) compatStream(w http.ResponseWriter, r *http.Request, requestID string) {
	msg := readMessage(r)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	task := a2a.NewTask("task-"+a2a.NewID(""), "ctx-1", msg)
	// ≥2 status-update frames + 1 artifact-update + [DONE] (E09).
	for _, st := range []a2a.TaskState{a2a.StateSubmitted, a2a.StateWorking} {
		task.Status = a2a.TaskStatus{State: st, Timestamp: a2a.Now()}
		_, _ = w.Write([]byte(a2a.SSEStatusFrame(requestID, task, false)))
		if flusher != nil {
			flusher.Flush()
		}
	}
	task.Status = a2a.TaskStatus{State: a2a.StateCompleted, Timestamp: a2a.Now()}
	_, _ = w.Write([]byte(a2a.SSEStatusFrame(requestID, task, true)))
	if flusher != nil {
		flusher.Flush()
	}
	art := a2a.Artifact{ArtifactID: "art-1", Name: "result",
		Parts: []a2a.Part{{Kind: "text", Text: msg.Text()}}}
	_, _ = w.Write([]byte(a2a.SSEArtifactFrame(requestID, task, art, false, true, true)))
	if flusher != nil {
		flusher.Flush()
	}
	_, _ = w.Write([]byte(a2a.SSEDoneFrame()))
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) compatTaskGet(w http.ResponseWriter, id, requestID string) {
	if id == "" {
		s.writeEnvelope(w, a2a.NewError(a2a.CodeTaskNotFound, "missing task id", requestID))
		return
	}
	task := a2a.NewTask(id, "ctx-1", a2a.NewTextMessage("msg-1", "user", ""))
	task.Status = a2a.TaskStatus{State: a2a.StateCompleted, Timestamp: a2a.Now()}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": requestID, "result": task})
}

func (s *Server) compatTaskCancel(w http.ResponseWriter, id, requestID string) {
	task := a2a.NewTask(id, "ctx-1", a2a.NewTextMessage("msg-1", "user", ""))
	task.Status = a2a.TaskStatus{State: a2a.StateCanceled, Timestamp: a2a.Now()}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": requestID, "result": task})
}

func (s *Server) compatSubscribe(w http.ResponseWriter, id, requestID string) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	task := a2a.NewTask(id, "ctx-1", a2a.NewTextMessage("msg-1", "user", ""))
	for _, st := range []a2a.TaskState{a2a.StateWorking, a2a.StateCompleted} {
		task.Status = a2a.TaskStatus{State: st, Timestamp: a2a.Now()}
		_, _ = w.Write([]byte(a2a.SSEStatusFrame(requestID, task, st.IsTerminal())))
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = w.Write([]byte(a2a.SSEDoneFrame()))
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) compatCard(w http.ResponseWriter) {
	card := a2a.AgentCard{
		ProtocolVersion: "1.0", Name: "data-plane-compat",
		Description: "compat-track validation agent (E12)",
		URL:         s.SidecarBase + "/a2a/echo-agent", Version: "1.0.0",
		Capabilities:      a2a.Capabilities{Streaming: true, PushNotifications: true, StateTransitionHistory: true},
		DefaultInputModes: []string{"text", "data"}, DefaultOutputModes: []string{"text", "data"},
		Skills:   []a2a.Skill{{ID: "echo", Name: "Echo", Description: "回显", Tags: []string{"text", "test"}}},
		Provider: &a2a.Provider{Organization: "local-agent-stack", URL: s.SidecarBase},
	}
	writeJSON(w, http.StatusOK, card)
}

// readMessage extracts a Message from either a JSON-RPC params.message or a
// bare message body (tolerant, since this is a validation endpoint).
func readMessage(r *http.Request) a2a.Message {
	var m map[string]any
	b := make([]byte, 0)
	if r.Body != nil {
		b, _ = ioReadAll(r.Body)
	}
	_ = json.Unmarshal(b, &m)
	if p, ok := m["params"].(map[string]any); ok {
		if raw, ok := p["message"]; ok {
			if bb, err := json.Marshal(raw); err == nil {
				var msg a2a.Message
				if json.Unmarshal(bb, &msg) == nil {
					return normaliseMsg(msg)
				}
			}
		}
	}
	var msg a2a.Message
	if json.Unmarshal(b, &msg) == nil && msg.Kind == "message" {
		return normaliseMsg(msg)
	}
	return a2a.NewTextMessage("msg-"+a2a.NewID(""), "user", "")
}

func normaliseMsg(m a2a.Message) a2a.Message {
	if m.Kind == "" {
		m.Kind = "message"
	}
	if m.MessageID == "" {
		m.MessageID = "msg-" + a2a.NewID("")
	}
	if m.Role == "" {
		m.Role = "user"
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(mustJSON(v))
}
