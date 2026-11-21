package las.cp;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpExchange;

import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.function.Function;

/**
 * JSON-RPC 2.0 处理器（复用并改进 _probe/java 的 probe.JsonRpcServer 骨架）。
 *
 * <p>支持：单次请求、批量请求、通知（无 id 不回响应，HTTP 204）、标准错误码
 * -32700 / -32600 / -32601 / -32602 / -32603。
 *
 * <p>控制面主协议是 REST（/v1/*），但额外提供 POST /rpc 便于脚本化管理：
 * 方法 cp.config / cp.agents / cp.policies / cp.models / cp.rateLimits /
 * cp.guardrails / cp.events / cp.health / cp.bump（触发 revision++）。
 *
 * <p>改进点：错误对象带 data.code（映射契约 §0.3 的 INVALID_REQUEST 等），
 * 与 §1.4 的 JSON-RPC 错误形态对齐。
 */
public final class JsonRpc {

    @FunctionalInterface
    public interface Method extends Function<JsonNode, JsonNode> {}

    public static final class JsonRpcException extends RuntimeException {
        final int code;
        final String rpcMessage;
        JsonRpcException(int code, String rpcMessage) {
            super(rpcMessage);
            this.code = code;
            this.rpcMessage = rpcMessage;
        }
    }

    /** 线程安全：方法注册表 + 单一 ObjectMapper。 */
    public static final class Dispatcher {
        private final ObjectMapper mapper = HttpJson.MAPPER;
        private final Map<String, Method> methods = new LinkedHashMap<>();

        public synchronized Dispatcher register(String name, Method method) {
            methods.put(name, method);
            return this;
        }

        /** 处理一段 UTF-8 JSON 请求体，返回响应体；通知返回 null。 */
        public String handle(String requestBody) {
            JsonNode root;
            try {
                root = mapper.readTree(requestBody);
            } catch (JsonProcessingException e) {
                return errorNode(null, -32700, "Parse error").toString();
            }
            if (root == null || root.isNull()) {
                return errorNode(null, -32600, "Invalid Request").toString();
            }
            if (root.isArray()) {
                ArrayNode batch = mapper.createArrayNode();
                for (JsonNode one : root) {
                    ObjectNode resp = handleOne(one);
                    if (resp != null) batch.add(resp);
                }
                return batch.isEmpty() ? null : batch.toString();
            }
            ObjectNode resp = handleOne(root);
            return resp == null ? null : resp.toString();
        }

        private ObjectNode handleOne(JsonNode req) {
            if (req == null || !req.isObject()) return errorNode(null, -32600, "Invalid Request");
            String version = req.path("jsonrpc").asText(null);
            if (!"2.0".equals(version)) return errorNode(idOf(req), -32600,
                    "Invalid Request: jsonrpc must be \"2.0\"");
            String name = req.path("method").asText(null);
            if (name == null || name.isEmpty() || !req.get("method").isTextual()) {
                return errorNode(idOf(req), -32600, "Invalid Request: missing method");
            }
            boolean notification = !req.has("id") || req.get("id").isNull();
            JsonNode id = idOf(req);
            JsonNode params = req.has("params") ? req.get("params") : mapper.createObjectNode();
            if (!params.isObject() && !params.isArray() && !params.isNull()) {
                return errorNode(id, -32602, "Invalid params");
            }
            Method method;
            synchronized (this) { method = methods.get(name); }
            if (method == null) {
                return notification ? null : errorNode(id, -32601, "Method not found: " + name);
            }
            try {
                JsonNode result = method.apply(params);
                if (notification) return null;
                ObjectNode resp = mapper.createObjectNode();
                resp.put("jsonrpc", "2.0");
                resp.set("result", result == null ? mapper.nullNode() : result);
                resp.set("id", id == null ? mapper.nullNode() : id);
                return resp;
            } catch (JsonRpcException e) {
                return notification ? null : errorNode(id, e.code, e.rpcMessage);
            } catch (RuntimeException e) {
                return notification ? null : errorNode(id, -32603, "Internal error: " + e);
            }
        }

        private JsonNode idOf(JsonNode req) {
            return req.has("id") ? req.get("id") : null;
        }

        private ObjectNode errorNode(JsonNode id, int code, String message) {
            ObjectNode resp = mapper.createObjectNode();
            resp.put("jsonrpc", "2.0");
            ObjectNode err = mapper.createObjectNode();
            err.put("code", code);
            err.put("message", message);
            ObjectNode data = mapper.createObjectNode();
            data.put("code", code == -32601 || code == -32600 || code == -32700 || code == -32602
                    ? "INVALID_REQUEST" : "INTERNAL_ERROR");
            err.set("data", data);
            resp.set("error", err);
            resp.set("id", id == null ? mapper.nullNode() : id);
            return resp;
        }

        public ObjectMapper mapper() { return mapper; }
    }

    /** POST /rpc 的 HTTP 接入层（UTF-8，通知 204）。 */
    public static void handle(HttpExchange ex, Dispatcher d) throws Exception {
        if (!"POST".equalsIgnoreCase(ex.getRequestMethod())) {
            ex.getResponseHeaders().set("Allow", "POST");
            throw HttpJson.ApiException.invalid("POST /rpc only");
        }
        byte[] raw;
        try (java.io.InputStream is = ex.getRequestBody()) {
            raw = is.readAllBytes();
        }
        String body = new String(raw, StandardCharsets.UTF_8);
        String resp = d.handle(body);
        if (resp == null) {
            ex.sendResponseHeaders(204, -1);   // 通知：无响应体
            ex.close();
            return;
        }
        HttpJson.send(ex, 200, HttpJson.MAPPER.readTree(resp));
    }

    private JsonRpc() {}
}
