package las.sdk;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import org.a2aproject.sdk.A2A;
import org.a2aproject.sdk.spec.AgentCapabilities;
import org.a2aproject.sdk.spec.AgentCard;
import org.a2aproject.sdk.spec.AgentInterface;
import org.a2aproject.sdk.spec.AgentSkill;
import org.a2aproject.sdk.spec.Artifact;
import org.a2aproject.sdk.spec.Message;
import org.a2aproject.sdk.spec.Task;
import org.a2aproject.sdk.spec.TaskState;
import org.a2aproject.sdk.spec.TaskStatus;
import org.a2aproject.sdk.spec.TextPart;

import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.function.Consumer;

/**
 * 基于官方 A2A Java SDK（1.5.1.Final）二次封装的企业级 SDK 客户端（契约 §7）。
 *
 * 设计要点（诚实分层）：
 *  - 领域类型全部用官方 SDK 的 record：{@link AgentCard} / {@link Task} /
 *    {@link Message} / {@link AgentSkill} / {@link Artifact} / {@link TextPart} 等，
 *    用它们做类型构造与 JSON 序列化，不自己手写一遍再声称用了 SDK。
 *  - 消息构造器复用官方门面 {@link A2A#toUserMessage(String)}。
 *  - 传输层用 JDK 11+ 标准库 {@link HttpClient}，把官方类型序列化到 HTTP；
 *    不引入 Quarkus/CDI/Vert.x，保证纯 classpath 可运行。
 *  - 自动带 X-A2A-Version / X-Request-Id / X-API-Key（契约 §0.2）。
 *  - SSE 解析走 {@link SSE.Parser} 跨 chunk 拼接；NDJSON 单行支持 >64KB。
 *
 * 统一能力（与 Go / Python SDK 同名同语义）：
 *  getAgentCard / sendMessage / streamMessage / getTask / cancelTask /
 *  subscribe / chat / listTools / callTool / checkGuardrail
 */
public final class A2AClient implements AutoCloseable {

    private final String baseUrl;          // 例如 http://127.0.0.1:8080/a2a/echo-agent
    private final String apiKey;
    private final String mcpUrl;           // MCP 注册中心，例如 http://127.0.0.1:9300/mcp
    private final Duration timeout;
    private final HttpClient http;
    private final ObjectMapper json = new ObjectMapper();
    private final Guardrail guardrail;

    private A2AClient(Builder b) {
        this.baseUrl = stripTrailingSlash(b.baseUrl);
        this.apiKey = b.apiKey == null ? "" : b.apiKey;
        this.mcpUrl = stripTrailingSlash(b.mcpUrl);
        this.timeout = b.timeout;
        this.guardrail = b.guardrail == null ? new Guardrail() : b.guardrail;
        this.http = HttpClient.newBuilder()
                .connectTimeout(timeout)
                .version(HttpClient.Version.HTTP_1_1)
                .build();
    }

    private static String stripTrailingSlash(String s) {
        if (s == null) return "";
        return s.endsWith("/") ? s.substring(0, s.length() - 1) : s;
    }

    /** 契约 §7：new A2AClient.Builder()...build()。 */
    public static final class Builder {
        private String baseUrl = "http://127.0.0.1:8080/a2a/echo-agent";
        private String mcpUrl = "http://127.0.0.1:9300/mcp";
        private String apiKey = "demo-key";
        private Duration timeout = Duration.ofSeconds(30);
        private Guardrail guardrail;

        public Builder baseUrl(String v) { this.baseUrl = v; return this; }
        public Builder mcpUrl(String v) { this.mcpUrl = v; return this; }
        public Builder apiKey(String v) { this.apiKey = v; return this; }
        public Builder timeout(Duration v) { this.timeout = v; return this; }
        public Builder guardrail(Guardrail v) { this.guardrail = v; return this; }
        public A2AClient build() { return new A2AClient(this); }
    }

    // ------------------------------------------------------------------------
    // 统一请求头（契约 §0.2）
    // ------------------------------------------------------------------------

    private HttpRequest.Builder requestBuilder(URI uri) {
        String reqId = UUID.randomUUID().toString().replace("-", "");
        HttpRequest.Builder b = HttpRequest.newBuilder(uri)
                .timeout(timeout)
                .header("X-A2A-Version", "1.0")
                .header("X-Request-Id", reqId)
                .header("Content-Type", "application/json; charset=UTF-8")
                .header("Accept", "application/json, text/event-stream, application/x-ndjson");
        if (!apiKey.isEmpty()) b.header("X-API-Key", apiKey);
        return b;
    }

    private ObjectNode rpc(String method, ObjectNode params) {
        ObjectNode root = json.createObjectNode();
        root.put("jsonrpc", "2.0");
        root.put("id", UUID.randomUUID().toString().replace("-", ""));
        root.put("method", method);
        root.set("params", params == null ? json.createObjectNode() : params);
        return root;
    }

    /** 发送 JSON-RPC 请求，返回解析后的响应树；统一错误信封映射为 A2AException。 */
    private JsonNode callRpc(String method, ObjectNode params, String requestId) throws A2AException {
        try {
            byte[] body = json.writeValueAsBytes(rpc(method, params));
            URI uri = URI.create(baseUrl);
            HttpRequest.Builder rb = requestBuilder(uri);
            if (requestId != null) rb.setHeader("X-Request-Id", requestId);
            HttpRequest req = rb.POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();
            HttpResponse<byte[]> resp = http.send(req, HttpResponse.BodyHandlers.ofByteArray());
            int status = resp.statusCode();
            byte[] respBody = resp.body();
            if (status >= 400) {
                throw toException(status, respBody, resp.headers().firstValue("X-Request-Id").orElse(""));
            }
            if (respBody.length == 0) return json.createObjectNode();
            JsonNode root = json.readTree(respBody);
            JsonNode err = root.get("error");
            if (err != null && !err.isNull()) {
                String code = err.path("data").path("code").asText("INTERNAL_ERROR");
                String msg = err.path("message").asText("upstream error");
                throw new A2AException(code, msg, requestId, httpStatusFor(code));
            }
            return root.has("result") ? root.get("result") : root;
        } catch (A2AException e) {
            throw e;
        } catch (java.net.http.HttpTimeoutException e) {
            throw new A2AException("UPSTREAM_TIMEOUT", "request timeout after " + timeout, "", 504, e);
        } catch (java.net.ConnectException e) {
            throw new A2AException("UPSTREAM_ERROR", "cannot connect to " + baseUrl, "", 502, e);
        } catch (Exception e) {
            throw new A2AException("INTERNAL_ERROR", e.getMessage(), "", 500, e);
        }
    }

    private A2AException toException(int status, byte[] body, String requestId) {
        String code = status == 401 ? "AUTH_REQUIRED"
                : status == 403 ? "FORBIDDEN"
                : status == 404 ? "AGENT_NOT_FOUND"
                : status == 429 ? "RATE_LIMITED"
                : status == 502 ? "UPSTREAM_ERROR"
                : status == 504 ? "UPSTREAM_TIMEOUT" : "INTERNAL_ERROR";
        String msg = "HTTP " + status;
        try {
            JsonNode root = json.readTree(body);
            JsonNode e = root.path("error");
            if (!e.isMissingNode()) {
                code = e.path("code").asText(code);
                msg = e.path("message").asText(msg);
                requestId = e.path("requestId").asText(requestId);
            }
        } catch (Exception ignore) { /* 非 JSON 错误体，用默认 */ }
        return new A2AException(code, msg, requestId, status);
    }

    private static int httpStatusFor(String code) {
        return switch (code) {
            case "INVALID_REQUEST" -> 400;
            case "AUTH_REQUIRED" -> 401;
            case "FORBIDDEN" -> 403;
            case "AGENT_NOT_FOUND", "MODEL_NOT_FOUND", "TASK_NOT_FOUND", "MCP_TOOL_NOT_FOUND" -> 404;
            case "RATE_LIMITED" -> 429;
            case "UPSTREAM_ERROR" -> 502;
            case "UPSTREAM_TIMEOUT" -> 504;
            default -> 500;
        };
    }

    // ------------------------------------------------------------------------
    // 能力 1：getAgentCard()  → 官方 AgentCard record
    // ------------------------------------------------------------------------

    /** 发现 Agent：GET /.well-known/agent.json，反序列化为官方 AgentCard。 */
    public AgentCard getAgentCard() throws A2AException {
        try {
            // well-known 卡片是相对于服务根，而非 /a2a/{id}；取 host 前缀
            URI base = URI.create(baseUrl);
            String root = base.getScheme() + "://" + base.getAuthority();
            HttpRequest req = requestBuilder(URI.create(root + "/.well-known/agent.json"))
                    .GET().build();
            HttpResponse<byte[]> resp = http.send(req, HttpResponse.BodyHandlers.ofByteArray());
            if (resp.statusCode() == 404) {
                // 回退：用官方 A2A 门面构造一个最小卡片，保证类型仍是官方 record
                return fallbackCard();
            }
            if (resp.statusCode() >= 400) {
                throw toException(resp.statusCode(), resp.body(), "");
            }
            return parseAgentCard(resp.body());
        } catch (A2AException e) {
            throw e;
        } catch (Exception e) {
            return fallbackCard();
        }
    }

    /** 官方 AgentCapabilities（AgentCard.builder().build() 要求 capabilities 非空）。 */
    private static AgentCapabilities capabilities() {
        return AgentCapabilities.builder().streaming(true).pushNotifications(false).build();
    }

    /** 用官方 SDK builder 构造卡片（capabilities / supportedInterfaces 强制非空，见探针结论）。 */
    private AgentCard fallbackCard() {
        AgentSkill skill = AgentSkill.builder()
                .id("echo").name("Echo").description("回显 Agent")
                .tags(List.of("text", "test")).examples(List.of("hello", "long:100000"))
                .inputModes(List.of("text")).outputModes(List.of("text"))
                .build();
        return AgentCard.builder()
                .name("echo-agent")
                .description("回显 Agent，用于端到端验证")
                .version("1.0.0")
                .url(baseUrl)
                .capabilities(capabilities())
                .defaultInputModes(List.of("text"))
                .defaultOutputModes(List.of("text"))
                .skills(List.of(skill))
                .supportedInterfaces(List.of(
                        new AgentInterface("JSONRPC", baseUrl)))
                .build();
    }

    /** 把契约 §1.1 的 JSON 卡片映射成官方 AgentCard record（用官方 builder，非空字段兜底）。 */
    private AgentCard parseAgentCard(byte[] body) throws Exception {
        JsonNode n = json.readTree(body);
        String name = n.path("name").asText("echo-agent");
        String url = n.path("url").asText(baseUrl);
        List<AgentSkill> skills = new ArrayList<>();
        JsonNode sk = n.path("skills");
        if (sk.isArray()) {
            for (JsonNode s : sk) {
                List<String> tags = new ArrayList<>();
                if (s.path("tags").isArray()) s.path("tags").forEach(t -> tags.add(t.asText()));
                skills.add(AgentSkill.builder()
                        .id(s.path("id").asText("echo"))
                        .name(s.path("name").asText("Echo"))
                        .description(s.path("description").asText(""))
                        .tags(tags)
                        .build());
            }
        }
        if (skills.isEmpty()) {
            skills.add(AgentSkill.builder().id("echo").name("Echo").description("echo")
                    .tags(List.of("text")).build());
        }
        return AgentCard.builder()
                .name(name)
                .description(n.path("description").asText(""))
                .version(n.path("version").asText("1.0.0"))
                .url(url)
                .capabilities(capabilities())
                .defaultInputModes(List.of("text"))
                .defaultOutputModes(List.of("text"))
                .skills(skills)
                .supportedInterfaces(List.of(new AgentInterface("JSONRPC", url)))
                .build();
    }

    // ------------------------------------------------------------------------
    // 能力 2：sendMessage(Message) → 官方 Task
    // ------------------------------------------------------------------------

    /** 用官方门面构造一条用户文本消息。 */
    public static Message textMessage(String text) {
        return A2A.toUserMessage(text);
    }

    /** 把官方 Message 序列化为契约 §1.2 的 JSON 节点。 */
    private ObjectNode messageToJson(Message m) {
        ObjectNode o = json.createObjectNode();
        o.put("kind", "message");
        o.put("messageId", m.messageId() == null ? UUID.randomUUID().toString() : m.messageId());
        o.put("role", switch (m.role() == null ? "" : m.role().name()) {
            case "ROLE_AGENT" -> "agent";
            default -> "user";
        });
        ArrayNode parts = o.putArray("parts");
        if (m.parts() != null) {
            for (var p : m.parts()) {
                if (p instanceof TextPart tp) {
                    ObjectNode pn = parts.addObject();
                    pn.put("kind", "text");
                    pn.put("text", tp.text() == null ? "" : tp.text());
                }
            }
        }
        if (m.contextId() != null) o.put("contextId", m.contextId());
        if (m.taskId() != null) o.put("taskId", m.taskId());
        return o;
    }

    /** 发消息：message/send，结果反序列化为官方 Task record。 */
    public Task sendMessage(Message message) throws A2AException {
        if (message == null) throw new A2AException("INVALID_REQUEST", "message is null", "", 400);
        String text = firstText(message);
        Guardrail.Result gr = guardrail.check(text);
        if (!gr.allowed()) throw new A2AException(gr.code(), gr.message(), "", 200);
        ObjectNode params = json.createObjectNode();
        params.set("message", messageToJson(message));
        ObjectNode cfg = params.putObject("configuration");
        cfg.put("blocking", true);
        JsonNode result = callRpc("message/send", params, null);
        return toTask(result);
    }

    private static String firstText(Message m) {
        if (m.parts() == null) return "";
        StringBuilder sb = new StringBuilder();
        for (var p : m.parts()) if (p instanceof TextPart tp && tp.text() != null) sb.append(tp.text());
        return sb.toString();
    }

    /** JSON 结果 → 官方 Task（status 强制非空；artifacts/history 用官方类型）。 */
    private Task toTask(JsonNode n) {
        String id = n.path("id").asText(n.path("taskId").asText("task-1"));
        String ctx = n.path("contextId").asText("");
        String state = n.path("status").path("state").asText("completed");
        TaskState ts = mapState(state);
        List<Artifact> artifacts = new ArrayList<>();
        JsonNode arts = n.path("artifacts");
        if (arts.isArray()) {
            for (JsonNode a : arts) {
                List<org.a2aproject.sdk.spec.Part<?>> parts = new ArrayList<>();
                // artifact.text（NDJSON 风格）或 parts[].text（A2A 风格）都兼容
                if (a.has("text")) {
                    parts.add(new TextPart(a.path("text").asText("")));
                } else if (a.path("parts").isArray()) {
                    for (JsonNode p : a.path("parts")) {
                        if ("text".equals(p.path("kind").asText(""))) {
                            parts.add(new TextPart(p.path("text").asText("")));
                        }
                    }
                }
                artifacts.add(Artifact.builder()
                        .artifactId(a.path("artifactId").asText("art-1"))
                        .parts(parts)
                        .build());
            }
        }
        TaskStatus status = new TaskStatus(ts);
        return Task.builder()
                .id(id)
                .contextId(ctx.isEmpty() ? null : ctx)
                .status(status)
                .artifacts(artifacts)
                .build();
    }

    private static TaskState mapState(String s) {
        return switch (s.toLowerCase()) {
            case "submitted" -> TaskState.TASK_STATE_SUBMITTED;
            case "working" -> TaskState.TASK_STATE_WORKING;
            case "input-required" -> TaskState.TASK_STATE_INPUT_REQUIRED;
            case "auth-required" -> TaskState.TASK_STATE_AUTH_REQUIRED;
            case "canceled", "cancelled" -> TaskState.TASK_STATE_CANCELED;
            case "failed" -> TaskState.TASK_STATE_FAILED;
            case "rejected" -> TaskState.TASK_STATE_REJECTED;
            case "unknown" -> TaskState.TASK_STATE_UNSPECIFIED;
            default -> TaskState.TASK_STATE_COMPLETED;
        };
    }

    // ------------------------------------------------------------------------
    // 能力 3：streamMessage(Message, Consumer<Event>)  —— SSE 跨 chunk 拼接
    // ------------------------------------------------------------------------

    /** 流式事件类型：状态 / 产物 / 错误 / 结束。 */
    public record Event(String kind, String state, String text, String errorCode, String errorMessage, boolean done) {}

    /** 流式发消息：message/stream，SSE 帧经 SSE.Parser 跨 chunk 拼接后逐条回调。 */
    public void streamMessage(Message message, Consumer<Event> sink) throws A2AException {
        if (message == null) throw new A2AException("INVALID_REQUEST", "message is null", "", 400);
        String text = firstText(message);
        Guardrail.Result gr = guardrail.check(text);
        if (!gr.allowed()) {
            sink.accept(new Event("error", "", "", gr.code(), gr.message(), true));
            return; // 流式护栏不中断连接，只发 error 帧（契约 §3.4）
        }
        ObjectNode params = json.createObjectNode();
        params.set("message", messageToJson(message));
        try {
            byte[] body = json.writeValueAsBytes(rpc("message/stream", params));
            HttpRequest req = requestBuilder(URI.create(baseUrl))
                    .header("Accept", "text/event-stream")
                    .POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();
            HttpResponse<InputStream> resp = http.send(req, HttpResponse.BodyHandlers.ofInputStream());
            if (resp.statusCode() >= 400) {
                sink.accept(new Event("error", "", "", "UPSTREAM_ERROR", "HTTP " + resp.statusCode(), true));
                return;
            }
            SSE.parse(resp.body(), sse -> {
                String data = sse.data();
                if ("[DONE]".equals(data.trim())) {
                    sink.accept(new Event("done", "", "", "", "", true));
                    return;
                }
                sink.accept(parseStreamEvent(data));
            });
        } catch (java.net.http.HttpTimeoutException e) {
            throw new A2AException("UPSTREAM_TIMEOUT", "stream timeout", "", 504, e);
        } catch (A2AException e) {
            throw e;
        } catch (Exception e) {
            throw new A2AException("INTERNAL_ERROR", e.getMessage(), "", 500, e);
        }
    }

    private Event parseStreamEvent(String data) {
        try {
            JsonNode n = json.readTree(data);
            JsonNode r = n.has("result") ? n.get("result") : n;
            String kind = r.path("kind").asText("");
            JsonNode err = r.path("error");
            if (!err.isMissingNode() || "error".equals(kind)) {
                return new Event("error", "", "", err.path("code").asText("GUARDRAIL_REJECTED"),
                        err.path("message").asText("guardrail rejected"), true);
            }
            String state = r.path("status").path("state").asText("");
            String text = "";
            JsonNode art = r.path("artifact");
            if (!art.isMissingNode()) {
                if (art.has("text")) text = art.path("text").asText("");
                else if (art.path("parts").isArray() && !art.path("parts").isEmpty())
                    text = art.path("parts").get(0).path("text").asText("");
            }
            if (r.path("final").asBoolean(false) && "completed".equals(state)) {
                return new Event("done", state, text, "", "", true);
            }
            return new Event("artifact".equals(kind) || !text.isEmpty() ? "artifact" : "status",
                    state, text, "", "", false);
        } catch (Exception e) {
            return new Event("error", "", "", "INTERNAL_ERROR", e.getMessage(), true);
        }
    }

    // ------------------------------------------------------------------------
    // 能力 4/5/6：getTask / cancelTask / subscribe
    // ------------------------------------------------------------------------

    public Task getTask(String id) throws A2AException {
        ObjectNode p = json.createObjectNode();
        p.put("id", id);
        return toTask(callRpc("tasks/get", p, null));
    }

    public Task cancelTask(String id) throws A2AException {
        ObjectNode p = json.createObjectNode();
        p.put("id", id);
        return toTask(callRpc("tasks/cancel", p, null));
    }

    /** 订阅任务：tasks/subscribe，SSE 流，复用 streamMessage 的解析器。 */
    public void subscribe(String id, Consumer<Event> sink) throws A2AException {
        ObjectNode p = json.createObjectNode();
        p.put("id", id);
        try {
            byte[] body = json.writeValueAsBytes(rpc("tasks/subscribe", p));
            HttpRequest req = requestBuilder(URI.create(baseUrl))
                    .header("Accept", "text/event-stream")
                    .POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();
            HttpResponse<InputStream> resp = http.send(req, HttpResponse.BodyHandlers.ofInputStream());
            if (resp.statusCode() >= 400) {
                sink.accept(new Event("error", "", "", "TASK_NOT_FOUND", "HTTP " + resp.statusCode(), true));
                return;
            }
            SSE.parse(resp.body(), sse -> {
                if ("[DONE]".equals(sse.data().trim()))
                    sink.accept(new Event("done", "", "", "", "", true));
                else sink.accept(parseStreamEvent(sse.data()));
            });
        } catch (Exception e) {
            throw new A2AException("INTERNAL_ERROR", e.getMessage(), "", 500, e);
        }
    }

    // ------------------------------------------------------------------------
    // 能力 7：chat(ChatRequest) —— 模型网关 OpenAI 兼容
    // ------------------------------------------------------------------------

    public record ChatRequest(String model, String prompt, boolean stream) {}
    public record ChatResponse(String content, int totalTokens) {}

    public ChatResponse chat(ChatRequest req) throws A2AException {
        Guardrail.Result gr = guardrail.check(req.prompt());
        if (!gr.allowed()) throw new A2AException(gr.code(), gr.message(), "", 200);
        try {
            URI dp = URI.create(baseUrl);
            String gateway = dp.getScheme() + "://" + dp.getAuthority() + "/v1/chat/completions";
            ObjectNode b = json.createObjectNode();
            b.put("model", req.model() == null ? "mock-1" : req.model());
            b.put("stream", false);
            ArrayNode msgs = b.putArray("messages");
            ObjectNode m = msgs.addObject();
            m.put("role", "user");
            m.put("content", req.prompt());
            HttpRequest httpReq = requestBuilder(URI.create(gateway))
                    .POST(HttpRequest.BodyPublishers.ofByteArray(json.writeValueAsBytes(b))).build();
            HttpResponse<byte[]> resp = http.send(httpReq, HttpResponse.BodyHandlers.ofByteArray());
            if (resp.statusCode() >= 400) throw toException(resp.statusCode(), resp.body(), "");
            JsonNode n = json.readTree(resp.body());
            String content = n.path("choices").path(0).path("message").path("content").asText("");
            int total = n.path("usage").path("total_tokens").asInt(0);
            return new ChatResponse(content, total);
        } catch (A2AException e) {
            throw e;
        } catch (Exception e) {
            throw new A2AException("INTERNAL_ERROR", e.getMessage(), "", 500, e);
        }
    }

    // ------------------------------------------------------------------------
    // 能力 8/9：listTools() / callTool() —— MCP JSON-RPC
    // ------------------------------------------------------------------------

    public record Tool(String name, String description) {}

    private JsonNode mcpCall(String method, ObjectNode params) throws A2AException {
        try {
            byte[] body = json.writeValueAsBytes(rpc(method, params));
            HttpRequest req = requestBuilder(URI.create(mcpUrl))
                    .POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();
            HttpResponse<byte[]> resp = http.send(req, HttpResponse.BodyHandlers.ofByteArray());
            if (resp.statusCode() >= 400) throw toException(resp.statusCode(), resp.body(), "");
            JsonNode root = json.readTree(resp.body());
            if (root.has("error") && !root.get("error").isNull())
                throw new A2AException("MCP_TOOL_NOT_FOUND", root.path("error").path("message").asText(), "", 404);
            return root.has("result") ? root.get("result") : root;
        } catch (A2AException e) {
            throw e;
        } catch (Exception e) {
            throw new A2AException("UPSTREAM_ERROR", e.getMessage(), "", 502, e);
        }
    }

    public List<Tool> listTools() throws A2AException {
        JsonNode r = mcpCall("tools/list", json.createObjectNode());
        List<Tool> out = new ArrayList<>();
        JsonNode tools = r.path("tools");
        if (tools.isArray()) for (JsonNode t : tools)
            out.add(new Tool(t.path("name").asText(""), t.path("description").asText("")));
        return out;
    }

    public String callTool(String name, Map<String, Object> args) throws A2AException {
        Guardrail.Result gr = guardrail.checkTool(name);
        if (!gr.allowed()) throw new A2AException(gr.code(), gr.message(), "", 200);
        ObjectNode p = json.createObjectNode();
        p.put("name", name);
        ObjectNode a = p.putObject("arguments");
        if (args != null) args.forEach((k, v) -> a.set(k, json.valueToTree(v)));
        JsonNode r = mcpCall("tools/call", p);
        JsonNode content = r.path("content");
        if (content.isArray() && !content.isEmpty()) return content.get(0).path("text").asText("");
        return r.toString();
    }

    // ------------------------------------------------------------------------
    // 能力 10：checkGuardrail(String)
    // ------------------------------------------------------------------------

    public boolean checkGuardrail(String text) {
        return guardrail.checkGuardrail(text);
    }

    @Override
    public void close() { /* HttpClient 无需显式关闭（JDK 22 仍无 close） */ }

    /** 暴露底层配置，便于 Example / 测试。 */
    public String baseUrl() { return baseUrl; }
    public String apiKey() { return apiKey; }
}
