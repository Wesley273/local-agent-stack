package las.mcp;

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
 * <p>支持：单次 / 批量 / 通知(204) / 标准错误码
 * -32700 parse error、 -32600 invalid request、 -32601 method not found、
 * -32602 invalid params、 -32603 internal error。
 *
 * <p>MCP 注册中心的 POST /mcp 用 McpApi 注册方法（initialize / tools/list /
 * tools/call / tools/get / ping）到本 Dispatcher。
 */
public final class JsonRpc {

    @FunctionalInterface
    public interface Method extends Function<JsonNode, JsonNode> {}

    /** 携带明确 JSON-RPC code 的异常。 */
    public static final class JsonRpcException extends RuntimeException {
        final int code;
        final String rpcMessage;
        final JsonNode data;

        JsonRpcException(int code, String rpcMessage) {
            this(code, rpcMessage, null);
        }
        JsonRpcException(int code, String rpcMessage, JsonNode data) {
            super(rpcMessage);
            this.code = code;
            this.rpcMessage = rpcMessage;
            this.data = data;
        }
        public static JsonRpcException invalidParams(String m) { return new JsonRpcException(-32602, m); }
        public static JsonRpcException methodNotFound(String m) { return new JsonRpcException(-32601, m); }
        public static JsonRpcException internal(String m) { return new JsonRpcException(-32603, m); }
    }

    public static final class Dispatcher {
        private final ObjectMapper mapper = HttpJson.MAPPER;
        private final Map<String, Method> methods = new LinkedHashMap<>();

        public synchronized Dispatcher register(String name, Method method) {
            methods.put(name, method);
            return this;
        }

        /** 处理 UTF-8 JSON 请求体；通知（无响应）返回 null。 */
        public String handle(String requestBody) {
            JsonNode root;
            try {
                root = mapper.readTree(requestBody);
            } catch (JsonProcessingException e) {
                return errorNode(null, -32700, "Parse error", null).toString();
            }
            if (root == null || root.isNull()) {
                return errorNode(null, -32600, "Invalid Request", null).toString();
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
            if (req == null || !req.isObject()) return errorNode(null, -32600, "Invalid Request", null);
            String version = req.path("jsonrpc").asText(null);
            if (!"2.0".equals(version)) return errorNode(idOf(req), -32600,
                    "Invalid Request: jsonrpc must be \"2.0\"", null);
            String name = req.path("method").asText(null);
            if (name == null || name.isEmpty() || !req.get("method").isTextual()) {
                return errorNode(idOf(req), -32600, "Invalid Request: missing method", null);
            }
            boolean notification = !req.has("id") || req.get("id").isNull();
            JsonNode id = idOf(req);
            JsonNode params = req.has("params") ? req.get("params") : mapper.createObjectNode();
            if (!params.isObject() && !params.isArray() && !params.isNull()) {
                return errorNode(id, -32602, "Invalid params", null);
            }
            Method method;
            synchronized (this) { method = methods.get(name); }
            if (method == null) {
                return notification ? null
                        : errorNode(id, -32601, "Method not found: " + name, null);
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
                return notification ? null : errorNode(id, e.code, e.rpcMessage, e.data);
            } catch (HttpJson.ApiException e) {
                // MCP_TOOL_NOT_FOUND 等契约错误映射为 -32602，data.code 保留契约 code
                ObjectNode data = mapper.createObjectNode();
                data.put("code", e.code);
                return notification ? null : errorNode(id, -32602, e.getMessage(), data);
            } catch (RuntimeException e) {
                return notification ? null : errorNode(id, -32603, "Internal error: " + e, null);
            }
        }

        private JsonNode idOf(JsonNode req) {
            return req.has("id") ? req.get("id") : null;
        }

        private ObjectNode errorNode(JsonNode id, int code, String message, JsonNode data) {
            ObjectNode resp = mapper.createObjectNode();
            resp.put("jsonrpc", "2.0");
            ObjectNode err = mapper.createObjectNode();
            err.put("code", code);
            err.put("message", message);
            if (data != null) err.set("data", data);
            else {
                ObjectNode d = mapper.createObjectNode();
                d.put("code", code == -32603 ? "INTERNAL_ERROR" : "INVALID_REQUEST");
                err.set("data", d);
            }
            resp.set("error", err);
            resp.set("id", id == null ? mapper.nullNode() : id);
            return resp;
        }

        public ObjectMapper mapper() { return mapper; }
    }

    /** POST /mcp 的 HTTP 接入层（UTF-8，通知 204）。 */
    public static void handle(HttpExchange ex, Dispatcher d) throws Exception {
        if (!"POST".equalsIgnoreCase(ex.getRequestMethod())) {
            ex.getResponseHeaders().set("Allow", "POST");
            throw HttpJson.ApiException.invalid("POST /mcp only (Content-Type: application/json)");
        }
        byte[] raw;
        try (java.io.InputStream is = ex.getRequestBody()) {
            raw = is.readAllBytes();
        }
        String body = new String(raw, StandardCharsets.UTF_8);
        String resp = d.handle(body);
        if (resp == null) {
            HttpJson.sendNoBody(ex, 204);   // 通知：无响应体
            return;
        }
        HttpJson.send(ex, 200, HttpJson.MAPPER.readTree(resp));
    }

    private JsonRpc() {}
}
