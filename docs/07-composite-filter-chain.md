# 07 · Composite Filter Chain（单插件内责任链）

> 位置：`pkg/composite`
> 目标：在一个 Envoy Golang HTTP filter 内注册多个 sub-filter，模拟 Envoy 原生 filter chain 语义，
> 同时解决原生 chain「每个 filter 只看到 bytes」导致的重复 buffer / 重复 JSON parse / context 难共享问题。

---

## 1. 为什么需要它

Envoy 原生 filter chain 的数据模型：

```text
filter A --(headers/body BYTES)--> filter B --(headers/body BYTES)--> filter C
```

filter 之间传递的是 **bytes**，不是「A 解析好的 Go map」。所以：

- A 为了检查 JSON，buffer 一次 + parse 一次
- B 也需要 JSON，再 buffer 一次 + parse 一次
- C 同样再来一遍
- A 想告诉 B「appId=app-demo」，只能塞 header / filter state / metadata

这对 AI 网关尤其浪费：鉴权、限流、输入护栏、协议转换、输出护栏、可观测，几乎都要看同一份 JSON。

Envoy 原生有 `FilterState` / `DynamicMetadata` / `Header` 这些旁路，但：

- FilterState 放复杂对象取决于版本 API，Go filter 支持有限（具体方法名请按你实际 Envoy 版本核实）
- Metadata 是结构化但偏扁平的值，复杂结构要序列化 → 又是一次 parse
- Header 只适合小标量

> Envoy 官方也有 `envoy.filters.http.composite`，但它是「matcher 驱动的 filter 动态组合/异步恢复」，**不是**你要的「一个 filter 内部多个 sub-filter 共享上下文」。

因此：**把多个自研 Go 插件合并成一个 composite filter，内部自己做责任链**，是更合适的工程解。

---

## 2. 语义：与 Envoy 对齐的部分

假设注册顺序：

```go
chain.Register("A", ...)
chain.Register("B", ...)
chain.Register("C", ...)
```

### 请求方向（decode，正序）

```text
A.DecodeHeaders → B.DecodeHeaders → C.DecodeHeaders
A.DecodeData    → B.DecodeData    → C.DecodeData   // 每 chunk
```

### 响应方向（encode，反序）

```text
C.EncodeHeaders → B.EncodeHeaders → A.EncodeHeaders
C.EncodeData    → B.EncodeData    → A.EncodeData   // 每 chunk
```

### Local reply

- 响应开始前：某 filter 返回 `StopAndReply` → 立即生成错误响应，后续 filter 不执行
- 响应开始后（`ctx.ResponseStarted == true`）：`StopAndReply` 被忽略并降级为 `AppendEncodedData`，**绝不写第二个状态行**

这就是简历里「Envoy `SendLocalReply` 掐断 SSE」问题的正确修复。

### 实例生命周期

`Session` 绑定一个请求/响应流：

```go
s := chain.NewSession(ctx)   // 一次性 acquire 全部 sub-filter
s.RunDecodeHeaders(...)
s.RunDecodeData(...)
s.RunEncodeHeaders(...)
s.RunEncodeData(...)
s.Close(ReasonNormal)        // 一次性 OnDestroy + 归还 pool
```

同一个 filter 实例贯穿该 stream 的所有 phase —— 与 Envoy 原生「每 stream 一个 filter 实例」一致。
这点很重要：如果每个 phase 都重新 acquire，存状态的 filter 会拿到不同实例，产生难以定位的 bug。

---

## 3. 核心增量：请求体只 buffer / parse 一次

### 状态机

```text
BodyNone ──StopAndBuffer──> BodyCollecting ──endStream──> BodyParsed
```

- `BodyNone`：没有 filter 需要完整 body
- `BodyCollecting`：某 filter 在 `DecodeHeaders`（或 `DecodeData`）返回 `StopAndBuffer`，chain 开始累积 bytes，后续 filter 的 decode 暂停
- `BodyParsed`：收到最后一块后，chain 调用 `ctx.FinishRequestBodyParse()` **一次**，把结果发布到：
  - `ctx.ParsedBody()` / `ctx.ParsedBodyMap()`
  - `ctx.Get("parsedBody")`

### 下游 filter 怎么消费

下游 filter **不再碰 bytes**，直接读缓存：

```go
func (f *GuardrailFilter) DecodeData(ctx *Context, data []byte, end bool) Status {
    m := ctx.ParsedBodyMap()          // 零成本，永不重新 parse
    prompt, _ := m["prompt"].(string)
    if prompt == "BLOCK_ME" {
        ctx.Reject("GUARDRAIL_REJECTED", "blocked word")
        return StopAndReply
    }
    return Continue
}
```

前置 filter 可以把派生结果写进共享 `ctx.Values`：

```go
ctx.Set("appId", "app-demo")
ctx.Set("agentId", "echo-agent")
```

后续 filter 用 `ctx.GetString("appId")` 读取，不需要 header/metadata 往返。

### 一次真正的请求流

```text
1. A.DecodeHeaders → Continue
2. B.DecodeHeaders → StopAndBuffer     // B 需要完整 JSON
   chain: ctx.BeginRequestBodyBuffer(), 暂停 C
3. chunk1: chain.AppendRequestChunk(chunk1), 不 parse
4. chunk2(end=true): chain.AppendRequestChunk(chunk2)
   → FinishRequestBodyParse() 仅 1 次
   → ctx.BodyState = BodyParsed
   → ctx.Set("parsedBody", map)
5. B.DecodeData(ctx, fullBody, true): m := ctx.ParsedBodyMap()
6. C.DecodeData(ctx, fullBody, true): m := ctx.ParsedBodyMap()  // 同一对象
```

关键点：B 和 C 看到的 `parsedBody` 是**同一个缓存对象**，不是各自重新 parse。测试
`TestStopAndBufferParseOnceAndShare` 断言 `parseCount == 1`、`bufferChunks == 2`，且下游能读到前置 filter 写入的派生字段。

### 非 JSON body

`FinishRequestBodyParse` 不会因非 JSON 失败；它把原文发布成：

```json
{"raw": "<原始 body>"}
```

下游仍可用 `ctx.ParsedBodyMap()["raw"]` 拿到内容，不需要再读 `ctx.Body`。

---

## 4. 响应体 buffer：只针对非流式

### 判断时机

`EncodeHeaders` 阶段就要决定：

```go
ctx.DetectStream()
```

依据：

1. `ctx.Values["is_stream"] == true`（业务侧最可靠，来自请求 `stream:true`）
2. `Content-Type` 以 `text/event-stream` 开头或包含 `application/x-ndjson`

### 非流式

```text
EncodeData(chunk1) → ctx.AppendResponseChunk
EncodeData(chunk2, end=true) → ctx.AppendResponseChunk → ctx.ResponseJSON() 解析一次
```

下游 filter 和调用方都可以通过 `ctx.ResponseJSON()` / `ctx.ParsedResponse()` 读同一份结果。

### 流式

- **不 buffer**：`ctx.ResponseBody` 保持为空
- `EncodeData` 逐块检查
- 命中护栏：

```go
ctx.AppendEncodedData([]byte(
    "event: error\ndata: {\"error\":{\"code\":\"GUARDRAIL_REJECTED\"}}\n\ndata: [DONE]\n\n",
))
return Continue // 绝不 StopAndReply / 不关连接 / 不写第二个状态
```

这保证 HTTP 状态仍是 200、SSE 流正常结束、客户端不会报 `curl: (18) transfer closed`。

---

## 5. 文件与 API

| 文件 | 内容 |
|---|---|
| `context.go` | `Context`、HeaderMap、请求 buffer 状态机、`ParsedBody()`、响应 buffer、`AppendEncodedData`、pool reset |
| `filter.go` | `SubFilter` 接口 + `BaseFilter` 默认实现 |
| `chain.go` | `Chain`（注册、pool stats、makeReply）+ 兼容旧式便捷方法 |
| `session.go` | `Session`：绑定一个 stream 的 filter 实例，提供 5 个 phase 方法与 `Close` |
| `chain_test.go` | 单测：顺序、buffer/parse 一次并共享、外层 Decision 序列（StopAndBuffer→Ready+ReleaseBody）、缓存对象一致性、local reply 短路、流式追加帧、非流式响应解析、响应改写、pool 复用 |
| `adapter.md` | **外层 Decision → Envoy Golang filter 的映射**（StopAndBuffer 如何传播到 Envoy、ReleaseBody 如何注入 upstream） |

### 外层 StopAndBuffer 传播（关键）

内层 sub-filter 的 `StopAndBuffer` 必须传播到外层 Envoy，否则 Envoy 会继续把 body 发往 upstream，内层 buffer 就白攒了。
`Session.RunDecodeHeaders/RunDecodeData` 返回 `*Decision`，其 `Action` 映射为 Envoy 状态：

| Decision.Action | Envoy 返回值 |
|---|---|
| `FlowContinue` | `api.Continue` |
| `FlowStopAndBuffer` | `api.StopAndBuffer`（停止 decode，body 不外发） |
| `FlowStopAndReply` | `api.LocalReply` |
| `FlowReady` | `api.Continue` + 将 `ReleaseBody` 写回 buffer 后再放行 |

详见 [`pkg/composite/adapter.md`](../pkg/composite/adapter.md)。

主要类型：

```go
type Chain struct { ... }
func NewChain() *Chain
func (c *Chain) Register(name string, f Factory) *Chain
func (c *Chain) NewSession(ctx *Context) *Session
func (c *Chain) PoolStats() PoolStats

type Session struct { ... }
func (s *Session) RunDecodeHeaders(endStream bool) (*Reply, error)
func (s *Session) RunDecodeData(chunk []byte, endStream bool) (*Reply, error)
func (s *Session) RunEncodeHeaders(h *HeaderMap, endStream bool)
func (s *Session) RunEncodeData(data []byte, endStream bool) (out []byte, appended []byte)
func (s *Session) Close(reason DestroyReason)

type Context struct { ... }
func (c *Context) ParsedBody() any
func (c *Context) ParsedBodyMap() map[string]any
func (c *Context) BodyParsed() bool
func (c *Context) Set(k string, v any)
func (c *Context) GetString(k string) string
func (c *Context) AppendEncodedData(b []byte)
func (c *Context) Stats() map[string]any // parseCount / bufferChunks / bodyState
```

---

## 6. 与 Envoy 原生的取舍

| 维度 | Envoy 原生多 filter | Composite（本实现） |
|---|---|---|
| 顺序 | `http_filters` 数组静态控制 | `chain.Register` 顺序 |
| decode/encode 方向 | 正序 / 反序 | 正序 / 反序（一致） |
| 请求 body 共享 | 只传 bytes | 一次 buffer + 一次 parse，ctx 共享 |
| 响应 body 共享 | 只传 bytes | 非流式一次 parse，ctx 共享；流式不 buffer |
| 状态传递 | header / filter state / metadata | `ctx.Values` 直接读写 |
| 单 filter 独立配置/灰度 | ✅ | 需自己做 |
| 官方生态（ext_authz/ratelimit/lua） | ✅ | 需在外层混用 |
| SSE 安全 | 取决于 filter 实现 | 统一强制降级为 AppendEncodedData |

**推荐部署方式（混合）**：

```yaml
http_filters:
  - name: envoy.filters.http.ext_authz      # 只看 header，原生
  - name: envoy.filters.http.ratelimit      # 只看 header/meta，原生
  - name: envoy.filters.http.golang
    typed_config:
      plugin_name: ai_gateway_composite     # 所有需要 JSON 的自研逻辑
  - name: envoy.filters.http.router
```

---

## 7. 验证

```powershell
cd local-agent-stack
go test ./pkg/composite -count=1 -v
go build ./...
```

最近一次结果：7 个测试全部 PASS，`go build ./...` 成功。

---

## 8. 注意事项

1. `Session.Close` 必须在响应结束后调用（如 `OnDestroy`），否则 filter 实例不会归还 pool。
2. `MaxBody` / `MaxResponse` 默认 8MB，超限返回 `ErrBodyTooLarge` → `INVALID_REQUEST`；
   生产按业务调整。
3. `DetectStream` 在 `EncodeHeaders` 里调用；如果业务知道是流，优先 `ctx.Set("is_stream", true)`。
4. 非流式响应才 `ResponseJSON()`；流式调用会返回错误。
5. 本包是**纯 Go 的逻辑层**，不绑定具体 Envoy Go filter API 版本；接入时把 `HeaderMap` / `BufferInstance` / `DestroyReason` 做薄适配即可。
6. 关于 Envoy 版本相关的具体方法名（Go filter 的 buffer filter state API），请以你实际使用的 Envoy 版本源码为准；本包只实现语义。
