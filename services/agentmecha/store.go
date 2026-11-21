package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// Contract §6.3: state / session / task / persistence DECOUPLING.
//
// The business Agent (executor.go) NEVER opens a file or touches a map
// directly; it only sees StateStore / SessionStore. The FileStore below is one
// implementation (JSON files + in-memory index); swapping in Redis/MySQL
// means writing another implementation of these two interfaces.
//
// The canonical task type is the OFFICIAL a2a-go/v2 *a2a.Task (docs/00 §5:
// "字段/类型优先直接复用官方 SDK 的类型"). Aliases keep the business code
// free of a direct SDK dependency.
// ---------------------------------------------------------------------------

// Task / TaskStatus / Artifact / Message are the official SDK types, aliased
// so that executor.go and taskmanager.go stay storage-agnostic.
type (
	Task       = a2a.Task
	TaskStatus = a2a.TaskStatus
	Artifact   = a2a.Artifact
	Message    = a2a.Message
)

// StateStore is the task-state abstraction (contract §6.3).
type StateStore interface {
	SaveTask(t *Task) error
	GetTask(id string) (*Task, bool)
	UpdateStatus(id string, s TaskStatus) error
	AppendArtifact(id string, a Artifact) error
	ListByContext(ctxID string) []*Task
}

// Session is a conversation context shared across related tasks.
type Session struct {
	ContextID string         `json:"contextId"`
	AgentID   string         `json:"agentId,omitempty"`
	History   []*Message     `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	UpdatedAt string         `json:"updatedAt,omitempty"`
}

// SessionStore is the session/history abstraction (contract §6.3).
type SessionStore interface {
	Get(ctxID string) (*Session, bool)
	Put(s *Session) error
	AppendHistory(ctxID string, m Message) error
}

// StoredRecord is the in-memory index entry for one task. Version powers the
// official SDK's optimistic concurrency control (OCC).
type StoredRecord struct {
	Task        *Task
	Version     int64
	User        string
	LastUpdated int64
}

// taskFile / sessionFile are the on-disk JSON envelopes. The ID is embedded so
// loadAll() can rebuild the index from filenames without any extra metadata.
type taskFile struct {
	ID          string `json:"id"`
	Task        *Task  `json:"task"`
	Version     int64  `json:"version"`
	User        string `json:"user,omitempty"`
	LastUpdated int64  `json:"lastUpdated"`
}

type sessionFile struct {
	ID      string   `json:"id"`
	Session *Session `json:"session"`
}

// ---------------------------------------------------------------------------
// storeCore: all logic lives here. dir == "" => memory-only (no disk writes).
// FileStore and MemoryStore are thin wrappers, proving the interface boundary.
// ---------------------------------------------------------------------------

type storeCore struct {
	mu       sync.RWMutex
	tasks    map[string]*StoredRecord
	sessions map[string]*Session
	dir      string // data dir; "" means memory-only
}

// FileStore persists tasks and sessions as JSON files and keeps an in-memory
// index. Reopening the same directory reconstructs the index from disk, which
// is exactly the E25 (restart recovery) behaviour.
type FileStore struct{ *storeCore }

// MemoryStore is the test-only, non-persistent implementation (§6.3).
type MemoryStore struct{ *storeCore }

// NewFileStore creates (or reopens) a FileStore rooted at dir and rebuilds the
// in-memory index from the JSON files already on disk (restart recovery).
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		dir = "data"
	}
	tasksDir := filepath.Join(dir, "tasks")
	sessionsDir := filepath.Join(dir, "sessions")
	for _, d := range []string{dir, tasksDir, sessionsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir %s: %w", d, err)
		}
	}
	c := &storeCore{
		tasks:    map[string]*StoredRecord{},
		sessions: map[string]*Session{},
		dir:      dir,
	}
	fs := &FileStore{storeCore: c}
	if err := fs.loadAll(); err != nil {
		return nil, err
	}
	return fs, nil
}

// NewMemoryStore builds an in-memory-only store (no files, no restart recovery).
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{storeCore: &storeCore{
		tasks:    map[string]*StoredRecord{},
		sessions: map[string]*Session{},
		dir:      "",
	}}
}

// safeFileName maps an arbitrary ID (UUIDs, "long:128000", "%3A", very long
// ids, ...) to a collision-free, filesystem-safe basename.
func safeFileName(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			if b.Len() >= 64 {
				break
			}
		} else {
			b.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(id))
	return b.String() + "-" + hex.EncodeToString(sum[:])[:12] + ".json"
}

func (s *storeCore) taskPath(id string) string {
	return filepath.Join(s.dir, "tasks", safeFileName(id))
}

func (s *storeCore) sessionPath(id string) string {
	return filepath.Join(s.dir, "sessions", safeFileName(id))
}

// atomicWrite writes data to path via a temp file + rename so a crash never
// leaves a half-written JSON record (important for restart recovery).
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadAll rebuilds the in-memory index from disk. Called exactly once by
// NewFileStore, simulating a process restart in the unit tests.
func (f *FileStore) loadAll() error {
	taskFiles, _ := filepath.Glob(filepath.Join(f.dir, "tasks", "*.json"))
	for _, p := range taskFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read task file %s: %w", p, err)
		}
		var tf taskFile
		if err := json.Unmarshal(b, &tf); err != nil {
			// A single corrupt file must not abort the whole batch (§1.8).
			continue
		}
		if tf.ID == "" || tf.Task == nil {
			continue
		}
		f.tasks[tf.ID] = &StoredRecord{
			Task: tf.Task, Version: tf.Version, User: tf.User, LastUpdated: tf.LastUpdated,
		}
	}
	sessionFiles, _ := filepath.Glob(filepath.Join(f.dir, "sessions", "*.json"))
	for _, p := range sessionFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read session file %s: %w", p, err)
		}
		var sf sessionFile
		if err := json.Unmarshal(b, &sf); err != nil {
			continue
		}
		if sf.ID == "" || sf.Session == nil {
			continue
		}
		f.sessions[sf.ID] = sf.Session
	}
	return nil
}

// cloneTask deep-copies an official Task through JSON. The SDK's Task/Part
// types have custom Marshal/Unmarshal, so this is a faithful round-trip and
// protects the store from the SDK's "events MUST NOT be modified" rule.
func cloneTask(t *Task) (*Task, error) {
	if t == nil {
		return nil, nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var out Task
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func cloneSession(s *Session) (*Session, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out Session
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// persistTaskLocked writes one task record. Caller MUST hold s.mu.
func (s *storeCore) persistTaskLocked(id string, rec *StoredRecord) error {
	if s.dir == "" {
		return nil
	}
	tf := taskFile{
		ID: id, Task: rec.Task, Version: rec.Version,
		User: rec.User, LastUpdated: rec.LastUpdated,
	}
	b, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.taskPath(id), b)
}

// persistSessionLocked writes one session record. Caller MUST hold s.mu.
func (s *storeCore) persistSessionLocked(id string, sess *Session) error {
	if s.dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(sessionFile{ID: id, Session: sess}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.sessionPath(id), b)
}

// putTaskLocked stores a task, bumping the OCC version. Caller holds s.mu.
func (s *storeCore) putTaskLocked(id string, t *Task) error {
	cp, err := cloneTask(t)
	if err != nil {
		return err
	}
	prev := s.tasks[id]
	version := int64(1)
	user := ""
	last := time.Now().UnixMilli()
	if prev != nil {
		version = prev.Version + 1
		user = prev.User
	}
	rec := &StoredRecord{Task: cp, Version: version, User: user, LastUpdated: last}
	if err := s.persistTaskLocked(id, rec); err != nil {
		return err
	}
	s.tasks[id] = rec
	return nil
}

// ---------------------------------------------------------------------------
// StateStore implementation (contract §6.3)
// ---------------------------------------------------------------------------

var _ StateStore = (*FileStore)(nil)
var _ StateStore = (*MemoryStore)(nil)

// SaveTask creates or replaces a task (used by the business adapter, never by
// the storage internals). It is idempotent-friendly for restart/replay.
func (s *storeCore) SaveTask(t *Task) error {
	if t == nil || string(t.ID) == "" {
		return fmt.Errorf("SaveTask: task or id is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putTaskLocked(string(t.ID), t)
}

// GetTask returns a defensive copy; the caller can freely mutate it.
func (s *storeCore) GetTask(id string) (*Task, bool) {
	s.mu.RLock()
	rec := s.tasks[id]
	s.mu.RUnlock()
	if rec == nil {
		return nil, false
	}
	cp, err := cloneTask(rec.Task)
	if err != nil || cp == nil {
		return nil, false
	}
	return cp, true
}

// UpdateStatus updates only the status sub-object (§6.3 decoupling).
func (s *storeCore) UpdateStatus(id string, st TaskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.tasks[id]
	if rec == nil {
		return fmt.Errorf("UpdateStatus: task %q: %s", id, shared.CodeTaskNotFound)
	}
	cp, err := cloneTask(rec.Task)
	if err != nil {
		return err
	}
	cp.Status = st
	return s.putTaskLocked(id, cp)
}

// AppendArtifact appends one artifact to a task.
func (s *storeCore) AppendArtifact(id string, a Artifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.tasks[id]
	if rec == nil {
		return fmt.Errorf("AppendArtifact: task %q: %s", id, shared.CodeTaskNotFound)
	}
	cp, err := cloneTask(rec.Task)
	if err != nil {
		return err
	}
	art := a
	cp.Artifacts = append(cp.Artifacts, &art)
	return s.putTaskLocked(id, cp)
}

// ListByContext returns all tasks belonging to a context (newest last).
func (s *storeCore) ListByContext(ctxID string) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Task, 0)
	for _, rec := range s.tasks {
		if rec.Task.ContextID != ctxID {
			continue
		}
		if cp, err := cloneTask(rec.Task); err == nil && cp != nil {
			out = append(out, cp)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return string(out[i].ID) < string(out[j].ID) })
	return out
}

// ---------------------------------------------------------------------------
// SessionStore implementation (contract §6.3)
// ---------------------------------------------------------------------------

var _ SessionStore = (*FileStore)(nil)
var _ SessionStore = (*MemoryStore)(nil)

// Get returns a defensive copy of a session.
func (s *storeCore) Get(ctxID string) (*Session, bool) {
	s.mu.RLock()
	sess := s.sessions[ctxID]
	s.mu.RUnlock()
	if sess == nil {
		return nil, false
	}
	cp, err := cloneSession(sess)
	if err != nil || cp == nil {
		return nil, false
	}
	return cp, true
}

// Put creates or replaces a session.
func (s *storeCore) Put(sess *Session) error {
	if sess == nil || sess.ContextID == "" {
		return fmt.Errorf("Put: session or contextId is nil")
	}
	cp, err := cloneSession(sess)
	if err != nil {
		return err
	}
	cp.UpdatedAt = shared.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistSessionLocked(cp.ContextID, cp); err != nil {
		return err
	}
	s.sessions[cp.ContextID] = cp
	return nil
}

// AppendHistory appends a message; a missing session is created on demand.
func (s *storeCore) AppendHistory(ctxID string, m Message) error {
	if ctxID == "" {
		return fmt.Errorf("AppendHistory: empty contextId")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[ctxID]
	var cp *Session
	if sess == nil {
		cp = &Session{ContextID: ctxID, History: []*Message{}, Metadata: map[string]any{}}
	} else {
		var err error
		cp, err = cloneSession(sess)
		if err != nil {
			return err
		}
	}
	msg := m
	cp.History = append(cp.History, &msg)
	cp.UpdatedAt = shared.Now()
	if err := s.persistSessionLocked(ctxID, cp); err != nil {
		return err
	}
	s.sessions[ctxID] = cp
	return nil
}

// ---------------------------------------------------------------------------
// Official SDK taskstore.Store adapter.
//
// SDKTaskStore is a SEPARATE type (not a method on storeCore) because the SDK
// interface method names (Get/Create/Update/List) collide with the
// StateStore/SessionStore methods of the same names. Keeping the two surfaces
// on distinct types is itself part of the §6.3 decoupling.
//
// Passing SDKTaskStore to a2asrv.NewHandler(..., a2asrv.WithTaskStore(sdk))
// makes the OFFICIAL GetTask / CancelTask / SubscribeToTask JSON-RPC methods
// read from our JSON files. This is what makes E25 survive a process restart
// and E24 (resubscribe) work while the task is running.
// ---------------------------------------------------------------------------

// SDKTaskStore adapts a storeCore to the official a2asrv/taskstore.Store.
type SDKTaskStore struct{ core *storeCore }

var _ taskstore.Store = (*SDKTaskStore)(nil)

// NewSDKTaskStore wraps any FileStore/MemoryStore for the official SDK.
func NewSDKTaskStore(s interface{ coreStore() *storeCore }) *SDKTaskStore {
	return &SDKTaskStore{core: s.coreStore()}
}

func (s *storeCore) coreStore() *storeCore { return s }

// Create implements taskstore.Store.
func (s *SDKTaskStore) Create(_ context.Context, t *a2a.Task) (taskstore.TaskVersion, error) {
	if t == nil || string(t.ID) == "" {
		return taskstore.TaskVersionMissing, a2a.ErrInvalidParams
	}
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	id := string(t.ID)
	if _, exists := c.tasks[id]; exists {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	cp, err := cloneTask(t)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	rec := &StoredRecord{Task: cp, Version: 1, LastUpdated: time.Now().UnixMilli()}
	if err := c.persistTaskLocked(id, rec); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	c.tasks[id] = rec
	return taskstore.TaskVersion(rec.Version), nil
}

// Update implements taskstore.Store with optimistic concurrency control.
func (s *SDKTaskStore) Update(_ context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if req == nil || req.Task == nil {
		return taskstore.TaskVersionMissing, a2a.ErrInvalidParams
	}
	c := s.core
	id := string(req.Task.ID)
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := c.tasks[id]
	if stored == nil {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if req.PrevVersion != taskstore.TaskVersionMissing && stored.Version != int64(req.PrevVersion) {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}
	cp, err := cloneTask(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	rec := &StoredRecord{
		Task: cp, Version: stored.Version + 1, User: stored.User,
		LastUpdated: time.Now().UnixMilli(),
	}
	if err := c.persistTaskLocked(id, rec); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	c.tasks[id] = rec
	return taskstore.TaskVersion(rec.Version), nil
}

// Get implements taskstore.Store. Missing tasks return a2a.ErrTaskNotFound.
func (s *SDKTaskStore) Get(_ context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	c := s.core
	c.mu.RLock()
	rec := c.tasks[string(taskID)]
	c.mu.RUnlock()
	if rec == nil {
		return nil, a2a.ErrTaskNotFound
	}
	cp, err := cloneTask(rec.Task)
	if err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{
		Task: cp, Version: taskstore.TaskVersion(rec.Version), User: rec.User,
	}, nil
}

// List implements taskstore.Store (used by the official ListTasks method and
// by our /admin/tasks endpoint). PageToken is an opaque decimal offset.
func (s *SDKTaskStore) List(_ context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if req == nil {
		req = &a2a.ListTasksRequest{}
	}
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = 50
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("page size must be 1..100: %w", a2a.ErrInvalidRequest)
	}
	c := s.core
	c.mu.RLock()
	all := make([]*StoredRecord, 0, len(c.tasks))
	for _, rec := range c.tasks {
		if req.ContextID != "" && rec.Task.ContextID != req.ContextID {
			continue
		}
		if req.Status != a2a.TaskStateUnspecified && rec.Task.Status.State != req.Status {
			continue
		}
		all = append(all, rec)
	}
	c.mu.RUnlock()
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].LastUpdated != all[j].LastUpdated {
			return all[i].LastUpdated > all[j].LastUpdated
		}
		return string(all[i].Task.ID) < string(all[j].Task.ID)
	})
	offset := 0
	if req.PageToken != "" {
		n, err := strconv.Atoi(req.PageToken)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid page token: %w", a2a.ErrInvalidParams)
		}
		offset = n
	}
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + pageSize
	if end > len(all) {
		end = len(all)
	}
	historyLength := 100
	if req.HistoryLength != nil {
		historyLength = *req.HistoryLength
	}
	tasks := make([]*a2a.Task, 0, end-offset)
	for _, rec := range all[offset:end] {
		cp, err := cloneTask(rec.Task)
		if err != nil {
			continue
		}
		if historyLength <= 0 {
			cp.History = []*a2a.Message{}
		} else if len(cp.History) > historyLength {
			cp.History = cp.History[len(cp.History)-historyLength:]
		}
		if !req.IncludeArtifacts {
			cp.Artifacts = nil
		}
		tasks = append(tasks, cp)
	}
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	return &a2a.ListTasksResponse{
		Tasks: tasks, TotalSize: len(all), PageSize: pageSize, NextPageToken: next,
	}, nil
}

// AllTaskIDs exposes the index for /admin/tasks diagnostics.
func (s *storeCore) AllTaskIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// VersionOf returns the current OCC version of a task (diagnostics / tests).
func (s *storeCore) VersionOf(id string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rec := s.tasks[id]; rec != nil {
		return rec.Version
	}
	return 0
}
