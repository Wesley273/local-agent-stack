# Composite Decision → Envoy Golang filter 适配（关键：外层 StopAndBuffer）

## 问题（你指出的）

把所有 sub-filter 合进一个插件后，**内层** sub-filter 的 `StopAndBuffer` 只在 composite 内部生效。
如果 composite 这个**外层** filter 不告诉 Envoy「我要 buffer」，Envoy 会继续把 body 发给后续 filter / upstream —— 内层攒的 body 毫无意义，请求早就发出去了。

所以必须把内层状态**传播到外层**。`pkg/composite` 现在通过 `Decision` 做这件事。

## Decision → Envoy 状态

`Session.RunDecodeHeaders/RunDecodeData` 现在返回 `*Decision`：

```go
type FlowAction int
const (
    FlowContinue      // Envoy: Continue
    FlowStopAndBuffer // Envoy: StopIterationAndBuffer（还在攒 body）
    FlowStopAndReply  // local reply（Decision.Reply 带状态码/body）
    FlowReady         // body 攒完+解析完+下游跑完 → Envoy: Continue（用 ReleaseBody）
)
```

映射表：

| Decision.Action | Envoy Golang filter 应返回 | 说明 |
|---|---|---|
| `FlowContinue` | `api.Continue` | 正常继续 |
| `FlowStopAndBuffer` | `api.StopAndBuffer` / `StopIterationAndBuffer` | **别把 body 往下发** |
| `FlowStopAndReply` | `api.LocalReply(status, body)` | 直接本地回包 |
| `FlowReady` | `api.Continue`，并把 `Decision.ReleaseBody` 注入请求 | 用攒好的完整 body（可能已被改写）继续 |

## 适配器伪代码

下面的 API 名（`api.ResultAction`、`BufferInstance.SetBytes` 等）是语义示意，
**具体方法名请按你实际使用的 Envoy 版本**（`envoyproxy/envoy` contrib/golang filter）核实。

```go
type httpFilter struct {
    chain   *composite.Chain
    session *composite.Session
}

// DecodeHeaders: 内层 StopAndBuffer 必须原样传给 Envoy。
func (f *httpFilter) DecodeHeaders(h api.HeaderMap, endStream bool) api.ResultAction {
    ctx := composite.NewContext(methodOf(h), pathOf(h), queryOf(h), newReqID(), toHeaderMap(h))
    f.session = f.chain.NewSession(ctx)

    d, _ := f.session.RunDecodeHeaders(endStream)
    switch d.Action {
    case composite.FlowStopAndReply:
        return f.localReply(d.Reply)
    case composite.FlowStopAndBuffer:
        return api.StopAndBuffer        // ★ 关键：告诉 Envoy 停止 decode
    default:
        return api.Continue
    }
}

// DecodeData: 每个 chunk 都要按 Decision 决定是否继续往下发。
func (f *httpFilter) DecodeData(buf api.BufferInstance, endStream bool) api.ResultAction {
    d, _ := f.session.RunDecodeData(buf.Bytes(), endStream)
    switch d.Action {
    case composite.FlowStopAndReply:
        return f.localReply(d.Reply)
    case composite.FlowStopAndBuffer:
        // 还在攒：不要 inject，不要把 buf 往下发。
        return api.StopAndBuffer
    case composite.FlowReady:
        // 攒完且解析完：用 composite 发布的完整 body 替换本次 buffer，
        // 然后放行，让 upstream 收到完整（可能已改写）的 body。
        if d.ReleaseBody != nil {
            buf.SetBytes(d.ReleaseBody)   // 具体 API 名按版本核实
        }
        return api.Continue
    default:
        return api.Continue
    }
}

// EncodeHeaders / EncodeData: 响应侧，注意 ResponseStarted 后禁止 LocalReply。
func (f *httpFilter) EncodeHeaders(h api.HeaderMap, endStream bool) api.ResultAction {
    f.session.RunEncodeHeaders(toHeaderMap(h), endStream)
    return api.Continue
}

func (f *httpFilter) EncodeData(buf api.BufferInstance, endStream bool) api.ResultAction {
    out, appended := f.session.RunEncodeData(buf.Bytes(), endStream)
    if len(out) == 0 {
        buf.Drain(buf.Len())              // 扣住被拒绝的 chunk
    } else {
        buf.SetBytes(out)
    }
    if len(appended) > 0 {
        buf.Append(appended)              // SSE error + [DONE]
    }
    return api.Continue                   // 永远不 LocalReply
}

func (f *httpFilter) OnDestroy(reason api.DestroyReason) {
    f.session.Close(toReason(reason))
}
```

## 为什么 FlowReady 要注入 ReleaseBody

composite 把 body 攒在 `ctx.Body`，Envoy 并不知道这段数据（它没有继续往下发）。
到 `FlowReady` 时必须把 `ctx.Body` 写回当前 buffer 再 `Continue`，否则 upstream 收到的是空 body 或被截断的 body。

这就是「内层 buffer + 外层注入」的完整闭环：

```text
Envoy DecodeData(chunk1) -> composite 攒着 -> 返回 StopAndBuffer（Envoy 停）
Envoy DecodeData(chunk2) -> composite 攒着 -> 返回 StopAndBuffer（Envoy 停）
Envoy DecodeData(chunk3,end) -> composite 解析+下游跑完 -> 返回 Ready + ReleaseBody
                              -> adapter 把 ReleaseBody 写回 buffer -> Continue
                              -> upstream 收到完整 body
```

## 验证

`pkg/composite` 的 `TestStopAndBufferParseOnceAndShare` 已断言外层 decision 序列：

```text
RunDecodeHeaders        -> FlowStopAndBuffer
RunDecodeData(chunk1)   -> FlowStopAndBuffer
RunDecodeData(chunk2,end) -> FlowReady + ReleaseBody(完整 body)
```

`examples/aigateway-e2e` 运行时也会打印这个序列（`[outer] DH/DD(chunk1)/DD(chunk2,end) decision=...`）。
