package main

import (
	"context"
	"errors"
	"iter"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	a2asrv "github.com/a2aproject/a2a-go/v2/a2asrv"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// executor.go — implements the OFFICIAL a2asrv.AgentExecutor.
//
//	Execute(ctx, execCtx) iter.Seq2[a2a.Event, error]
//	Cancel(ctx, execCtx)  iter.Seq2[a2a.Event, error]
//
// SSE is produced by the SDK internally from the yielded events; this file
// NEVER writes SSE itself (contract §0.0, docs/06 §2). TaskState values are
// the official uppercase TASK_STATE_* constants, never the compat lowercase.
// ---------------------------------------------------------------------------

// AgentRuntime holds everything one mounted Agent needs. It is the ONLY thing
// the executor knows about — no net/http, no file paths, no storage internals
// (§6.3 state/session/task/persistence decoupling).
type AgentRuntime struct {
	def       AgentDefinition
	userSrv   *UserServerClient
	engine    *GuardrailEngine
	hooks     *HookChain
	spans     *shared.SpanStore
	tasks     *TaskManager
	requestID func() string
}

// sidecarExecutor implements a2asrv.AgentExecutor.
type sidecarExecutor struct {
	rt *AgentRuntime
}

// compile-time proof that we satisfy the OFFICIAL interface.
var _ a2asrv.AgentExecutor = (*sidecarExecutor)(nil)

// errStreamStopped is an internal sentinel: the SDK consumer stopped early.
var errStreamStopped = errors.New("stream consumer stopped")

// messageText extracts the plain text from the official Message parts.
func messageText(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var b []byte
	for _, p := range m.Parts {
		if p == nil {
			continue
		}
		b = append(b, p.Text()...)
	}
	return string(b)
}

// NewSidecarExecutor builds the executor for one agentId.
func NewSidecarExecutor(rt *AgentRuntime) *sidecarExecutor { return &sidecarExecutor{rt: rt} }

// Execute implements a2asrv.AgentExecutor.
//
// Sequence (contract hard requirement #3):
//
//	BeforeTask Hook + 4 guardrails
//	  -> reject: yield a GUARDRAIL_REJECTED event, keep the stream alive
//	submitted -> working -> NDJSON artifacts -> completed
//
// Streaming: the SDK turns each yielded event into an SSE frame. On a
// guardrail hit we STILL yield through to the terminal event (the [DONE]
// semantic) and never abort the connection (§3.4).
func (e *sidecarExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		rt := e.rt
		requestID := ""
		if rt.requestID != nil {
			requestID = rt.requestID()
		}
		if requestID == "" {
			requestID = shared.NewID("req")
		}
		taskID := string(execCtx.TaskID)
		contextID := execCtx.ContextID
		text := messageText(execCtx.Message)
		// Bus-only publisher for the compat subscribe rail (E24). The official
		// SDK owns task persistence via a2asrv.WithTaskStore(FileStore); we
		// MUST NOT write state here (double-write breaks SDK OCC / E25).
		bus := rt.tasks.OpenBus(taskID, requestID)

		// ---- span (contract §1.8, numeric fields are non-null) ----
		span := shared.NewSpan(requestID, "", "agentmecha.execute", "server")
		span.Set("http.method", "POST").Set("http.path", "/a2a/"+rt.def.ID).
			Set("http.status_code", 200).Set("agent.id", rt.def.ID).
			Set("app.id", "app-demo")
		defer func() {
			span.Event("executor.done")
			_ = rt.spans.Add(span.Finish("OK"))
		}()

		// ---- BeforeTask Hook + 4 guardrails (contract §6) ----
		hookRes := runBeforeTask(rt.hooks, &HookContext{
			RequestID: requestID, TaskID: taskID, AgentID: rt.def.ID,
			AppID: "app-demo", Input: text,
			Payload: map[string]any{"method": "SendMessage"},
		})
		// NOTE: do NOT pass the official PascalCase method name ("SendMessage")
		// to guardrailMeta here: the shared InstructionGuardrail's allowlist is
		// keyed by the COMPAT names ("message/send", ...). Passing an unknown
		// method would reject every request. The bridge hook already exercises
		// the allowlist with shared.MethodSend; here we only check the text.
		gr := checkGuardrails(rt.engine, text, guardrailMeta(requestID, "", ""))
		rejected := !hookRes.Allow || !gr.Allowed
		reason := hookRes.Reason
		if reason == "" {
			reason = gr.Reason
		}
		if reason == "" {
			reason = "blocked by sidecar guardrail"
		}

		// ---- submitted (official event) ----
		rejectMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("GUARDRAIL_REJECTED: "+reason))
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		PublishStatusFrame(bus, requestID, taskID, contextID, a2a.TaskStateSubmitted, false)

		if rejected {
			span.Set("guardrail.result", "REJECT")
			// §3.4: do NOT break the stream. Emit an error-carrying working
			// event, then continue to a terminal event ([DONE] semantics).
			if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, rejectMsg), nil) {
				return
			}
			PublishStatusFrame(bus, requestID, taskID, contextID, a2a.TaskStateRejected, true)
			if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateRejected, rejectMsg), nil) {
				return
			}
			CloseBus(bus)
			runAfterTask(rt.hooks, &HookContext{
				RequestID: requestID, TaskID: taskID, AgentID: rt.def.ID,
				Input: text, Payload: map[string]any{},
			})
			return
		}
		span.Set("guardrail.result", "ACCEPT")

		// ---- working (official event) ----
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		PublishStatusFrame(bus, requestID, taskID, contextID, a2a.TaskStateWorking, false)

		// ---- NDJSON stream from the User Server ----
		// The >64KB fix lives in userserver.go: ScanUserServerNDJSON uses
		// shared.NewNDJSONScanner (64K initial / 10M max). long:128000 yields
		// a single 128000-char artifact line with no "token too long" (E11).
		var artifactID a2a.ArtifactID
		terminal := a2a.TaskStateCompleted
		// consumerStopped mirrors yield's return value out of the NDJSON
		// callback (which cannot call yield directly from a nested closure).
		consumerStopped := false

		body, err := rt.userSrv.Stream(ctx, rt.def.ID, taskID, contextID, text)
		if err != nil {
			terminal = a2a.TaskStateFailed
		} else {
			defer body.Close()
			_, scanErr := ScanUserServerNDJSON(body, func(rec NDJSONRecord) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				switch rec.Type {
				case "artifact":
					// Output guardrail: emit an error semantic but NEVER
					// truncate the stream (§3.4).
					out := checkGuardrailOutput(rt.engine, rec.Text, guardrailMeta(requestID, "", ""))
					payload := rec.Text
					if !out.Allowed {
						payload = "[GUARDRAIL_REJECTED] " + out.Reason
					}
					part := a2a.NewTextPart(payload)
					PublishArtifactFrame(bus, requestID, taskID, contextID, payload)
					var ev *a2a.TaskArtifactUpdateEvent
					if artifactID == "" {
						ev = a2a.NewArtifactEvent(execCtx, part) // official constructor
						artifactID = ev.Artifact.ID
					} else {
						ev = a2a.NewArtifactUpdateEvent(execCtx, artifactID, part)
					}
					// Each yielded event becomes one SSE frame via the SDK.
					if !yield(ev, nil) {
						consumerStopped = true
						return errStreamStopped
					}
				case "error":
					terminal = a2a.TaskStateFailed
				case "done", "status":
					// progress / terminal notification; no event needed here
				}
				return nil
			})
			if scanErr != nil && !errors.Is(scanErr, errStreamStopped) {
				if ctx.Err() != nil {
					terminal = a2a.TaskStateCanceled
				} else {
					terminal = a2a.TaskStateFailed
				}
			}
		}
		if consumerStopped {
			return
		}

		// The official SDK has already persisted every yielded artifact/status
		// event into our FileStore via a2asrv.WithTaskStore (E25). We only
		// publish the terminal frame on the compat bus here.
		PublishStatusFrame(bus, requestID, taskID, contextID, terminal, terminal.Terminal())

		// ---- completed (official terminal event) ----
		// Always yield a terminal event: the SDK turns it into the final SSE
		// frame + [DONE]-equivalent stream close. Guardrail hits follow the
		// same path, so the connection is never cut (§3.4).
		if !yield(a2a.NewStatusUpdateEvent(execCtx, terminal, nil), nil) {
			return
		}
		CloseBus(bus)
		runAfterTask(rt.hooks, &HookContext{
			RequestID: requestID, TaskID: taskID, AgentID: rt.def.ID,
			Input: text, Payload: map[string]any{},
		})
	}
}

// Cancel implements a2asrv.AgentExecutor.
func (e *sidecarExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}
