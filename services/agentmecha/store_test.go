package main

import (
	"context"
	"strings"
	"testing"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// E25: restart recovery — write with one FileStore instance, read with a NEW
// instance over the same directory (simulates a process restart).
// ---------------------------------------------------------------------------

func TestFileStoreRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := &Task{
		ID: a2a.TaskID("task-restart-1"), ContextID: "ctx-1",
		Status:  TaskStatus{State: a2a.TaskStateWorking, Timestamp: &now},
		History: []*Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("persist me"))},
		Artifacts: []*Artifact{{
			ID: a2a.ArtifactID("art-1"), Name: "result",
			Parts: a2a.ContentParts{a2a.NewTextPart("saved output")},
		}},
		Metadata: map[string]any{"agentId": AgentEcho, "revision": 12},
	}
	if err := s1.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s1.Put(&Session{ContextID: "ctx-1", AgentID: AgentEcho}); err != nil {
		t.Fatal(err)
	}
	if err := s1.AppendHistory("ctx-1", *a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("turn-1"))); err != nil {
		t.Fatal(err)
	}

	// NEW instance, same directory == restart.
	s2, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.GetTask("task-restart-1")
	if !ok || got == nil {
		t.Fatal("task not recovered after restart")
	}
	if got.Status.State != a2a.TaskStateWorking {
		t.Fatalf("state = %q", got.Status.State)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Parts[0].Text() != "saved output" {
		t.Fatalf("artifact not recovered: %+v", got.Artifacts)
	}
	if got.Metadata["revision"] != float64(12) {
		t.Fatalf("metadata not recovered: %v", got.Metadata)
	}
	sess, ok := s2.Get("ctx-1")
	if !ok || len(sess.History) != 1 {
		t.Fatalf("session history not recovered: %+v", sess)
	}
	t.Logf("OK: task %s + session recovered from %s", got.ID, dir)
}

// E25 through the OFFICIAL SDK taskstore.Store surface (Create/Get/Update).
func TestSDKTaskStoreRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewFileStore(dir)
	sdk1 := NewSDKTaskStore(s1)
	now := time.Now().UTC()
	task := &Task{
		ID: a2a.TaskID("sdk-task-1"), ContextID: "ctx-sdk",
		Status:  TaskStatus{State: a2a.TaskStateSubmitted, Timestamp: &now},
		History: []*Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))},
	}
	v, err := sdk1.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("SDK Create: %v", err)
	}
	// Update with the SAME version (OCC) -> working state.
	task2 := *task
	task2.Status = TaskStatus{State: a2a.TaskStateWorking, Timestamp: &now}
	if _, err := sdk1.Update(context.Background(), &taskstore.UpdateRequest{
		Task: &task2, PrevVersion: v,
	}); err != nil {
		t.Fatalf("SDK Update: %v", err)
	}
	// Restart and read through a fresh SDK store.
	s2, _ := NewFileStore(dir)
	sdk2 := NewSDKTaskStore(s2)
	stored, err := sdk2.Get(context.Background(), a2a.TaskID("sdk-task-1"))
	if err != nil {
		t.Fatalf("SDK Get after restart: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateWorking {
		t.Fatalf("state = %q", stored.Task.Status.State)
	}
	if stored.Version < 2 {
		t.Fatalf("version not persisted: %d", stored.Version)
	}
	t.Logf("OK: SDK taskstore version %d + state %s survived restart", stored.Version, stored.Task.Status.State)
}

// OCC: a stale PrevVersion must be rejected with ErrConcurrentModification.
func TestSDKTaskStoreOCC(t *testing.T) {
	s, _ := NewFileStore(t.TempDir())
	sdk := NewSDKTaskStore(s)
	now := time.Now().UTC()
	task := &Task{ID: "occ-1", ContextID: "c", Status: TaskStatus{State: a2a.TaskStateSubmitted, Timestamp: &now}}
	v, _ := sdk.Create(context.Background(), task)
	task.Status = TaskStatus{State: a2a.TaskStateWorking, Timestamp: &now}
	if _, err := sdk.Update(context.Background(), &taskstore.UpdateRequest{Task: task, PrevVersion: v + 99}); err == nil {
		t.Fatal("stale PrevVersion must be rejected")
	} else if !strings.Contains(err.Error(), "concurrent modification") {
		t.Fatalf("want ErrConcurrentModification, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// E24: breakpoint resume — tasks/subscribe replays `working` then receives
// `completed` when a long task finishes.
// ---------------------------------------------------------------------------

func TestBreakpointResumeWorkingToCompleted(t *testing.T) {
	store := NewMemoryStore()
	m := NewTaskManager(store, store)
	rt := &AgentRuntime{
		def:     AgentDefinition{ID: AgentEcho},
		userSrv: NewUserServerClient("", 0), // offline -> local NDJSON generator
		engine:  NewGuardrailEngine(DefaultGuardrailConfig()),
		hooks:   newHookChain(DefaultGuardrailConfig(), NewGuardrailEngine(DefaultGuardrailConfig()), shared.NewSpanStore(10)),
		spans:   shared.NewSpanStore(10),
		tasks:   m,
	}
	taskID, ctxID := "resume-1", "ctx-resume"
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("slow:1"))
	go func() {
		_ = m.runTask(context.Background(), rt, taskID, ctxID, "req-1", "slow:1", msg)
	}()

	// Wait until the task reaches `working`, then subscribe mid-flight.
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, _, ok := m.Status(taskID)
		if ok && state == a2a.TaskStateWorking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task never reached working")
		}
		time.Sleep(5 * time.Millisecond)
	}
	replay, live, found := m.Subscribe(taskID, "req-sub")
	if !found {
		t.Fatal("subscribe on running task must succeed")
	}
	defer m.Unsubscribe(taskID, live)

	// Collect all frames (replay + live) until the bus closes.
	var frames []shared.SSEEvent
	frames = append(frames, replay...)
	for ev := range live {
		frames = append(frames, ev)
	}
	joined := ""
	for _, f := range frames {
		joined += f.Data
	}
	if !strings.Contains(joined, "working") {
		t.Fatalf("resume must replay working state, got %q", joined)
	}
	if !strings.Contains(joined, "completed") {
		t.Fatalf("resume must continue to completed, got %q", joined)
	}
	if !strings.Contains(joined, "artifact-update") {
		t.Fatalf("resume must include the artifact, got %q", joined)
	}
	state, _, _ := m.Status(taskID)
	if state != a2a.TaskStateCompleted {
		t.Fatalf("final state = %q", state)
	}
	t.Logf("OK: resumed from working -> completed with %d frames (E24)", len(frames))
}

// Long task (>60s contract) uses explicit ctx and never leaks state.
func TestLongTaskStateDecoupled(t *testing.T) {
	store := NewMemoryStore()
	m := NewTaskManager(store, store)
	// The business Agent only sees the interfaces; prove the interface
	// boundary by assigning to the interface types explicitly.
	var st StateStore = store
	var sess SessionStore = store
	_ = st
	_ = sess
	rt := &AgentRuntime{
		def:     AgentDefinition{ID: AgentEcho},
		userSrv: NewUserServerClient("", 0),
		engine:  NewGuardrailEngine(DefaultGuardrailConfig()),
		hooks:   newHookChain(DefaultGuardrailConfig(), NewGuardrailEngine(DefaultGuardrailConfig()), shared.NewSpanStore(10)),
		spans:   shared.NewSpanStore(10),
		tasks:   m,
	}
	ctx, cancel := context.WithCancel(context.Background())
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("slow:60"))
	done := make(chan a2a.TaskState, 1)
	go func() { done <- m.runTask(ctx, rt, "long-1", "ctx-long", "req-long", "slow:60", msg) }()
	// Cancel quickly; the task must move to canceled without dangling state.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case state := <-done:
		if state != a2a.TaskStateCanceled {
			t.Fatalf("canceled long task state = %q", state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long task did not honour ctx cancellation")
	}
}

// StateStore / SessionStore interfaces are satisfied by BOTH implementations.
func TestStoreInterfaces(t *testing.T) {
	var _ StateStore = (*FileStore)(nil)
	var _ StateStore = (*MemoryStore)(nil)
	var _ SessionStore = (*FileStore)(nil)
	var _ SessionStore = (*MemoryStore)(nil)
	file, _ := NewFileStore(t.TempDir())
	if err := file.UpdateStatus("missing", TaskStatus{State: a2a.TaskStateCompleted}); err == nil {
		t.Fatal("UpdateStatus on missing task must fail")
	}
	if err := file.AppendArtifact("missing", Artifact{}); err == nil {
		t.Fatal("AppendArtifact on missing task must fail")
	}
}
