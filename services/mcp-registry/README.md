# mcp-registry（MCP 注册中心 + OpenAPI→MCP 转换引擎，Java 11 :9300）

`local-agent-stack` 的 MCP 注册中心。核心是自研的 **OpenAPI 3.0 → MCP Tool 自动转换引擎**
（`$ref` 递归展开 / `allOf` 合并 / 嵌套对象与数组保留）和 **tools/call 真实 HTTP 回放**。
对应能力矩阵 C1–C6、E2E 用例 E18 / E19。

## 技术选型（有依据，不是偷懒）

官方 MCP Java SDK 在 JDK 11 下不可用（class major=61 / Java 17 + `Record`，MCP 2.2.0 还拖
Jackson 3），实测见 `docs/06-依赖与版本.md` §3。因此本服务用 **Jackson 2.17.2 +
JDK 内置 `com.sun.net.httpserver.HttpServer`** 自行实现 JSON-RPC 2.0 / REST 层。
探针 `_probe/java/.../JsonRpcServer.java` 作为 `JsonRpc.java` 的骨架基础。

**编码**：platform 默认 GBK，所有点显式 UTF-8 —— pom `sourceEncoding` + compiler
`<encoding>UTF-8</encoding>` + `new String(bytes, StandardCharsets.UTF_8)` +
响应头 `application/json; charset=UTF-8`。

## 编译 / 运行

```powershell
$env:JAVA_HOME = 'C:\Java\jdk-11'
mvn -B clean compile
mvn -q exec:java                                       # 运行（默认 9300）
java -cp "target\classes;<3个jackson jar>" las.mcp.Main 9300
# Main 第二个参数可指定数据文件：las.mcp.Main 9300 data\mcp-registry.json
```

自测（main-based，不引 JUnit，47 断言，覆盖 `$ref` 展开 / allOf / 循环引用 / 转换器）：

```powershell
mvn -q test-compile
java -cp "target\classes;target\test-classes;<3个jackson jar>" las.mcp.SelfTest
```

## 端点

| Method | Path | 说明 |
|---|---|---|
| GET | `/health` | `{"status":"UP","service":"mcp-registry",...}` |
| POST | `/mcp` | JSON-RPC 2.0：`initialize` / `tools/list` / `tools/call` / `tools/get` / `ping` |
| POST | `/registry/import` | OpenAPI → MCP 转换 + 落盘，返回 `{imported,tools[],skipped[]}` |
| POST | `/registry/validate` | 只校验/转换不导入（额外返回 `sampleTool` 便于检查 `$ref` 展开） |
| GET | `/registry/tools` | 列出工具（`?q=` 名称/描述检索） |
| GET | `/registry/tools/{name}` | 单个工具 |
| POST | `/registry/tools` | 手动注册工具（高码/低码/三方统一 `Tool` 模型） |
| DELETE | `/registry/tools/{name}` | 删除工具 |
| GET | `/registry/specs` | 已导入文档列表 |

`/mcp` 错误码：`-32700` 解析错误 / `-32600` 非法请求 / `-32601` 方法不存在 /
`-32602` 参数无效 / `-32603` 内部错误。通知（无 `id`）返回 **204 空 body**。

## OpenAPI → MCP 转换规则（契约 §5.2，逐条实现）

1. 遍历 `paths` 每个 `method`（get/post/put/delete/patch），**跳过无 operationId**，记录 `skipped[]`。
2. `name = operationId`，重名加 `_get`/`_post` 后缀。
3. `description = summary || description || "METHOD path"`，拼接 path 与 method。
4. `inputSchema.properties`：
   - `parameters[]`（`in: path|query|header`），path 参数**强制 required**；
   - `requestBody.content["application/json"].schema.properties`（标记 `in: body`）；
   - **`$ref` 递归展开 / `allOf` 合并 / 数组 items / 嵌套对象保留** —— `SchemaResolver.java`。
5. `annotations`：get→`readOnlyHint`，delete→`destructiveHint`，put/get→`idempotentHint`。
6. `source` 记录 `path/method/operationId/baseUrl`，供 `tools/call` 回放。
7. 返回 `{"imported":N,"tools":[names...],"skipped":[{"path","method","reason"}]}`。

### ★ `$ref` 展开实现位置

- `SchemaResolver.java` : 类注释 + `resolve(JsonNode, Set<String>)`（普通 schema 递归）、
  `resolveRef(...)`（解析 `#/components/schemas/X` + **防循环引用** ref 栈）、
  `resolveAllOf(...)`（allOf 合并 properties/required）、`pointer(...)`（JSON Pointer）。
- `OpenApiConverter.java` : `buildInputSchema(...)` 调用 `resolver.resolve(...)` 展开
  parameters 的 schema 与 requestBody 的 schema，把展开后的 properties 提升为 MCP 工具入参。

**展开前后对比**（`sample-openapi.json` 的 `CreateUserRequest.requestBody`）：

```
展开前（requestBody schema）：
  {"$ref":"#/components/schemas/CreateUserRequest"}
  CreateUserRequest = {"allOf":[ {"$ref":"#/components/schemas/BaseUser"},
                                 {"type":"object","properties":{
                                    "address":{"$ref":"#/components/schemas/Address"},
                                    "roles":{"type":"array","items":{"type":"string"}},...},
                                  "required":["address"]} ]}

展开后（createUser.inputSchema.properties，已无 $ref / allOf）：
  {
    "name":{"type":"string","in":"body"},
    "email":{"type":"string","format":"email","in":"body"},
    "address":{"type":"object","in":"body","properties":{
        "city":{...},"street":{...},"zip":{...},
        "geo":{"type":"object","properties":{"lat":{...},"lng":{...}}}}},  ← 嵌套对象保留
    "roles":{"type":"array","items":{"type":"string"},"in":"body"},          ← 数组保留
    "age":{"type":"integer","minimum":0,"in":"body"}
  }
  required = ["name","email","address"]                                       ← allOf 合并
```

`User.manager` 是 `$ref` 自引用，用 ref 栈检测并打 `circular $ref (stopped)` 标记，
不会 `StackOverflowError`（SelfTest 断言）。

## tools/call 真实 HTTP 回放（E19）

`Replay.java` 用 `Tool.source` 构造**真实** HTTP 请求：
- path 参数替换 URL 里的 `{id}`（URL 编码）；
- query 参数拼 `?a=1&b=2`；header 参数写请求头；body 参数组装 JSON；
- 打到 `source.baseUrl`（默认 `http://127.0.0.1:9200`，`replayPath=/tools/echo`，
  即 Mock LLM 的回显端点），并把原始 `_tool_path` / `_tool_method` 作为 query 元信息带上；
- 返回 `{"content":[{"type":"text","text":<HTTP响应体>}],"isError":false}`，
  HTTP 非 2xx → `isError:true`；连接失败也用 `isError:true`（不抛 JSON-RPC error）。

> 这是真的 `HttpURLConnection` 发请求，不是返回假数据。要看到成功的 `isError:false`，
> 需先启动 Mock LLM（:9200）；否则会得到 `isError:true` 且 text 里说明连接失败。

## 示例文档

`src/main/java/las/mcp/sample-openapi.json`（同时拷到 `src/main/resources/` 供 SelfTest 读取）：
5 个 path、2 个无 operationId 的 skipped 样本、`$ref` / `allOf` / 自引用循环、
path 参数 `{id}`、query 参数、header 参数、requestBody、嵌套对象、数组。
导入后 `imported=5`，`tools=[getUserById, deleteUser, createUser, replaceUser, patchPet]`。

## 文件

```
Main.java              启动 9300 / 注册 /mcp 与 /registry/ 前缀
JsonRpc.java           JSON-RPC 2.0 骨架（复用探针：批量/通知204/5 个错误码）
HttpJson.java          UTF-8 HTTP 工具 + 统一错误信封
Tool.java              §5.1 Tool 模型（name/description/inputSchema/annotations/source）
Registry.java          高码/低码/三方统一注册、检索、版本、原子持久化
OpenApiConverter.java  ★ OpenAPI 3.0 → MCP Tool 转换引擎
SchemaResolver.java    ★ $ref 递归展开 / allOf 合并 / 防循环 / 嵌套与数组保留
Replay.java            ★ tools/call 真实 HTTP 回放
McpApi.java            /mcp 的 initialize/tools/list/tools/call/tools/get/ping
ImportApi.java         /registry/import|validate|tools|specs + DELETE tools/{name}
Health.java            GET /health
sample-openapi.json    E18 验证用示例文档
SelfTest.java          main-based 自测（47 断言）
```
