package composite

// Session binds ONE set of sub-filter instances to ONE request/response stream,
// mirroring Envoy: a filter instance is created once per stream and used for
// DecodeHeaders, DecodeData, EncodeHeaders, EncodeData and OnDestroy.
//
// Without a Session, acquiring from the pool per phase could give a different
// instance in EncodeData than in DecodeHeaders, breaking filters that keep
// per-stream state (the exact bug class behind "creating a new filter instance
// per request" OOM/state issues).
type Session struct {
	chain *Chain
	ctx   *Context
	rs    *runState
	done  bool
}

// NewSession acquires a fresh set of pooled sub-filters for this stream.
func (c *Chain) NewSession(ctx *Context) *Session {
	if ctx.MaxBody <= 0 {
		ctx.MaxBody = c.maxBuffer
	}
	return &Session{chain: c, ctx: ctx, rs: c.acquireAll()}
}

// Context returns the shared context for this stream.
func (s *Session) Context() *Context { return s.ctx }

// FlowAction tells the OUTER Envoy Golang filter what to do. This is the piece
// that was missing when everything lived in one plugin: the inner chain's
// StopAndBuffer must be propagated to Envoy, otherwise Envoy keeps streaming
// the body to the next filter / upstream while we are still buffering.
type FlowAction int

const (
	FlowContinue      FlowAction = iota // Envoy: Continue
	FlowStopAndBuffer                   // Envoy: StopIterationAndBuffer (still collecting body)
	FlowStopAndReply                    // local reply; see Decision.Reply
	FlowReady                           // body fully buffered+parsed, downstream filters ran -> release body
)

func (a FlowAction) String() string {
	switch a {
	case FlowContinue:
		return "Continue"
	case FlowStopAndBuffer:
		return "StopAndBuffer"
	case FlowStopAndReply:
		return "StopAndReply"
	case FlowReady:
		return "Ready"
	}
	return "Unknown"
}

// Decision is the outer flow-control result of a decode phase.
type Decision struct {
	Action FlowAction
	Reply  *Reply // non-nil only when Action == FlowStopAndReply
	// ReleaseBody is the rewritten full body to inject downstream when Action
	// == FlowReady (the composite buffered it). nil means "use original bytes".
	ReleaseBody []byte
}

func errDecision(r *Reply, err error) (*Decision, error) {
	return &Decision{Action: FlowStopAndReply, Reply: r}, err
}

// RunDecodeHeaders runs DecodeHeaders forward and PROPAGATES StopAndBuffer to
// the caller so the outer Envoy filter can stop decoding.
func (s *Session) RunDecodeHeaders(endStream bool) (*Decision, error) {
	ctx, rs := s.ctx, s.rs
	for i, f := range rs.insts {
		st := f.DecodeHeaders(ctx, endStream)
		switch st {
		case StopAndReply:
			return errDecision(s.chain.makeReply(ctx), nil)
		case StopAndBuffer:
			ctx.BeginRequestBodyBuffer()
			ctx.Set("_buffer_from", i)
			s.chain.warnf("[%s] StopAndBuffer -> propagate FlowStopAndBuffer to Envoy", f.Name())
			return &Decision{Action: FlowStopAndBuffer}, nil
		}
	}
	return &Decision{Action: FlowContinue}, nil
}

// RunDecodeData feeds request bytes. Handles the buffer/parse-once state
// machine and returns the correct outer FlowAction at every step.
func (s *Session) RunDecodeData(chunk []byte, endStream bool) (*Decision, error) {
	ctx, rs := s.ctx, s.rs

	if ctx.BodyState == BodyCollecting {
		if err := ctx.AppendRequestChunk(chunk); err != nil {
			ctx.Reject("INVALID_REQUEST", err.Error())
			return errDecision(s.chain.makeReply(ctx), err)
		}
		if !endStream {
			// Still buffering: tell Envoy to keep stopping decoding.
			return &Decision{Action: FlowStopAndBuffer}, nil
		}
		if err := ctx.FinishRequestBodyParse(); err != nil {
			ctx.Reject("INVALID_REQUEST", err.Error())
			return errDecision(s.chain.makeReply(ctx), err)
		}
		ctx.Set("_buffer_released", true)
		from := 0
		if v, ok := ctx.Get("_buffer_from").(int); ok {
			from = v
		}
		for i := from; i < len(rs.insts); i++ {
			st := rs.insts[i].DecodeData(ctx, ctx.Body, true)
			if st == StopAndReply {
				return errDecision(s.chain.makeReply(ctx), nil)
			}
		}
		// Buffered body is now parsed and published; release it downstream.
		return &Decision{Action: FlowReady, ReleaseBody: append([]byte(nil), ctx.Body...)}, nil
	}

	for i, f := range rs.insts {
		st := f.DecodeData(ctx, chunk, endStream)
		switch st {
		case StopAndReply:
			return errDecision(s.chain.makeReply(ctx), nil)
		case StopAndBuffer:
			ctx.BeginRequestBodyBuffer()
			_ = ctx.AppendRequestChunk(chunk)
			ctx.Set("_buffer_from", i)
			s.chain.warnf("[%s] StopAndBuffer in DecodeData -> propagate FlowStopAndBuffer", f.Name())
			if endStream {
				_ = ctx.FinishRequestBodyParse()
			}
			return &Decision{Action: FlowStopAndBuffer}, nil
		}
	}
	return &Decision{Action: FlowContinue}, nil
}

// RunEncodeHeaders runs EncodeHeaders in REVERSE order.
func (s *Session) RunEncodeHeaders(h *HeaderMap, endStream bool) {
	ctx, rs := s.ctx, s.rs
	ctx.ResponseHeaders = h
	ctx.ResponseStarted = true
	ctx.DetectStream()
	for i := len(rs.insts) - 1; i >= 0; i-- {
		st := rs.insts[i].EncodeHeaders(ctx, endStream)
		if st == StopAndReply {
			s.chain.warnf("[%s] StopAndReply ignored after response started", rs.insts[i].Name())
		}
	}
}

// RunEncodeData runs EncodeData in REVERSE order; returns (out, appended).
func (s *Session) RunEncodeData(data []byte, endStream bool) ([]byte, []byte) {
	ctx, rs := s.ctx, s.rs
	if !ctx.IsStream {
		_ = ctx.AppendResponseChunk(data)
		if endStream {
			_, _ = ctx.ResponseJSON()
		}
	}
	out := data
	for i := len(rs.insts) - 1; i >= 0; i-- {
		st := rs.insts[i].EncodeData(ctx, out, endStream)
		switch st {
		case StopAndReply:
			s.chain.warnf("[%s] StopAndReply downgraded -> AppendEncodedData", rs.insts[i].Name())
		case StopAndBuffer:
			s.chain.warnf("[%s] StopAndBuffer in EncodeData unsupported", rs.insts[i].Name())
		}
	}
	// A sub-filter (e.g. async guardrail) may withhold or replace the chunk.
	if replaced, ok := ctx.TakeResponseOverride(); ok {
		out = replaced
	}
	return out, ctx.TakeAppended()
}

// Close releases all sub-filter instances back to the pool. Idempotent.
func (s *Session) Close(reason DestroyReason) {
	if s == nil || s.done {
		return
	}
	s.done = true
	s.chain.releaseAll(s.rs, s.ctx, reason)
}
