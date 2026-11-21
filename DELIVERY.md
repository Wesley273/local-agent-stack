# 交付状态 · local-agent-stack

> 最后验证：2026-11-21（Asia/Shanghai）
> 结论：**BUILD ALL PASSED · SMOKE 25/25 PASSED · 三语言 SDK E2E PASSED**

---

## 1. 这是什么

基于个人简历中的四段项目经历，在本地完整实现的、可编译可运行的：

- **AI 网关数据面**（Go）：Envoy 语义过滤器链、SSE 断流修复、模型网关（统一路由/优先级调度/Token 限流/协议转换）、配置热更新、可观测
- **控制面**（Java 11）：Agent 注册中心、配置快照（revision + ETag/304）、CRUD 持久化
- **AgentMecha Sidecar**（Go）：基于官方 `a2a-go/v2` 的 A2A Server/Client、状态/会话/任务/持久化解耦、4 类护栏 + Hook、NDJSON 64KB 修复
- **MCP 注册中心**（Java 11）：OpenAPI → MCP 自动转换（`$ref` 递归展开 / `allOf` 合并 / path/query/body）+ `tools/call` 真实 HTTP 回放
- **三语言 SDK**：Go / Java / Python，基于各自官方 A2A SDK 二次封装
- **Mock LLM**（Python）：OpenAI 兼容 Chat/embeddings + `/tools/echo`

协议合规层**引用官方 SDK**；网关治理层（过滤器链、SSE 修复、限流、热更新、护栏 Hook、OpenAPI 转换、状态解耦）为自研——这正是简历中的差异化能力。

---

## 2. 一键验证

```powershell
cd local-agent-stack

# 1) 编译全部模块（Go + Java 11 + Java 22 + Python venv）
powershell -ExecutionPolicy Bypass -File scripts\build-all.ps1

# 2) 拉起 8 个服务
powershell -ExecutionPolicy Bypass -File scripts\start-all.ps1

# 3) 25 项端到端冒烟（E01–E26 中的 HTTP 层）
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1

# 4) Go / Java / Python 三语言 SDK 全链路 + 冒号路径 + 128KB NDJSON
powershell -ExecutionPolicy Bypass -File scripts\run-e2e.ps1

# 5) 停止
powershell -ExecutionPolicy Bypass -File scripts\stop-all.ps1
```

> Windows PowerShell 5.1 读取 `.ps1` 依赖 UTF-8 BOM；仓库内脚本均已带 BOM，请勿另存为无 BOM 格式。

**实测结果**：
- `build-all.ps1` → `BUILD ALL PASSED`
- `smoke-test.ps1` → `PASS=25 FAIL=0 TOTAL=25`
- `run-e2e.ps1` → `E2E PASSED`（Go/Java/Python 三 SDK 8 步全通）

---

## 3. 依赖版本（已冻结，实测）

| 层 | 依赖 | 版本 | 理由 |
|---|---|---|---|
| A2A Go | `github.com/a2aproject/a2a-go/v2` | 2.8.0 | 官方，Go 1.26 兼容 |
| MCP Go | `github.com/modelcontextprotocol/go-sdk` | 1.8.0 | 官方 Tier 1 |
| A2A Python | `a2a-sdk[http-server]` | 0.3.26 | 1.x 删除 `a2a.server.apps`、改 protobuf types，实测不可用 |
| MCP Python | `mcp` | 1.30.0 | 2.x 把 FastMCP 改名 MCPServer，实测 import 抛异常 |
| A2A Java | `org.a2aproject.sdk:*` | 1.5.1.Final | 官方 SDK class major=61，需 JDK 22 |
| 控制面 / 注册中心 | JDK 11 + Jackson 2.17.2 | — | 业务逻辑自研，不拖 Quarkus/CDI |

> 全部版本与逐项 API 证据见 [`docs/06-依赖与版本.md`](docs/06-依赖与版本.md)。

---

## 4. 简历中的坑 → 代码中的修复（均可复现）

| 踩坑 | 修复位置 | 回归用例 |
|---|---|---|
| `SendLocalReply` 掐断 SSE（`curl (18)`） | `services/data-plane/filters/guardrail.go`（`ResponseStarted` 时降级为 `AppendEncodedData` + `Continue`）；`server.go` 单次 WriteHeader + 逐帧 Flush | E10 |
| `bufio.Scanner: token too long`（64KB） | `pkg/a2a/stream.go` `NewNDJSONScanner`（64K/10M）+ `NDJSONReader.ReadBytes` | E11（128KB 单行） |
| `/tasks/{id}:cancel` 让 Go `ServeMux` panic | `pkg/a2a/types.go` `ParseRestPath`（单前缀 handler + `LastIndex` 切冒号）；官方 SDK 同样用 `{idAndAction}` + `CutSuffix` | E12 |
| Envoy 插件每请求新建实例 OOM | `pkg/a2a/filter.go` `sync.Pool` + `Reset()`；`/v1/admin/pool` 计数 | E22（gets≫new） |
| 手改配置被 reconcile 覆盖 | 数据面不读本地运行时配置，只认控制面 `/v1/config` 快照，5s 轮询 + 304 | E21 |
| `getDuration()` null 拆箱导致整批 span 插入失败 | `pkg/a2a/trace.go` 数值字段非指针 + `AddBatch` 逐条容错 | E23 |
| embedding 超时 / 无独立 timeout | chat 30s / embeddings 90s 独立配置；长文档不走 prompt 护栏 | E15 |
| 官方 SDK 方法名双轨差异（`SendMessage` vs `message/send`） | 数据面透明代理零解析；Python SDK 走兼容轨 `/a2a/v1/*`；数据面自动补 `A2A-Version: 1.0` | E06/E09 + Python SDK E2E |

---

## 5. 文档索引

- [`README.md`](README.md) — 总览与快速开始
- [`docs/00-总体规划.md`](docs/00-总体规划.md) — 范围、架构、实施顺序
- [`docs/01-协议契约.md`](docs/01-协议契约.md) — **唯一真相源**（错误码 / A2A / MCP / 过滤器链 / 26 项验收）
- [`docs/02-架构设计.md`](docs/02-架构设计.md) — 请求生命周期、配置闭环、状态归属
- [`docs/03-能力映射矩阵.md`](docs/03-能力映射矩阵.md) — 简历每条能力 → 文件 → 用例
- [`docs/04-API参考.md`](docs/04-API参考.md) — 全部端点
- [`docs/05-生产映射与差异.md`](docs/05-生产映射与差异.md) — 本地实现 vs 华为云生产系统（诚实边界）
- [`docs/06-依赖与版本.md`](docs/06-依赖与版本.md) — 官方 SDK 精确坐标、实测 API、breaking change

---

## 6. 诚实边界

本地**不复刻**：K8s / Istio / microVM 隔离 / 企业 IAM / 真实 LLM 推理 / MySQL-Redis-ClickHouse 集群 / 卫星调度算法（硕士科研）。
逐项替代理由见 [`docs/05-生产映射与差异.md`](docs/05-生产映射与差异.md)。
