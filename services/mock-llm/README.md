# Mock LLM 服务（端口 9200）

`local-agent-stack` 数据面模型网关的本地 OpenAI 兼容后端。FastAPI + uvicorn，纯 Python 3.12。

> 本目录与 `sdk/python/`、`requirements.txt` 由同一个 Python 子任务负责；不修改其他模块。

## 依赖版本决策（诚实记录）

按 `docs/06-依赖与版本.md` §4 与探针 agent-3 实测结论：

| 库 | PyPI 最新 | 实际锁定 | 原因 |
|---|---|---|---|
| `a2a-sdk` | 1.3.0 | **0.3.26**（`[http-server]` extra） | **因 1.x breaking change 固定为 0.3.26**：1.x 删除了 `a2a.server.apps`（`ModuleNotFoundError`）、`a2a.types` 改为 protobuf 生成类，无法用 `from a2a.server.apps import A2AStarletteApplication` 构造 Starlette。0.3.26 实测 `a2a.server.apps` / `a2a.types` / `a2a.client` 全部可 import。 |
| `mcp` | 2.3.0 | **1.30.0** | **因 2.x breaking change 固定为 1.30.0**：2.x 把 `FastMCP` 改名为 `MCPServer`、字段名蛇形化，`from mcp.server.fastmcp import FastMCP` / `import mcp.server.fastmcp` 直接抛异常。1.30.0 实测 `FastMCP` 可 import、`@mcp.tool()` / `await list_tools()` / `await call_tool()` 可用。 |
| `pydantic` | — | **2.11.3** | `a2a-sdk==0.3.26` 要求 `pydantic>=2.11.3`，原计划 2.10.4 会 `ResolutionImpossible`，故上调到 2.11.3。 |
| `fastapi` / `uvicorn` / `httpx` | — | 0.115.6 / 0.34.0 / 0.28.1 | 固定版本，实测可用。 |

> 安装命令（清华镜像，原生 pypi.org 在本机下载慢 / 解析超时）：
> ```
> ..\..\.venv\Scripts\python.exe -m pip install --no-cache-dir --timeout 30 --retries 2 -i https://pypi.tuna.tsinghua.edu.cn/simple -r ..\..\requirements.txt
> ```

## 启动

```powershell
# 项目根 local-agent-stack/
.\.venv\Scripts\python.exe services/mock-llm/main.py 9200
# 或读环境变量
$env:LLM_PORT = '9200'; .\.venv\Scripts\python.exe services/mock-llm/main.py
# slow 模式：embeddings 睡 2s，供数据面 / SDK 超时排查（E15）
$env:MOCK_MODE = 'slow'; .\.venv\Scripts\python.exe services/mock-llm/main.py 9200
```

端口优先级：命令行参数 > `LLM_PORT` 环境变量 > 默认 9200。监听地址取 `LLM_HOST`（默认 `127.0.0.1`）。

## 端点（契约 §0.4 / §2 / §5.2）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | `{"status":"UP","service":"mock-llm","version":"1.0.0","now":"<RFC3339 +08:00>"}` |
| POST | `/v1/chat/completions` | §2.1。`stream:false` → 标准 `chat.completion`（usage 三个值都是 int）；`stream:true` → SSE |
| POST | `/v1/embeddings` | §2.2。`object:"list"`，embedding 是 float 数组，usage 整数；**接受 >8000 字符 / 124844 字节超长输入不报错** |
| GET | `/v1/models` | §2.3，返回 `mock-1` 与 `bge-m3` |
| POST/GET/... | `/tools/echo` | §5.2 / E19：真实回显 HTTP 请求（method/path/query/headers/body 原样放进 JSON）返回 `{"echo":{...},"ok":true}` |
| 任意 | `/{full_path:path}` | catch-all 兜底回显，便于 MCP 注册中心回放任意 `source.path` |

### 流式 SSE 帧（严格契约 §2.1）

```
data: {"id":"chatcmpl-...","object":"chat.completion.chunk",...,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"...","choices":[{"index":0,"delta":{"content":"[mock-llm 回显] hel"},"finish_reason":null}]}

... 若干 content 帧 ...
data: [DONE]

```

- 每帧 `data: <单行 JSON>\n\n`，JSON 用 `ensure_ascii=False` 保证中文不乱码；
- `StreamingResponse` 不设 `Content-Length`，加 `Cache-Control: no-cache` / `X-Accel-Buffering: no`（对齐 §3.4 不缓冲）；
- `X-Request-Id` 原样透传（§0.2），无则生成 uuid4 hex 并回写响应头。

### 统一编码与错误

- 所有响应 `Content-Type: application/json; charset=utf-8`（自定义 `UTF8JSONResponse` 覆盖 FastAPI 默认无 charset 的 `application/json`；middleware 兜底）；
- 非法 JSON / 缺必填字段 → 契约 §0.3 `INVALID_REQUEST` 错误信封（400）；
- `MOCK_MODE=slow` 只影响 `/v1/embeddings`（`time.sleep(2)`），用于复现数据面 chat 30s / embeddings 90s 独立超时配置与简历里 `content-length: 124844` 的排查场景（E15）。

## 文件

- `main.py` —— FastAPI app、路由、middleware、启动入口（`uvicorn.run`）
- `openai_compat.py` —— OpenAI 响应 / SSE 帧 / embedding 构造（纯函数，方便 py_compile 与复用）
- `tools_echo.py` —— `/tools/echo` 回显结构（从 ASGI scope 取 method/path/query/headers/body）

## 与 E2E 用例的关系

| 用例 | 本服务提供的能力 |
|---|---|
| E01 | `/health` 返回 UP |
| E13 | 非流 chat 标准 OpenAI 结构 + `usage.total_tokens` 整数 |
| E14 | 流式 chat 多 chunk + `[DONE]` |
| E15 | embeddings 超长输入（124844 字节）不报错；slow 模式供超时排查 |
| E19 | `/tools/echo` 真实回显，供 MCP 注册中心 `tools/call` 回放验证 |
