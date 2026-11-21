# las-a2a-java-sdk · 基于官方 A2A Java SDK 的二次封装

契约 §7 对齐的 Java SDK。**JDK 22**（官方 SDK 1.5.1.Final 的 `AgentCard`/`Task`/`Message`/`AgentSkill`
都是 `java.lang.Record`，class major 61，JDK 11 无法编译或加载）。

## 构建（JAVA_HOME 必须指向 JDK 22）

```powershell
$env:JAVA_HOME='C:\Java\jdk-22'
mvn -B -f sdk\java\pom.xml clean compile
```

`pom.xml` 关键配置：

```xml
<properties>
  <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
  <maven.compiler.release>22</maven.compiler.release>
</properties>
<dependencyManagement><dependencies><dependency>
  <groupId>org.a2aproject.sdk</groupId><artifactId>a2a-java-sdk-bom</artifactId>
  <version>1.5.1.Final</version><type>pom</type><scope>import</scope>
</dependency></dependencies></dependencyManagement>
<dependencies>
  <dependency><groupId>org.a2aproject.sdk</groupId><artifactId>a2a-java-sdk-spec</artifactId></dependency>
  <dependency><groupId>org.a2aproject.sdk</groupId><artifactId>a2a-java-sdk-client</artifactId></dependency>
  <dependency><groupId>com.fasterxml.jackson.core</groupId><artifactId>jackson-databind</artifactId><version>2.17.2</version></dependency>
</dependencies>
```

`a2a-java-sdk-client:1.5.1.Final` 已用 `mvn dependency:get` 查证真实存在（见下方「坑」）。

## 官方 SDK 使用位置

| 官方类 | 用途 | 位置 |
|---|---|---|
| `org.a2aproject.sdk.spec.AgentCard` (+ `.builder()`) | 发现卡片反序列化/兜底构造，supportedInterfaces 强制非空 | `A2AClient.java` getAgentCard/fallbackCard/parseAgentCard |
| `org.a2aproject.sdk.spec.AgentSkill` / `AgentInterface` | 卡片 skills / supportedInterfaces | `A2AClient.java` |
| `org.a2aproject.sdk.spec.Task` (+ `.builder()`) | sendMessage/getTask/cancelTask 结果类型，status 强制非空 | `A2AClient.java` toTask |
| `org.a2aproject.sdk.spec.TaskStatus` / `TaskState` | 任务状态枚举映射（TASK_STATE_COMPLETED 等） | `A2AClient.java` mapState/toTask |
| `org.a2aproject.sdk.spec.Message` / `TextPart` / `Artifact` / `Part` | 消息与产物构造/序列化 | `A2AClient.java` messageToJson/toTask、`Example.java` |
| `org.a2aproject.sdk.A2A.toUserMessage` | 官方客户端门面构造用户消息 | `A2AClient.textMessage` |

传输层：`java.net.http.HttpClient`（JDK 标准库），把官方 record 经 Jackson 序列化到 HTTP。
不引入 Quarkus/CDI/Vert.x，纯 classpath 可运行。

## 统一接口（契约 §7）

| 能力 | 方法 |
|---|---|
| 构造 | `new A2AClient.Builder().baseUrl(...).apiKey(...).build()` |
| 发现 | `getAgentCard()` |
| 发消息 | `sendMessage(Message)` |
| 流式 | `streamMessage(Message, Consumer<Event>)` |
| 查/取消/订阅 | `getTask(String)` / `cancelTask(String)` / `subscribe(String, Consumer<Event>)` |
| 模型 | `chat(ChatRequest)` |
| MCP | `listTools()` / `callTool(String, Map)` |
| 护栏 | `checkGuardrail(String)` |
| 错误 | `A2AException` |

自动带 `X-A2A-Version: 1.0` / `X-Request-Id`（UUID 无横线）/ `X-API-Key`。
SSE 用 `SSE.Parser` 跨 chunk 拼接，支持 >128KB 单行 data 帧；`UPSTREAM_TIMEOUT` 映射为 `A2AException.isTimeout()`。

## 运行 Example

```powershell
$env:JAVA_HOME='C:\Java\jdk-22'
mvn -B -f sdk\java\pom.xml exec:java '-Dexec.mainClass=las.sdk.Example'
```

示例离线部分始终通过（官方类型构造、SSE 128KB 跨 chunk、护栏）；数据面在线时还会跑一次真实 `message/send`。

## 坑与解决

- **JDK 版本**：官方 A2A Java SDK 全系 record / major 61，JDK 11 报「类文件具有错误的版本 61.0, 应为 55.0」。必须 JDK 22 + `release 22`。
- **client artifact 坐标**：任务书里写的 `a2a-java-sdk-client` 起初本地仓库没有，用 `mvn -B dependency:get '-Dartifact=org.a2aproject.sdk:a2a-java-sdk-client:1.5.1.Final'` 查证 Maven Central 真实存在并成功下载（25 kB），pom 保持不变。
- **PowerShell 的 `-D` 参数**：必须写成 `'-Dartifact=...'`（引号），否则 PowerShell 把 `-D` 当参数名、把 artifact 当插件前缀，报「Plugin not found」。
- **编码**：Maven 平台编码为 GBK，pom 显式 `UTF-8` + compiler `<encoding>UTF-8</encoding>`；HTTP 读写用 `StandardCharsets.UTF_8`。
- **官方 builder 约束**：`AgentCard.capabilities` / `supportedInterfaces` 与 `Task.status` 强制非空，`build()` 时会校验（实测 `capabilities` 缺失抛 `IllegalArgumentException: Parameter 'capabilities' may not be null`）；构造时已用 `AgentCapabilities.builder()...build()` / `List.of(new AgentInterface("JSONRPC", url))` / `new TaskStatus(...)` 兜底。
- **不手写协议类型**：领域对象全部 import 官方 `spec` record，用 Jackson 做 JSON 绑定，避免「自己写一遍再声称用了 SDK」。
