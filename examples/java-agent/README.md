# Java 示例 Agent（User Server / NDJSON）· 端口 9102

基于**官方 A2A Java SDK 1.5.1.Final**（JDK 22）的示例 Agent，同时暴露 Sidecar 需要的 NDJSON 协议。

## 构建 / 启动

```powershell
$env:JAVA_HOME='C:\Java\jdk-22'
mvn -B -f examples\java-agent\pom.xml clean compile
# 运行（classpath 用 dependency:build-classpath 生成）
mvn -B -q -f examples\java-agent\pom.xml dependency:build-classpath '-Dmdep.outputFile=cp.txt'
$cp = (Get-Content examples\java-agent\cp.txt -Raw).Trim()
& 'C:\Java\jdk-22\bin\java.exe' -cp "examples\java-agent\target\classes;$cp" las.example.JavaAgent 9102
```

## 官方 SDK 使用位置（`src/main/java/las/example/JavaAgent.java`）

| 官方类 | 用途 | 行 |
|---|---|---|
| `org.a2aproject.sdk.spec.AgentCard` (+ builder) | `buildCard()` 构造合规卡片，capabilities/supportedInterfaces 非空 | L76–L96 |
| `AgentCapabilities` / `AgentInterface` / `AgentSkill` | 卡片字段 | L77–L94 |
| `org.a2aproject.sdk.spec.Artifact` / `TextPart` / `Part` | `echoArtifact()` 构造回显产物 | L99–L102 |
| `org.a2aproject.sdk.spec.Task` / `TaskStatus` / `TaskState` | `buildTask()`（status 非空） | L105–L112 |
| `org.a2aproject.sdk.A2A.toUserMessage` | 构造用户 Message（history） | L106 |
| Jackson 绑定官方 record | `/.well-known/agent.json`、`agent-card.json`、`/a2a` 响应序列化 | L129 / jsonRpc |

传输用 JDK 标准库 `com.sun.net.httpserver.HttpServer`（classpath 模式直接可用）。

## 端点（与 Go / Python Agent 完全一致）

| 方法 | 路径 | 返回 |
|---|---|---|
| GET | `/health` | `{"status":"UP","service":"java-agent"}` |
| POST | `/task` 或 `/run` | `application/x-ndjson` 流 |
| GET | `/.well-known/agent.json`、`/.well-known/agent-card.json` | 官方 AgentCard 序列化 |
| POST | `/a2a` | 最小 JSON-RPC `message/send`（官方 Task 响应） |

NDJSON 流（三行）：
```
{"type":"status","state":"working","progress":0.5}\n
{"type":"artifact","artifactId":"art-1","text":"<回显内容>"}\n
{"type":"done","state":"completed"}\n
```

- 普通输入：artifact.text = 用户文本原样（UTF-8，中文不乱码）
- `long:N`：artifact.text = N 个 `'A'` 的**单行**，`N=128000` 时单行 >128KB，写时不截断（E11）
- 端点、格式、字段名与 Go(9101) / Python(9103) 三语言完全一致，Sidecar 可无差别挂载

## 坑

- JDK 必须 22（官方 record major 61）；`release 22` + 显式 UTF-8。
- `HttpServer.sendResponseHeaders(200, 0)` 用 chunked，不设 Content-Length，才能流式逐行 flush。
- `long:128000` 用 `char[]` 一次性构造再 UTF-8 写入，绝不在写出前截断。
- `TaskStatus.timestamp()` 是 `OffsetDateTime`，Jackson 默认无 `JavaTimeModule`；`/a2a` 响应按官方 record 的 accessor **逐字段映射**，不用 `valueToTree(task)`（否则报 "Java 8 date/time type not supported"）。
