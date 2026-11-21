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

// RunDecodeHeaders runs DecodeHeaders forward.
func (s *Session) RunDecodeHeaders(endStream bool) (*Reply, error) {
	ctx, rs := s.ctx, s.rs
	for i, f := range rs.insts {
		st := f.DecodeHeaders(ctx, endStream)
		switch st {
		case StopAndReply:
			return s.chain.makeReply(ctx), nil
		case StopAndBuffer:
			ctx.BeginRequestBodyBuffer()
			ctx.Set("_buffer_from", i)
			s.chain.warnf("[%s] StopAndBuffer -> buffer+parse once, publish to ctx", f.Name())
			return nil, nil
		}
	}
	return nil, nil
}

// RunDecodeData feeds request bytes. Handles the buffer/parse-once state machine.
func (s *Session) RunDecodeData(chunk []byte, endStream bool) (*Reply, error) {
	ctx, rs := s.ctx, s.rs

	if ctx.BodyState == BodyCollecting {
		if err := ctx.AppendRequestChunk(chunk); err != nil {
			ctx.Reject("INVALID_REQUEST", err.Error())
			return s.chain.makeReply(ctx), err
		}
		if !endStream {
			return nil, nil
		}
		if err := ctx.FinishRequestBodyParse(); err != nil {
			ctx.Reject("INVALID_REQUEST", err.Error())
			return s.chain.makeReply(ctx), err
		}
		ctx.Set("_buffer_released", true)
		from := 0
		if v, ok := ctx.Get("_buffer_from").(int); ok {
			from = v
		}
		for i := from; i < len(rs.insts); i++ {
			st := rs.insts[i].DecodeData(ctx, ctx.Body, true)
			if st == StopAndReply {
				return s.chain.makeReply(ctx), nil
			}
		}
		return nil, nil
	}

	for i, f := range rs.insts {
		st := f.DecodeData(ctx, chunk, endStream)
		switch st {
		case StopAndReply:
			return s.chain.makeReply(ctx), nil
		case StopAndBuffer:
			ctx.BeginRequestBodyBuffer()
			_ = ctx.AppendRequestChunk(chunk)
			ctx.Set("_buffer_from", i)
			s.chain.warnf("[%s] StopAndBuffer in DecodeData -> collecting", f.Name())
			if endStream {
				_ = ctx.FinishRequestBodyParse()
			}
		}
	}
	return nil, nil
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
