package composite

// SubFilter is one logical plugin inside the composite chain. It sees the same
// Context for the whole request/response, so it can read cached parse results
// produced by earlier filters instead of buffering/parsing again.
type SubFilter interface {
	Name() string

	DecodeHeaders(ctx *Context, endStream bool) Status
	// DecodeData is called with the current chunk. When the chain is buffering,
	// it is called ONCE with the full body after parsing (see Chain.RunDecode).
	DecodeData(ctx *Context, data []byte, endStream bool) Status
	EncodeHeaders(ctx *Context, endStream bool) Status
	EncodeData(ctx *Context, data []byte, endStream bool) Status

	OnDestroy(ctx *Context, reason DestroyReason)
	Reset()
}

// BaseFilter provides no-op defaults so concrete filters override only what
// they need.
type BaseFilter struct{}

func (b *BaseFilter) Name() string                                     { return "base" }
func (b *BaseFilter) DecodeHeaders(c *Context, end bool) Status        { return Continue }
func (b *BaseFilter) DecodeData(c *Context, d []byte, end bool) Status { return Continue }
func (b *BaseFilter) EncodeHeaders(c *Context, end bool) Status        { return Continue }
func (b *BaseFilter) EncodeData(c *Context, d []byte, end bool) Status { return Continue }
func (b *BaseFilter) OnDestroy(c *Context, r DestroyReason)            {}
func (b *BaseFilter) Reset()                                           {}
