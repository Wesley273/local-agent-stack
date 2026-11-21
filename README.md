# local-agent-stack

> 把简历里的四个项目，在本地变成**可编译、可运行、可端到端验证**的真实代码。
> AI 网关（控制面 + 数据面 + 模型网关）、AgentMecha Sidecar、MCP 注册中心、Go/Java/Python 三语言 SDK。

---

## 一句话

协议层**直接引用官方 SDK**（A2A / MCP），网关治理层（过滤器链、SSE 断流修复、限流、优先级调度、
配置热更新、护栏 Hook、OpenAPI→MCP 转换、状态解耦）**自研**——这正是简历里属于你的那部分能力。

---

## 快速开始

```powershell
cd local-agent-stack

# 1) 编译全部模块（含依赖下载）
powershell -ExecutionPolicy Bypass -File scripts\build-all.ps1

# 2) 一键拉起全部服务（按依赖顺序 + /health 等待）
powershell -ExecutionPolicy Bypass -File scripts\start-all.ps1

# 3) 端到端冒烟（契约 §8 的 E01–E26）
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1

# 4) 三语言 SDK 全链路
powershell -ExecutionPolicy Bypass -File scripts\run-e2e.ps1

# 5) 停止
powershell -ExecutionPolicy Bypass -File scripts\stop-all.ps1
```

端口集中在 [`config/ports.env`](config/ports.env)：控制面 9000 / 数据面 8080 / Sidecar 9100 /
示例 Agent 9101-9103 / Mock LLM 9200 / MCP 注册中心 9300。

---

## 目录

| 路径 | 内容 | 语言 | 依赖 |
|---|---|---|---|
| `docs/00-总体规划.md` | 范围、架构、实施顺序、验收 | — | — |
| `docs/01-协议契约.md` | **唯一真相源**（冻结版）：错误码、A2A/MCP、过滤器链、26 项验收 | — | — |
| `docs/03-能力映射矩阵.md` | 简历每一条能力 → 文件 → 用例 | — | — |
| `docs/05-生产映射与差异.md` | 本地实现 vs 华为云生产系统的诚实对照 | — | — |
| `docs/06-依赖与版本.md` | 三语言官方 SDK 精确坐标、实测 API、breaking change | — | — |
| `docs/07-composite-filter-chain.md` | 单插件内责任链：共享解析结果、StopAndBuffer 状态机、Envoy 原生对比 | — | — |
| `pkg/a2a/` | 共享库：协议类型、冒号路径解析、SSE/NDJSON、trace、过滤器链、护栏 Hook、限流 | Go | 标准库 |
| `pkg/composite/` | 单插件内复合责任链：共享上下文、请求/响应一次 buffer+parse、Envoy 语义 decode/encode 顺序、SSE 安全追加帧 | Go | 标准库 |
| `services/control-plane/` | Agent 注册中心、配置快照、CRUD、revision 热更新 | Java 11 | Jackson 2 |
| `services/data-plane/` | 过滤器链、SSE 断流修复、模型网关、配置轮询、admin 端点 | Go | a2a-go/v2 |
| `services/agentmecha/` | 官方 A2A Server/Client + 状态解耦 + 4 类护栏 Hook + NDJSON 64KB 修复 | Go | a2a-go/v2 |
| `services/mcp-registry/` | 官方语义的 MCP 协议 + **OpenAPI→MCP 自动转换（$ref 展开）** + 回放 | Java 11 | Jackson 2 |
| `services/mock-llm/` | OpenAI 兼容模型服务 + `/tools/echo` 回放端点 | Python 3.12 | FastAPI/uvicorn |
| `sdk/go/` | Go SDK（基于 `a2aclient` 二次封装） | Go | a2a-go/v2 |
| `sdk/java/` | Java SDK（基于官方 A2A Java SDK record 类型） | JDK 22 | a2a-java-sdk 1.5.1.Final |
| `sdk/python/` | Python SDK（基于 `a2a-sdk` + `httpx`） | Python 3.12 | a2a-sdk 0.3.26 |
| `examples/*-agent/` | 三语言 User Server（NDJSON，含 128KB 长行） | 三语言 | 各自官方 SDK |
| `scripts/` | build / start / stop / smoke / e2e | PowerShell | — |

---

## 依赖决策（为什么不是"纯标准库"也不是"全用最新版"）

| 层 | 选择 | 依据 |
|---|---|---|
| A2A Go | `github.com/a2aproject/a2a-go/v2 v2.8.0` | 官方，Go 1.26 兼容，探针实跑通过 |
| MCP Go | `github.com/modelcontextprotocol/go-sdk v1.8.0` | 官方 Tier 1 |
| A2A Python | `a2a-sdk[http-server]==0.3.26` | **1.x 删了 `a2a.server.apps`、改 protobuf types**，实测不可用 |
| MCP Python | `mcp==1.30.0` | **2.x 把 FastMCP 改名 MCPServer**，实测 import 抛异常 |
| A2A Java | `org.a2aproject.sdk:*:1.5.1.Final` @ **JDK 22** | 官方 SDK class major=61（Java 17），JDK 11 不可用 |
| 控制面/MCP 注册中心 | JDK 11 + Jackson 2 + `HttpServer` | 业务逻辑自研，拖 Quarkus/CDI 会失真；探针已跑通 |
| JSON (Java) | `com.fasterxml.jackson.core:jackson-databind:2.17.2` | Java 8 字节码，JDK 11 安全 |

> 全部版本与坑见 [`docs/06-依赖与版本.md`](docs/06-依赖与版本.md)。所有结论来自本机实测，不是查文档猜的。

---

## 协议双轨制（重要）

官方 SDK 与我们契约的字段有差异（`SendMessage` vs `message/send`、`TASK_STATE_COMPLETED` vs
`completed`、`SupportedInterfaces` vs `url`）。**不做互相转换**，架构上分开：

- 数据面是**治理层 + 透明反向代理**，不解析 A2A 语义，HTTP 原样透传 → 两套方法名都能过
- Sidecar / SDK 的协议合规由官方 SDK 负责
- 契约里的兼容轨（`/a2a/v1/*` 冒号路径）只用于回归测试与冒号路径验证

详见 [`docs/01-协议契约.md`](docs/01-协议契约.md) §0.0。

---

## 简历踩过的坑 → 代码里的修复

| 坑 | 修复位置 |
|---|---|
| `SendLocalReply` 掐断 SSE（`curl (18)`） | `services/data-plane/filters/`，`pkg/a2a/filter.go` 的 `AppendEncodedData` 降级规则 |
| `bufio.Scanner: token too long`（64KB） | `pkg/a2a/stream.go` `NewNDJSONScanner` 用 `Buffer(64K, 10M)`；E11 用 128KB 单行验证 |
| `/tasks/{id}:cancel` 让 Go `ServeMux` panic | `pkg/a2a/types.go` `ParseRestPath`；官方 SDK 也用 `{idAndAction}` + `CutSuffix` |
| Envoy 插件每请求新建导致 OOM | `pkg/a2a/filter.go` `sync.Pool` + `Reset()`；`/v1/admin/pool` 可观测 |
| 手改 DestinationRule 被 reconcile 覆盖 | 数据面只认控制面 `/v1/config` 快照，5s 轮询 + 304 |
| `getDuration()` null 拆箱 NPE 导致整批 span 插入失败 | `pkg/a2a/trace.go` 数值字段非指针 + `AddBatch` 逐条容错 |
| embedding 超时 / 无独立 timeout | chat 30s / embeddings 90s 独立配置 |

---

## 验收

`docs/01-协议契约.md` §8 定义 26 项端到端用例（E01–E26）。
`scripts/smoke-test.ps1` 覆盖 HTTP 层，`scripts/run-e2e.ps1` 覆盖三语言 SDK 全链路，
各模块内的 `*_test.go` / `SelfTest.java` 覆盖单元层。

---

## 诚实边界

本地不复刻 K8s / Istio / microVM / 企业 IAM / 真实 LLM / MySQL-Redis 集群。
逐项对照与替代理由见 [`docs/05-生产映射与差异.md`](docs/05-生产映射与差异.md)。
