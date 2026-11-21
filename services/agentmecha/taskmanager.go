package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// taskmanager.go — task state machine, breakpoint resume, long tasks (>60s)
// and tasks/subscribe continuation (E24).
//
// Decoupling (§6.3): TaskManager only holds the StateStore / SessionStore
// INTERFACES. It never knows whether the backing store is FileStore or
// MemoryStore, and the business Agent never touches those stores directly.
// ---------------------------------------------------------------------------

// taskBus is the per-task event bus. Every state transition / artifact is
// recorded (for replay) AND fanned out to live subscribers, so a subscriber
// joining mid-way (tasks/subscribe) first receives the recorded history from
// `working` and then continues to `completed` — that is E24.
type taskBus struct {
	mu     sync.Mutex
	frames []shared.SSEEvent // recorded frames, replayed on a late subscribe
	subs   map[chan shared.SSEEvent]struct{}
	closed bool
	reqID  string
}

func newTaskBus(reqID string) *taskBus {
	return &taskBus{subs: map[chan shared.SSEEvent]struct{}{}, reqID: reqID}
}

// publish records a frame and pushes it to every live subscriber.
// Non-blocking send: a slow subscriber is dropped rather than blocking the
// executor (contract §3.4: never let a stream stall the producer).
func (b *taskBus) publish(ev shared.SSEEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.frames = append(b.frames, ev)
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscribe registers a live subscriber and returns (replay, live channel).
func (b *taskBus) subscribe() ([]shared.SSEEvent, chan shared.SSEEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	replay := append([]shared.SSEEvent(nil), b.frames...)
	ch := make(chan shared.SSEEvent, 256)
	if !b.closed {
		b.subs[ch] = struct{}{}
	}
	return replay, ch
}

func (b *taskBus) unsubscribe(ch chan shared.SSEEvent) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// close marks the bus terminal and closes all subscriber channels.
func (b *taskBus) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		close(ch)
		delete(b.subs, ch)
	}
}

func (b *taskBus) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// ---------------------------------------------------------------------------
// TaskManager
// ---------------------------------------------------------------------------

// TaskManager owns the task lifecycle. All persistence goes through the two
// interfaces; all continuation goes through taskBus.
type TaskManager struct {
	mu       sync.Mutex
	state    StateStore
	sessions SessionStore
	buses    map[string]*taskBus
}

// NewTaskManager builds a manager over the given interfaces.
func NewTaskManager(state StateStore, sessions SessionStore) *TaskManager {
	return &TaskManager{state: state, sessions: sessions, buses: map[string]*taskBus{}}
}

// begin creates the task in `submitted` state, records the user message in the
// session, and opens the event bus. It is idempotent for the same task id.
func (m *TaskManager) begin(taskID, contextID, agentID, requestID string, msg *Message) (*taskBus, error) {
	if taskID == "" || contextID == "" {
		return nil, fmt.Errorf("begin: taskId/contextId required")
	}
	now := time.Now().UTC()
	t := &Task{
		ID:        a2a.TaskID(taskID),
		ContextID: contextID,
		Status:    TaskStatus{State: a2a.TaskStateSubmitted, Timestamp: &now},
		Metadata:  map[string]any{"agentId": agentID},
	}
	if msg != nil {
		t.History = append(t.History, msg)
	}
	if err := m.state.SaveTask(t); err != nil {
		return nil, err
	}
	_ = m.sessions.Put(&Session{ContextID: contextID, AgentID: agentID})
	if msg != nil {
		_ = m.sessions.AppendHistory(contextID, *msg)
	}
	m.mu.Lock()
	b := m.buses[taskID]
	if b == nil {
		b = newTaskBus(requestID)
		m.buses[taskID] = b
	}
	m.mu.Unlock()
	return b, nil
}

// busFor returns (or lazily creates) the bus for a task. Used by the compat
// subscribe path when the task was created by the official SDK handler and
// therefore has no bus yet; the current state is replayed as a synthetic
// status frame.
func (m *TaskManager) busFor(taskID, requestID string) *taskBus {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buses[taskID]
	if b == nil {
		b = newTaskBus(requestID)
		m.buses[taskID] = b
	}
	return b
}

// transition applies a state change through StateStore and publishes the
// matching SSE status frame on the compat bus.
func (m *TaskManager) transition(b *taskBus, taskID string, state a2a.TaskState) {
	now := time.Now().UTC()
	_ = m.state.UpdateStatus(taskID, TaskStatus{State: state, Timestamp: &now})
	if b == nil {
		return
	}
	t, _ := m.state.GetTask(taskID)
	if t == nil {
		return
	}
	ct := compatTaskFromState(t)
	b.publish(shared.SSEEvent{Event: "status-update", Data: statusData(b.reqID, ct, state.Terminal())})
}

// addArtifact appends an artifact through StateStore and publishes an
// artifact-update frame. `text` may be >64KB (E11).
func (m *TaskManager) addArtifact(b *taskBus, taskID, text string) {
	art := Artifact{
		ID:    a2a.ArtifactID(shared.NewID("art")),
		Name:  "result",
		Parts: a2a.ContentParts{a2a.NewTextPart(text)},
	}
	_ = m.state.AppendArtifact(taskID, art)
	if b == nil {
		return
	}
	t, _ := m.state.GetTask(taskID)
	if t == nil {
		return
	}
	ct := compatTaskFromState(t)
	b.publish(shared.SSEEvent{
		Event: "artifact-update",
		Data:  artifactData(b.reqID, ct, text),
	})
}

// finish marks the task terminal and closes the bus.
func (m *TaskManager) finish(b *taskBus, taskID string, state a2a.TaskState) {
	m.transition(b, taskID, state)
	if b != nil {
		b.close()
	}
}

// Status returns the current state of a task (for tasks/get / resume).
func (m *TaskManager) Status(taskID string) (a2a.TaskState, *Task, bool) {
	t, ok := m.state.GetTask(taskID)
	if !ok || t == nil {
		return a2a.TaskStateUnspecified, nil, false
	}
	return t.Status.State, t, true
}

// Cancel moves a task to canceled (compat /tasks/{id}:cancel).
func (m *TaskManager) Cancel(taskID string) (*Task, error) {
	_, _, ok := m.Status(taskID)
	if !ok {
		return nil, fmt.Errorf("%s: %s", shared.CodeTaskNotFound, taskID)
	}
	m.finish(m.busFor(taskID, ""), taskID, a2a.TaskStateCanceled)
	t2, _ := m.state.GetTask(taskID)
	return t2, nil
}

// Subscribe returns (replay, live channel, found). A late subscriber gets all
// frames recorded so far (starting from `working`) and then live frames until
// the bus closes at `completed` — E24 breakpoint resume.
func (m *TaskManager) Subscribe(taskID, requestID string) ([]shared.SSEEvent, chan shared.SSEEvent, bool) {
	_, t, ok := m.Status(taskID)
	if !ok {
		return nil, nil, false
	}
	b := m.busFor(taskID, requestID)
	replay, ch := b.subscribe()
	// If the official SDK created the task (no recorded frames), synthesize the
	// current status as the first replayed frame so the stream starts at the
	// real state rather than from scratch.
	if len(replay) == 0 {
		ct := compatTaskFromState(t)
		replay = append(replay, shared.SSEEvent{
			Event: "status-update",
			Data:  statusData(requestID, ct, t.Status.State.Terminal()),
		})
	}
	return replay, ch, true
}

// Unsubscribe removes a live subscriber.
func (m *TaskManager) Unsubscribe(taskID string, ch chan shared.SSEEvent) {
	m.mu.Lock()
	b := m.buses[taskID]
	m.mu.Unlock()
	if b != nil {
		b.unsubscribe(ch)
	}
}

// ---------------------------------------------------------------------------
// Bus-only publishers (NO StateStore writes).
//
// The OFFICIAL executor path MUST NOT call begin/transition/AppendArtifact:
// the SDK already persists the *a2a.Task through our FileStore via
// a2asrv.WithTaskStore, and a second writer would break its optimistic
// concurrency control (double Create / version mismatch). These helpers let
// the executor publish live SSE frames to the compat subscribe bus WITHOUT
// touching storage — satisfying §6.3 decoupling while keeping E24 live.
// ---------------------------------------------------------------------------

// OpenBus returns (or creates) the live bus for a task.
func (m *TaskManager) OpenBus(taskID, requestID string) *taskBus {
	return m.busFor(taskID, requestID)
}

// PublishStatusFrame emits a status-update frame on the bus only.
func PublishStatusFrame(b *taskBus, reqID, taskID, contextID string, state a2a.TaskState, final bool) {
	if b == nil {
		return
	}
	ct := &shared.Task{
		Kind: "task", ID: taskID, ContextID: contextID,
		Status: shared.TaskStatus{State: compatState(state), Timestamp: shared.Now()},
	}
	b.publish(shared.SSEEvent{Event: "status-update", Data: statusData(reqID, ct, final)})
}

// PublishArtifactFrame emits an artifact-update frame on the bus only.
func PublishArtifactFrame(b *taskBus, reqID, taskID, contextID, text string) {
	if b == nil {
		return
	}
	ct := &shared.Task{Kind: "task", ID: taskID, ContextID: contextID}
	b.publish(shared.SSEEvent{Event: "artifact-update", Data: artifactData(reqID, ct, text)})
}

// CloseBus marks the bus terminal (called after the final official event).
func CloseBus(b *taskBus) {
	if b != nil {
		b.close()
	}
}

// ---------------------------------------------------------------------------
// Compat-track (§1) view of the official state.
//
// Contract §0.0 says the two rails are NOT converted into each other. This
// function is only used by the OWN compat endpoints (colons.go) to render a
// §1.3-shaped Task/SSE body; the official SDK rail keeps its own types.
// ---------------------------------------------------------------------------

func compatState(s a2a.TaskState) shared.TaskState {
	switch s {
	case a2a.TaskStateSubmitted:
		return shared.StateSubmitted
	case a2a.TaskStateWorking:
		return shared.StateWorking
	case a2a.TaskStateCompleted:
		return shared.StateCompleted
	case a2a.TaskStateFailed:
		return shared.StateFailed
	case a2a.TaskStateCanceled:
		return shared.StateCanceled
	case a2a.TaskStateRejected:
		return shared.StateRejected
	case a2a.TaskStateInputRequired:
		return shared.StateInputRequired
	case a2a.TaskStateAuthRequired:
		return shared.StateAuthRequired
	default:
		return shared.StateUnknown
	}
}

func compatParts(parts a2a.ContentParts) []shared.Part {
	out := make([]shared.Part, 0, len(parts))
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch c := p.Content.(type) {
		case a2a.Text:
			out = append(out, shared.Part{Kind: "text", Text: string(c)})
		case a2a.Data:
			if m, ok := c.Value.(map[string]any); ok {
				out = append(out, shared.Part{Kind: "data", Data: m})
			} else {
				out = append(out, shared.Part{Kind: "data", Data: map[string]any{"value": c.Value}})
			}
		default:
			out = append(out, shared.Part{Kind: "text", Text: p.Text()})
		}
	}
	return out
}

func compatMessage(m *Message) shared.Message {
	if m == nil {
		return shared.Message{}
	}
	return shared.Message{
		Kind: "message", MessageID: m.ID, Role: strings.ToLower(strings.TrimPrefix(string(m.Role), "ROLE_")),
		Parts: compatParts(m.Parts), ContextID: m.ContextID, TaskID: string(m.TaskID),
		Metadata: m.Metadata,
	}
}

// compatTaskFromState converts the official SDK Task into the contract §1.3
// Task shape (with "kind":"task" and lowercase states).
func compatTaskFromState(t *Task) *shared.Task {
	if t == nil {
		return nil
	}
	ct := &shared.Task{
		Kind: "task", ID: string(t.ID), ContextID: t.ContextID,
		Status:   shared.TaskStatus{State: compatState(t.Status.State), Timestamp: shared.Now()},
		Metadata: t.Metadata,
	}
	if t.Status.Message != nil {
		cm := compatMessage(t.Status.Message)
		ct.Status.Message = &cm
	}
	for _, a := range t.Artifacts {
		if a == nil {
			continue
		}
		ct.Artifacts = append(ct.Artifacts, shared.Artifact{
			ArtifactID: string(a.ID), Name: a.Name, Description: a.Description,
			Parts: compatParts(a.Parts), Metadata: a.Metadata, Extensions: a.Extensions,
		})
	}
	for _, h := range t.History {
		cm := compatMessage(h)
		ct.History = append(ct.History, cm)
	}
	return ct
}

// statusData / artifactData build single-line JSON data for §1.6 SSE frames.
func statusData(reqID string, t *shared.Task, final bool) string {
	body := map[string]any{
		"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{
			"kind": "status-update", "taskId": t.ID, "contextId": t.ContextID,
			"status": t.Status, "final": final,
		},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func artifactData(reqID string, t *shared.Task, text string) string {
	art := shared.Artifact{
		ArtifactID: shared.NewID("art"), Name: "result",
		Parts: []shared.Part{{Kind: "text", Text: text}},
	}
	body := map[string]any{
		"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{
			"kind": "artifact-update", "taskId": t.ID, "contextId": t.ContextID,
			"artifact": art, "append": false, "lastChunk": true, "final": true,
		},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// runTask is the shared task body used by BOTH the official executor and the
// compat rail. It drives: submitted -> working -> NDJSON artifacts -> terminal.
//
// ctx is honoured at every step so a client disconnect never leaves a
// dangling goroutine (long tasks can exceed 60s).
func (m *TaskManager) runTask(ctx context.Context, rt *AgentRuntime, taskID, contextID, requestID, text string, msg *Message) a2a.TaskState {
	b, err := m.begin(taskID, contextID, rt.def.ID, requestID, msg)
	if err != nil {
		return a2a.TaskStateFailed
	}
	// submitted frame
	m.transition(b, taskID, a2a.TaskStateSubmitted)
	// working frame
	m.transition(b, taskID, a2a.TaskStateWorking)

	// NDJSON stream from the User Server (or local generator when offline).
	body, err := rt.userSrv.Stream(ctx, rt.def.ID, taskID, contextID, text)
	if err != nil {
		m.finish(b, taskID, a2a.TaskStateFailed)
		return a2a.TaskStateFailed
	}
	defer body.Close()

	final := a2a.TaskStateCompleted
	_, scanErr := ScanUserServerNDJSON(body, func(rec NDJSONRecord) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		switch rec.Type {
		case "status":
			// progress updates keep the task in working (E24)
			m.transition(b, taskID, a2a.TaskStateWorking)
		case "artifact":
			// output guardrail: emit error frame semantics but DO NOT break
			// the NDJSON/SSE stream (contract §3.4)
			if out := checkGuardrailOutput(rt.engine, rec.Text, guardrailMeta(requestID, "", "")); !out.Allowed {
				m.addArtifact(b, taskID, "[GUARDRAIL_REJECTED] "+out.Reason)
			} else {
				m.addArtifact(b, taskID, rec.Text)
			}
		case "error":
			final = a2a.TaskStateFailed
		case "done":
			// terminal state decided below
		}
		return nil
	})
	if scanErr != nil {
		if ctx.Err() != nil {
			m.finish(b, taskID, a2a.TaskStateCanceled)
			return a2a.TaskStateCanceled
		}
		final = a2a.TaskStateFailed
	}
	// A cancelled context can also surface as a CLEAN EOF from the NDJSON
	// generator (the goroutine closes the pipe on ctx.Done()). Detect it here
	// so a cancelled long task never masquerades as completed (E24).
	if ctx.Err() != nil {
		m.finish(b, taskID, a2a.TaskStateCanceled)
		return a2a.TaskStateCanceled
	}
	m.finish(b, taskID, final)
	return final
}
