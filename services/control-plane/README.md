# control-plane（控制面 / Agent 注册中心，Java 11 :9000）

`local-agent-stack` 的控制面。职责：维护 Agent / 策略 / 授权 / 模型 / 限流 / 护栏 / Webhook 的
全量配置快照，供数据面（Go :8080）5s 轮询热更新（契约 §3.5 / E21）。

## 技术选型（有依据，不是偷懒）

官方 A2A / MCP Java SDK 在 JDK 11 下不可用（class major=61 / Java 17 + `Record`），实测见
`docs/06-依赖与版本.md` §3。因此本服务用 **Jackson 2.17.2 + JDK 内置
`com.sun.net.httpserver.HttpServer`** 自行实现 REST / JSON-RPC 层。探针
`_probe/java/.../JsonRpcServer.java` 作为 `JsonRpc.java` 的骨架基础。

**编码**：platform 默认 GBK，所有点显式 UTF-8 —— pom `sourceEncoding` + compiler
`<encoding>UTF-8</encoding>` + `new String(bytes, StandardCharsets.UTF_8)` +
响应头 `application/json; charset=UTF-8`。

## 编译 / 运行

```powershell
$env:JAVA_HOME = 'C:\Java\jdk-11'
mvn -B clean compile                                   # 编译
mvn -q exec:java                                       # 运行（默认 9000）

# 或用 java -cp 直接跑（classpath 含 3 个 jackson jar）
java -cp "target\classes;<jackson-databind-2.17.2.jar>;<jackson-core-2.17.2.jar>;<jackson-annotations-2.17.2.jar>" las.cp.Main 9000
# jar 默认在 C:\Users\<you>\.m2\repository\com\fasterxml\jackson\core\...
# Main 第二个参数可指定数据文件：las.cp.Main 9000 data\control-plane.json
```

自测（main-based，不引 JUnit）：

```powershell
mvn -q test-compile
java -cp "target\classes;target\test-classes;<3个jackson jar>" las.cp.SelfTest
# 断言：默认快照逐字段对齐 §4 / 写操作 revision++ / ETag(304) / 重启恢复 / CRUD
```

## 端点

| Method | Path | 说明 |
|---|---|---|
| GET | `/health` | `{"status":"UP","service":"control-plane","version":"1.0.0","revision":N,"now":...}` |
| GET | `/v1/config` | **全量快照**（§4）。响应头 `ETag: <revision>`；带 `If-None-Match: <revision>` 未变 → **304 空 body** |
| GET | `/v1/events` | 最近变更事件 + 当前 revision（E21 人工核对） |
| GET/POST | `/v1/agents` | 列表 / 创建 |
| GET/PUT/PATCH/DELETE | `/v1/agents/{id}` | 单查 / 全量替换 / 局部更新 / 删除 |
| GET | `/v1/agents/{id}/skills` | skills |
| POST | `/v1/agents/{id}/enable` | `{"enabled":true/false}` |
| GET/PUT/POST | `/v1/skills` | `{"agentId":"...","skills":[...]}` |
| GET/POST | `/v1/policies` | 列表 / 创建 |
| GET/PUT/DELETE | `/v1/policies/{id}` | 策略 CRUD |
| PUT | `/v1/policies/{id}/keys` | `{"key":"demo-key","appId":"app-demo"}` |
| GET/POST/PUT/DELETE | `/v1/grants` | 授权（DELETE 用 `?appId=&agentId=`） |
| GET/POST/PUT | `/v1/models` | 模型（PUT 可整体替换） |
| GET/PUT/DELETE | `/v1/models/{name}` | 单模型 |
| GET/PUT | `/v1/ratelimits` | 限流（PUT 可整体替换） |
| GET/PUT/DELETE | `/v1/ratelimits/{key}` | 单条限流（E21 改 `app-demo` 用这个） |
| GET/PUT | `/v1/guardrails` | 护栏 |
| GET/POST/DELETE | `/v1/webhooks` | 全局 Webhook（独立文件，不污染 §4 快照） |
| POST | `/rpc` | JSON-RPC 2.0 管理快捷方式（`cp.config` / `cp.agents` / `cp.bump` 等） |

## 默认快照（逐字段对齐契约 §4）

`revision>=1`，`agents` 含 `echo-agent`/`java-agent`/`python-agent`
（url → `http://127.0.0.1:9100/a2a/{id}`，metadata.userServer → 9101/9102/9103），
`policies.default.keys`（demo-key→app-demo、admin-key→app-admin）、`grants`、
`models`（mock-1/bge-m3 → 9200）、`rateLimits`（default + app-demo）、
`guardrails`（BLOCK_ME / secret-leak / blockedTools / maxPromptChars 8000）。

## 关键语义

- **任何写操作**（POST/PUT/PATCH/DELETE）→ `revision++` + `generatedAt` 更新 +
  原子落盘 `data/control-plane.json`（临时文件 + ATOMIC_MOVE）+ 追加一条变更事件。
- `GET /v1/config` 的 `ETag` 始终等于当前 revision；数据面带 `If-None-Match` 轮询，
  revision 变化才热替换（E21）。
- **统一错误信封**（§0.3）：`{"error":{"code","message","requestId","httpStatus","detail"}}`，
  code 只用契约表里的值（`INVALID_REQUEST` / `AGENT_NOT_FOUND` / `MODEL_NOT_FOUND` /
  `INTERNAL_ERROR` 等），不自造。
- 持久化文件重启后恢复所有 CRUD 结果与 revision。

## 文件

```
Main.java          启动 9000 / 注册 context
JsonRpc.java       JSON-RPC 2.0 骨架（复用探针：批量/通知204/-32700/-32601）
HttpJson.java      UTF-8 HTTP 工具 + 统一错误信封
Store.java         JSON 文件持久化 + 内存索引 + 原子写 + 事件
Snapshot.java      §4 全量快照模型 + revision 管理 + 默认值
AgentRegistry.java agents CRUD / 版本 / skills / 元数据 / enabled
PolicyRegistry.java policies(keys) / grants / webhooks
ModelRegistry.java models[] / rateLimits / guardrails
Events.java        变更事件 + revision 广播视图
ConfigApi.java     GET /v1/config (ETag / If-None-Match 304)
CrudApi.java       所有 /v1/* CRUD 路由
Health.java        GET /health
SelfTest.java      main-based 自测（28 断言）
```
