# 08 · AI Gateway 插件编排（端到端场景）

> 位置：`pkg/aigateway` + `examples/aigateway-e2e`
> 目标：在 composite 责任链上跑通一个真实 AI 网关的插件时序：
> **请求体解析出 model 供路由 → 安全护栏异步 input/output 检测 → EncodeData 等待检测结果 → 指标 → 按输入/输出 token 限流**。

---

## 1. 场景时序

```
请求:
  DecodeHeaders:
    body-model-parser -> StopAndBuffer（需要完整 body）

  DecodeData(chunk1): 累积，不解析
  DecodeData(chunk2, end=true):
    chain 解析 body 一次 -> ctx.ParsedBody()
    body-model-parser.DecodeData: 提取 model、估算 prompt token、初始化 metrics、
                                  启动异步 INPUT 检测 (Future)
    token-limiter.DecodeData:     预扣 prompt token（超限 -> RATE_LIMITED）
    route-by-model.DecodeData:    读 ctx.model -> 选 backend -> ctx + 响应头

响应:
  EncodeHeaders:
    metrics: 标记 UPSTREAM
    async-guardrail: 设定 fail-open/closed 策略

  EncodeData(chunk, ...):
    async-guardrail (第一次调用):
      启动异步 OUTPUT 检测 (Future) —— 此时才有响应文本
      Wait(input Future, timeout) + Wait(output Future, timeout)
      ├─ 都 PASS          -> 放行 chunk
      ├─ 任一 REJECT      -> 流式: 清空该 chunk + 追加 SSE error+[DONE]
      │                      非流式: 用错误信封替换 body
      └─ 超时             -> fail-open 放行 / fail-closed 按拒绝处理
    token-limiter: 用 usage 的 prompt/completion 结算 token
    metrics: OnDestroy 落指标
```

**要点**：异步护栏在 decode 阶段发起 input 检测（尽早并行），在 `EncodeData` 才"等结果再放行"；output 检测因为有响应文本，在 `EncodeData` 第一次调用时发起并等待。门闩用 `sync` 语义只执行一次（后续 chunk 不再重复等待）。

---

## 2. 为什么这样设计

1. **一次解析**：`StopAndBuffer` 后 chain 只 parse 一次，发布到 `ctx`；model 抽取、token 估算、护栏文本、路由全部读缓存，不重复 parse。
2. **异步不等白工**：input 检测在请求体一解析完就发起，和上游调用并行；到 EncodeData 时通常已完成，等待几乎零成本（示例里 gateWait≈30ms 就是检测延迟）。
3. **EncodeData 门闩只一次**：`KGateDone` 保证后续 chunk 不再等待，避免每个 chunk 都阻塞。
4. **流式安全**：拒绝时通过 `Context.ReplaceResponseData([]byte{})` 扣住违规 chunk，只追加 `event: error` + `[DONE]`，**绝不 `SendLocalReply`、绝不写第二个状态行、绝不 RST**——客户端不会先看到违规内容。
5. **token 口径统一**：预扣用离线估算，结算时若后端返回 OpenAI `usage`，用权威数字覆盖（示例里 3/2/5）。

---

## 3. 组件清单

| 文件 | 内容 |
|---|---|
| `future.go` | `Future[T]`（Resolve/Fail/Get/Wait(timeout)）+ `GuardVerdict` |
| `context_ext.go` | typed keys、GateVerdict、MetricSnapshot、token 估算 |
| `plugins.go` | 5 个插件 + Checker 接口 + FixedWindowLimiter |
| `pipeline.go` | `BuildChain(cfg)` 组装（共享 limiter） |
| `pipeline_test.go` | 7 个端到端测试 |
| `examples/aigateway-e2e/main.go` | 可运行 demo |

### 五个插件

| 插件 | Decode | Encode | 职责 |
|---|---|---|---|
| `body-model-parser` | StopAndBuffer → 解析后提取 model、估算 token、启动 input Future | — | 一次解析 + 路由输入 |
| `token-limiter` | 预扣 prompt token，超限 429 | 结算 completion token | 按输入/输出 token 限流 |
| `route-by-model` | 读 model 选 backend | 写 `X-Routed-Backend` 头 | 请求体驱动路由 |
| `async-guardrail` | — | 启动 output Future + EncodeData 等待两 Future | 异步护栏门闩 |
| `metrics` | — | EncodeHeaders 标记，OnDestroy 落指标 | 可观测 |

---

## 4. 可插拔点

- **AsyncGuardChecker**：`BlockingChecker(delay, blockedWords)` 是 demo 实现；生产替换成调用审核服务的实现即可（`Check(direction, text) GuardVerdict`）。
- **TokenLimiter**：`FixedWindowLimiter` 是进程内 demo；换成 Redis 令牌桶只需实现 `Reserve/Settle`。
- **MetricsSink**：`Record(MetricSnapshot)` 可接到 Prometheus/ClickHouse。
- **Routes**：`map[model]backend`，支持 fallback。
- **超时策略**：`GuardTimeout` + `GuardFailOpen`（fail-open/closed）。

---

## 5. 验证

```powershell
cd local-agent-stack
go test ./pkg/aigateway ./pkg/composite -count=1 -v
go run ./examples/aigateway-e2e
```

最近一次结果：

- `go test ./...` 全绿（aigateway 7 + composite 8 + 原有全部）
- `go run ./examples/aigateway-e2e` 三个场景全部正确：
  - happy：`allowed=true`，token 3/2/5，status=normal
  - input-blocked：gate 拒绝，chunk 被错误信封替换，metrics reject=1
  - stream-output-blocked：违规 chunk 扣住，只发 `event: error` + `[DONE]`，连接不中断

---

## 6. 与生产的映射

| 本实现 | 生产（华为云 AgentRun / 典型大模型网关） |
|---|---|
| `Future[T]` 异步门闩 | gRPC 并发调用内容安全服务（输入/输出检测） |
| `ReplaceResponseData` | Envoy `addEncodedData` / 流改写 |
| `TokenState` | Redis 令牌桶 + 精准 tokenizer |
| `MetricSnapshot` | Prometheus Histogram/Gauge → ClickHouse |
| `RouteByModel` | 语义路由 / 模型别名 / 多后端灰度 |
| `composite.Chain` | Envoy Golang filter 内的自研责任链 |

注意：本包是纯 Go 逻辑层，不绑定具体 Envoy Go filter API；接入时把 `composite.HeaderMap` / `BufferInstance` / `DestroyReason` 做薄适配即可。Envoy 版本相关的具体 buffer/filter-state 方法名请以实际源码为准。
