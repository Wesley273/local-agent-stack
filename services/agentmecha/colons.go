package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// colons.go — COMPATIBILITY TRACK (contract §0.0 / §1.5 / E12).
//
// Go 1.26 net/http.ServeMux PANICS on patterns containing a colon wildcard:
//
//	panic: parsing "/tasks/{id}:cancel": bad wildcard segment
//
// HARD RULE: we NEVER register a colon path on a ServeMux. main.go registers
// ONE prefix handler ("a2a/") and this file resolves the remainder with the
// SHARED a2a.ParseRestPath helper (pkg/a2a/types.go:398). That helper:
//   - splits the LAST colon (ids may contain colons, e.g. "long:128000")
//   - decodes %3A / %3a
//   - validates known actions (cancel / subscribe)
//
// This is the same fix the official SDK uses internally (docs/06 §2:
// "POST /tasks/{idAndAction} + strings.CutSuffix(..., \":cancel\")"), but
// ours is the explicit compat rail returning the §1-shaped Task/SSE bodies.
// ---------------------------------------------------------------------------

// compatRequest is the parsed result of the /a2a/{agentId}/v1/... remainder.
type compatRequest struct {
	AgentID string
	Rest    shared.RestPath
	Method  string // JSON-RPC-equivalent method name (§1.4)
	TaskID  string
	Action  string
	RawPath string
}

// parseCompatPath uses a2a.NormalizePath + a2a.ParseRestPath. It is exported
// for stream_test.go (E12 unit test) and used by the live handler.
func parseCompatPath(path string) (compatRequest, error) {
	agentID, rest, ok := shared.NormalizePath(path)
	if !ok {
		return compatRequest{}, fmt.Errorf("not an /a2a/ path")
	}
	rp, err := shared.ParseRestPath(rest)
	if err != nil {
		return compatRequest{}, err
	}
	cr := compatRequest{
		AgentID: agentID, Rest: rp, RawPath: path,
		TaskID: rp.ID, Action: rp.Action, Method: rp.MethodForRest(),
	}
	return cr, nil
}

// writeJSON writes a JSON body with the standard content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	b, err := json.Marshal(v)
	if err != nil {
		_, _ = io.WriteString(w, `{"error":{"code":"INTERNAL_ERROR"}}`)
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

// writeEnvelope writes the unified §0.3 error envelope.
func writeEnvelope(w http.ResponseWriter, code shared.ErrorCode, msg, requestID string) {
	writeJSON(w, shared.HTTPStatus(code), shared.NewError(code, msg, requestID))
}

// handleCompatTrack is the single handler for all /a2a/{agentId}/v1/... and
// /a2a/v1/... colon paths. It is called by main.go's ONE "/a2a/" prefix
// handler; no colon path is ever handed to ServeMux.
func (s *Server) handleCompatTrack(w http.ResponseWriter, r *http.Request) {
	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = shared.NewID("req")
	}
	cr, err := parseCompatPath(r.URL.Path)
	if err != nil {
		writeEnvelope(w, shared.CodeInvalidRequest, err.Error(), requestID)
		return
	}
	// agentId validation (hard requirement #2: AGENT_NOT_FOUND envelope).
	// "v1" is the top-level compat prefix (/a2a/v1/*), NOT an agentId; in that
	// case the default agent (echo-agent) is used by the handlers below.
	if cr.AgentID != "" && cr.AgentID != "v1" && !s.hasAgent(cr.AgentID) {
		writeEnvelope(w, shared.CodeAgentNotFound,
			"agentId "+cr.AgentID+" is not registered", requestID)
		return
	}
	if cr.AgentID == "v1" {
		cr.AgentID = ""
	}

	switch {
	case cr.Rest.Resource == "card":
		s.handleCompatCard(w, cr.AgentID, requestID)
	case cr.Action == "send":
		s.handleCompatSend(w, r, cr, requestID)
	case cr.Action == "stream":
		s.handleCompatStream(w, r, cr, requestID)
	case cr.Rest.Resource == "tasks" && cr.Action == "cancel":
		s.handleCompatCancel(w, cr, requestID)
	case cr.Rest.Resource == "tasks" && cr.Action == "subscribe":
		s.handleCompatSubscribe(w, r, cr, requestID)
	case cr.Rest.Resource == "tasks":
		s.handleCompatGet(w, cr, requestID)
	default:
		writeEnvelope(w, shared.CodeInvalidRequest,
			"unsupported compat path: "+cr.RawPath, requestID)
	}
}

// handleCompatCard: GET /a2a/v1/card (or /a2a/{id}/v1/card).
func (s *Server) handleCompatCard(w http.ResponseWriter, agentID, _ string) {
	if agentID == "" {
		agentID = AgentEcho
	}
	def, ok := s.agentDef(agentID)
	if !ok {
		writeEnvelope(w, shared.CodeAgentNotFound, "unknown agentId "+agentID, "")
		return
	}
	writeJSON(w, http.StatusOK, BuildCompatAgentCard(def))
}

// readCompatMessage parses a §1.2 Message from a JSON-RPC message/send body
// into the official SDK Message used to drive the shared runTask.
func readCompatMessage(body []byte) (*a2a.Message, string, error) {
	// Accept both the §1.4 JSON-RPC envelope and a bare Message (E12 curl).
	var env struct {
		Params struct {
			Message json.RawMessage `json:"message"`
		} `json:"params"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		raw := env.Params.Message
		if len(raw) == 0 {
			raw = env.Message
		}
		if len(raw) > 0 {
			var cm shared.Message
			if err := json.Unmarshal(raw, &cm); err == nil {
				return compatMessageToOfficial(cm), cm.Text(), nil
			}
		}
	}
	var cm shared.Message
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, "", err
	}
	if cm.Kind == "" {
		cm.Kind = "message"
	}
	return compatMessageToOfficial(cm), cm.Text(), nil
}

// compatMessageToOfficial converts the §1.2 Message into the official SDK
// Message type (the two rails are not converted into each other at the
// PROTOCOL level; this is just the input adapter for the shared task body).
func compatMessageToOfficial(cm shared.Message) *a2a.Message {
	parts := make(a2a.ContentParts, 0, len(cm.Parts))
	for _, p := range cm.Parts {
		switch p.Kind {
		case "text":
			parts = append(parts, a2a.NewTextPart(p.Text))
		case "data":
			parts = append(parts, a2a.NewDataPart(p.Data))
		default:
			parts = append(parts, a2a.NewTextPart(p.Text))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, a2a.NewTextPart(cm.Text()))
	}
	return &a2a.Message{
		ID: cm.MessageID, Role: a2a.MessageRoleUser, Parts: parts,
		ContextID: cm.ContextID, TaskID: a2a.TaskID(cm.TaskID),
	}
}

// handleCompatSend: POST /a2a/v1/message:send (E12).
func (s *Server) handleCompatSend(w http.ResponseWriter, r *http.Request, cr compatRequest, requestID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		writeEnvelope(w, shared.CodeInvalidRequest, err.Error(), requestID)
		return
	}
	msg, text, err := readCompatMessage(body)
	if err != nil {
		writeEnvelope(w, shared.CodeInvalidRequest, "bad message: "+err.Error(), requestID)
		return
	}
	agentID := cr.AgentID
	if agentID == "" {
		agentID = AgentEcho
	}
	rt, ok := s.runtime(agentID)
	if !ok {
		writeEnvelope(w, shared.CodeAgentNotFound, "unknown agentId "+agentID, requestID)
		return
	}
	// guardrail (non-stream): §0.3 GUARDRAIL_REJECTED envelope, HTTP 200.
	gr := checkGuardrails(rt.engine, text, guardrailMeta(requestID, shared.MethodSend, ""))
	if !gr.Allowed {
		writeJSON(w, http.StatusOK, gr.AsErrorEnvelope())
		return
	}
	taskID := string(msg.TaskID)
	if taskID == "" {
		taskID = shared.NewID("task")
	}
	contextID := msg.ContextID
	if contextID == "" {
		contextID = shared.NewID("ctx")
	}
	state := rt.tasks.runTask(r.Context(), rt, taskID, contextID, requestID, text, msg)
	t, _ := rt.tasks.state.GetTask(taskID)
	ct := compatTaskFromState(t)
	if ct == nil {
		ct = &shared.Task{Kind: "task", ID: taskID, ContextID: contextID}
	}
	// JSON-RPC 2.0 success response carrying a §1.3 Task.
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": requestID, "result": ct,
	})
	_ = state
}

// handleCompatStream: POST /a2a/v1/message:stream (SSE, §1.6).
func (s *Server) handleCompatStream(w http.ResponseWriter, r *http.Request, cr compatRequest, requestID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		writeEnvelope(w, shared.CodeInvalidRequest, err.Error(), requestID)
		return
	}
	msg, text, err := readCompatMessage(body)
	if err != nil {
		writeEnvelope(w, shared.CodeInvalidRequest, "bad message: "+err.Error(), requestID)
		return
	}
	agentID := cr.AgentID
	if agentID == "" {
		agentID = AgentEcho
	}
	rt, ok := s.runtime(agentID)
	if !ok {
		writeEnvelope(w, shared.CodeAgentNotFound, "unknown agentId "+agentID, requestID)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeEnvelope(w, shared.CodeInternalError, "streaming unsupported", requestID)
		return
	}
	// SSE headers (§1.6), no Content-Length, flush per frame.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	taskID := string(msg.TaskID)
	if taskID == "" {
		taskID = shared.NewID("task")
	}
	contextID := msg.ContextID
	if contextID == "" {
		contextID = shared.NewID("ctx")
	}

	// Guardrail hit during a stream: send an `event: error` frame and then
	// [DONE]; NEVER write a new HTTP status, NEVER close early (§3.4 / E10).
	gr := checkGuardrails(rt.engine, text, guardrailMeta(requestID, shared.MethodStream, ""))
	if !gr.Allowed {
		_, _ = io.WriteString(w, shared.SSEErrorFrame(shared.CodeGuardrailRejected, gr.Reason, requestID))
		flusher.Flush()
		_, _ = io.WriteString(w, shared.SSEDoneFrame())
		flusher.Flush()
		return
	}

	// Drive the shared task body in a goroutine; the SSE frames come from the
	// compat bus (submitted/working/artifact/completed) via Subscribe.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	bus := rt.tasks.OpenBus(taskID, requestID)
	go func() {
		_ = rt.tasks.runTask(ctx, rt, taskID, contextID, requestID, text, msg)
	}()
	replay, live, _ := rt.tasks.Subscribe(taskID, requestID)
	defer rt.tasks.Unsubscribe(taskID, live)
	writeFrames := func(evs []shared.SSEEvent) {
		for _, ev := range evs {
			_, _ = io.WriteString(w, shared.SSEFrame(ev))
			flusher.Flush()
			if strings.TrimSpace(ev.Data) == "[DONE]" {
				return
			}
		}
	}
	writeFrames(replay)
	for ev := range live {
		writeFrames([]shared.SSEEvent{ev})
	}
	_ = bus
	_, _ = io.WriteString(w, shared.SSEDoneFrame())
	flusher.Flush()
}

// handleCompatGet: GET /a2a/v1/tasks/{id} (E25 on the compat rail).
func (s *Server) handleCompatGet(w http.ResponseWriter, cr compatRequest, requestID string) {
	t, ok := s.store.GetTask(cr.TaskID)
	if !ok {
		writeEnvelope(w, shared.CodeTaskNotFound, "task not found: "+cr.TaskID, requestID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": requestID, "result": compatTaskFromState(t),
	})
}

// handleCompatCancel: POST /a2a/v1/tasks/{id}:cancel (E12).
func (s *Server) handleCompatCancel(w http.ResponseWriter, cr compatRequest, requestID string) {
	t, err := s.manager.Cancel(cr.TaskID)
	if err != nil {
		writeEnvelope(w, shared.CodeTaskNotFound, err.Error(), requestID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": requestID, "result": compatTaskFromState(t),
	})
}

// handleCompatSubscribe: GET /a2a/v1/tasks/{id}:subscribe (E24, §1.6 SSE).
func (s *Server) handleCompatSubscribe(w http.ResponseWriter, r *http.Request, cr compatRequest, requestID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeEnvelope(w, shared.CodeInternalError, "streaming unsupported", requestID)
		return
	}
	replay, live, found := s.manager.Subscribe(cr.TaskID, requestID)
	if !found {
		writeEnvelope(w, shared.CodeTaskNotFound, "task not found: "+cr.TaskID, requestID)
		return
	}
	defer s.manager.Unsubscribe(cr.TaskID, live)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	for _, ev := range replay {
		_, _ = io.WriteString(w, shared.SSEFrame(ev))
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-live:
			if !ok {
				_, _ = io.WriteString(w, shared.SSEDoneFrame())
				flusher.Flush()
				return
			}
			_, _ = io.WriteString(w, shared.SSEFrame(ev))
			flusher.Flush()
		case <-time.After(15 * time.Second):
			// keep-alive comment, never a new HTTP response
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}
