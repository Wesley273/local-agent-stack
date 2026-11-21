package las.example;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import org.a2aproject.sdk.spec.AgentCapabilities;
import org.a2aproject.sdk.spec.AgentCard;
import org.a2aproject.sdk.spec.AgentInterface;
import org.a2aproject.sdk.spec.AgentSkill;
import org.a2aproject.sdk.spec.Artifact;
import org.a2aproject.sdk.spec.Message;
import org.a2aproject.sdk.spec.Part;
import org.a2aproject.sdk.spec.Task;
import org.a2aproject.sdk.spec.TaskState;
import org.a2aproject.sdk.spec.TaskStatus;
import org.a2aproject.sdk.spec.TextPart;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.concurrent.Executors;

/**
 * Java 示例 Agent —— User Server（NDJSON 协议，契约 §1.7）。
 *
 * 双重身份：
 *  1) 官方 A2A Java SDK：用 AgentCard / Task / Message / AgentSkill / Artifact / TextPart
 *     等官方 record 构造领域对象（见 buildCard / buildTask / echoArtifact），并用
 *     Jackson 把它们序列化到 /.well-known/agent.json 与 /a2a 端点；
 *  2) User Server：Sidecar 通过 NDJSON /task 连它取流。
 *
 * 端口 9102（config/ports.env AGENT_JAVA_PORT）。
 *
 * 端点（与 Go / Python Agent 完全一致）：
 *   GET  /health                    → {"status":"UP","service":"java-agent"}
 *   POST /task  |  POST /run        → application/x-ndjson 流
 *   GET  /.well-known/agent.json   → 官方 AgentCard 序列化（合规轨）
 *   POST /a2a                      → 最小 JSON-RPC message/send（合规轨）
 *
 * NDJSON 流逐行：
 *   {"type":"status","state":"working","progress":0.5}\n
 *   {"type":"artifact","artifactId":"art-1","text":"<回显内容>"}\n
 *   {"type":"done","state":"completed"}\n
 *
 * long:N 输入 → artifact.text 产生 N 个字符的单行（E11 验证 >128KB 不截断）。
 */
public final class JavaAgent {

    private static final ObjectMapper JSON = new ObjectMapper();
    private static final String SERVICE = "java-agent";
    private static final int DEFAULT_PORT = 9102;

    public static void main(String[] args) throws Exception {
        int port = args.length > 0 ? Integer.parseInt(args[0]) : DEFAULT_PORT;
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", port), 0);
        server.createContext("/health", JavaAgent::health);
        server.createContext("/task", JavaAgent::ndjson);
        server.createContext("/run", JavaAgent::ndjson);
        server.createContext("/.well-known/agent.json", JavaAgent::card);
        server.createContext("/.well-known/agent-card.json", JavaAgent::card);
        server.createContext("/a2a", JavaAgent::jsonRpc);
        server.setExecutor(Executors.newFixedThreadPool(8));
        server.start();
        System.out.println("[" + SERVICE + "] listening on 127.0.0.1:" + port);
    }

    // ------------------------------------------------------------------------
    // 官方 A2A SDK 类型构造（合规轨）
    // ------------------------------------------------------------------------

    /** 用官方 AgentCard.builder() 构造卡片：capabilities / supportedInterfaces 强制非空。 */
    static AgentCard buildCard(String baseUrl) {
        AgentSkill skill = AgentSkill.builder()
                .id("echo").name("Echo")
                .description("原样回显输入文本，可产生超长 artifact 用于验证 64KB 边界")
                .tags(List.of("text", "test"))
                .examples(List.of("hello", "long:100000"))
                .inputModes(List.of("text")).outputModes(List.of("text"))
                .build();
        AgentCapabilities caps = AgentCapabilities.builder().streaming(true).build();
        return AgentCard.builder()
                .name(SERVICE)
                .description("Java 示例回显 Agent（User Server / NDJSON）")
                .version("1.0.0")
                .url(baseUrl)
                .capabilities(caps)
                .defaultInputModes(List.of("text"))
                .defaultOutputModes(List.of("text"))
                .skills(List.of(skill))
                .supportedInterfaces(List.of(new AgentInterface("JSONRPC", baseUrl + "/a2a")))
                .build();
    }

    /** 用官方 Artifact.builder() + TextPart 构造产物（回显文本）。 */
    static Artifact echoArtifact(String text) {
        List<Part<?>> parts = List.of(new TextPart(text));
        return Artifact.builder().artifactId("art-1").name("result").parts(parts).build();
    }

    /** 用官方 Task.builder() 构造任务（status 强制非空）。 */
    static Task buildTask(String id, String ctxId, String text) {
        Message userMsg = org.a2aproject.sdk.A2A.toUserMessage(text);
        return Task.builder()
                .id(id).contextId(ctxId)
                .status(new TaskStatus(TaskState.TASK_STATE_COMPLETED))
                .artifacts(List.of(echoArtifact(text)))
                .history(userMsg)
                .build();
    }

    // ------------------------------------------------------------------------
    // User Server 端点
    // ------------------------------------------------------------------------

    private static void health(HttpExchange ex) throws IOException {
        ObjectNode o = JSON.createObjectNode();
        o.put("status", "UP");
        o.put("service", SERVICE);
        sendJson(ex, 200, o.toString(), "application/json; charset=UTF-8");
    }

    /** 官方 AgentCard 序列化成 JSON（用 Jackson 绑定官方 record）。 */
    private static void card(HttpExchange ex) throws IOException {
        String base = "http://" + ex.getRequestHeaders().getFirst("Host");
        byte[] body = JSON.writeValueAsBytes(buildCard(base));
        sendBytes(ex, 200, body, "application/json; charset=UTF-8");
    }

    /**
     * POST /task 或 /run：接收 {"type":"task","taskId":"...","message":{...}}，
     * 返回 application/x-ndjson 流（§1.7）。逐行写，long:N 产生 N 字符单行，不截断。
     */
    private static void ndjson(HttpExchange ex) throws IOException {
        if (!"POST".equalsIgnoreCase(ex.getRequestMethod())) {
            sendError(ex, 405, "INVALID_REQUEST", "method not allowed");
            return;
        }
        String userText;
        String taskId;
        try {
            byte[] req = ex.getRequestBody().readAllBytes();
            JsonNode root = JSON.readTree(req);
            taskId = root.path("taskId").asText("task-1");
            userText = extractText(root.path("message"));
        } catch (Exception e) {
            sendError(ex, 400, "INVALID_REQUEST", "JSON parse failed: " + e.getMessage());
            return;
        }

        // 回显规则：普通输入原样；long:N 产生 N 个字符的单行
        String artifactText = echoText(userText);

        // 构造官方 Task（合规轨类型），用于日志/未来扩展；NDJSON 是内部兼容轨协议
        Task task = buildTask(taskId, "", userText);

        ex.getResponseHeaders().set("Content-Type", "application/x-ndjson; charset=UTF-8");
        ex.getResponseHeaders().set("Cache-Control", "no-cache");
        ex.getResponseHeaders().set("X-Accel-Buffering", "no");
        ex.sendResponseHeaders(200, 0); // chunked，不设 Content-Length

        try (OutputStream os = ex.getResponseBody()) {
            writeLine(os, statusLine("working", 0.5));
            os.flush();
            writeLine(os, artifactLine("art-1", artifactText));
            os.flush();
            writeLine(os, doneLine("completed"));
            os.flush();
        }
        // task.status() 非空，证明官方 record 参与运行
        assert task.status().state() == TaskState.TASK_STATE_COMPLETED;
    }

    /** 从 message 对象提取文本（兼容 parts[].text 与直接 text/text 字段）。 */
    private static String extractText(JsonNode message) {
        if (message == null || message.isMissingNode() || message.isNull()) return "";
        if (message.has("text")) return message.path("text").asText("");
        JsonNode parts = message.path("parts");
        if (parts.isArray()) {
            StringBuilder sb = new StringBuilder();
            for (JsonNode p : parts) {
                if ("text".equals(p.path("kind").asText("")) || p.has("text")) {
                    sb.append(p.path("text").asText(""));
                }
            }
            return sb.toString();
        }
        return message.path("content").asText("");
    }

    /** 回显逻辑：long:N → N 个字符的单行；否则原样返回（UTF-8，中文不乱码）。 */
    static String echoText(String input) {
        if (input == null) return "";
        if (input.startsWith("long:")) {
            try {
                int n = Integer.parseInt(input.substring(5).trim());
                if (n < 0) n = 0;
                // 用可打印 ASCII 填充，保证是「单行」且长度精确；不自行截断
                char[] buf = new char[n];
                java.util.Arrays.fill(buf, 'A');
                return new String(buf);
            } catch (NumberFormatException e) {
                return input;
            }
        }
        return input;
    }

    private static String statusLine(String state, double progress) {
        ObjectNode o = JSON.createObjectNode();
        o.put("type", "status");
        o.put("state", state);
        o.put("progress", progress);
        return o.toString();
    }

    private static String artifactLine(String id, String text) {
        ObjectNode o = JSON.createObjectNode();
        o.put("type", "artifact");
        o.put("artifactId", id);
        o.put("text", text);
        return o.toString();
    }

    private static String doneLine(String state) {
        ObjectNode o = JSON.createObjectNode();
        o.put("type", "done");
        o.put("state", state);
        return o.toString();
    }

    private static void writeLine(OutputStream os, String line) throws IOException {
        os.write(line.getBytes(StandardCharsets.UTF_8));
        os.write('\n');
    }

    // ------------------------------------------------------------------------
    // 合规轨：最小 JSON-RPC message/send（用官方类型构造响应）
    // ------------------------------------------------------------------------

    private static void jsonRpc(HttpExchange ex) throws IOException {
        if (!"POST".equalsIgnoreCase(ex.getRequestMethod())) {
            sendError(ex, 405, "INVALID_REQUEST", "method not allowed");
            return;
        }
        try {
            byte[] req = ex.getRequestBody().readAllBytes();
            JsonNode root = JSON.readTree(req);
            String id = root.path("id").asText("1");
            String method = root.path("method").asText("");
            JsonNode params = root.path("params");
            JsonNode result;
            if ("message/send".equals(method) || "SendMessage".equals(method)) {
                String text = extractText(params.path("message"));
                String artifactText = echoText(text);
                // 用官方 Task record 构造领域对象，再按契约字段逐个读取序列化。
                // 不用 valueToTree(t)：TaskStatus.timestamp() 是 OffsetDateTime，
                // Jackson 默认无 JavaTimeModule（避免额外依赖），逐字段映射更稳。
                Task t = buildTask("task-" + id, "", text);
                ObjectNode taskJson = JSON.createObjectNode();
                taskJson.put("kind", "task");
                taskJson.put("id", t.id());
                if (t.contextId() != null) taskJson.put("contextId", t.contextId());
                ObjectNode status = taskJson.putObject("status");
                status.put("state", stateString(t.status().state()));
                var arts = taskJson.putArray("artifacts");
                ObjectNode a = arts.addObject();
                a.put("artifactId", "art-1");
                a.put("text", artifactText);
                result = taskJson;
            } else {
                ObjectNode err = JSON.createObjectNode();
                err.put("code", -32601);
                err.put("message", "method not found");
                ObjectNode rpcErr = JSON.createObjectNode();
                rpcErr.put("jsonrpc", "2.0");
                rpcErr.put("id", id);
                rpcErr.set("error", err);
                sendBytes(ex, 200, JSON.writeValueAsBytes(rpcErr), "application/json; charset=UTF-8");
                return;
            }
            ObjectNode resp = JSON.createObjectNode();
            resp.put("jsonrpc", "2.0");
            resp.put("id", id);
            resp.set("result", result);
            sendBytes(ex, 200, JSON.writeValueAsBytes(resp), "application/json; charset=UTF-8");
        } catch (Exception e) {
            sendError(ex, 400, "INVALID_REQUEST", "JSON parse failed: " + e.getMessage());
        }
    }

    private static String stateString(TaskState s) {
        return switch (s) {
            case TASK_STATE_SUBMITTED -> "submitted";
            case TASK_STATE_WORKING -> "working";
            case TASK_STATE_INPUT_REQUIRED -> "input-required";
            case TASK_STATE_AUTH_REQUIRED -> "auth-required";
            case TASK_STATE_CANCELED -> "canceled";
            case TASK_STATE_FAILED -> "failed";
            case TASK_STATE_REJECTED -> "rejected";
            case TASK_STATE_UNSPECIFIED -> "unknown";
            default -> "completed";
        };
    }

    private static void sendJson(HttpExchange ex, int status, String body, String ct) throws IOException {
        sendBytes(ex, status, body.getBytes(StandardCharsets.UTF_8), ct);
    }

    private static void sendBytes(HttpExchange ex, int status, byte[] body, String ct) throws IOException {
        ex.getResponseHeaders().set("Content-Type", ct);
        ex.sendResponseHeaders(status, body.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(body);
        }
    }

    private static void sendError(HttpExchange ex, int status, String code, String msg) throws IOException {
        ObjectNode err = JSON.createObjectNode();
        err.put("code", code);
        err.put("message", msg);
        err.put("httpStatus", status);
        ObjectNode root = JSON.createObjectNode();
        root.set("error", err);
        sendJson(ex, status, root.toString(), "application/json; charset=UTF-8");
    }

    private JavaAgent() {}
}
