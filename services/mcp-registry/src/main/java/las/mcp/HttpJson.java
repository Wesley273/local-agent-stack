package las.mcp;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpHandler;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.UUID;

/**
 * HTTP + JSON 工具层（UTF-8 显式 + 统一错误信封）。
 *
 * <p>统一错误信封按 docs/01-协议契约.md §0.3：
 * {"error":{"code","message","requestId","httpStatus","detail"}}。
 * MCP 注册中心用到的错误码：INVALID_REQUEST / MCP_TOOL_NOT_FOUND / INTERNAL_ERROR /
 * UPSTREAM_ERROR（回放连接失败）。
 */
public final class HttpJson {

    public static final ObjectMapper MAPPER = new ObjectMapper();

    private HttpJson() {}

    public static final class ApiException extends RuntimeException {
        public final String code;
        public final int httpStatus;
        public final JsonNode detail;

        public ApiException(int httpStatus, String code, String message, JsonNode detail) {
            super(message);
            this.httpStatus = httpStatus;
            this.code = code;
            this.detail = detail == null ? MAPPER.createObjectNode() : detail;
        }

        public static ApiException invalid(String message) {
            return new ApiException(400, "INVALID_REQUEST", message, null);
        }
        public static ApiException toolNotFound(String message) {
            return new ApiException(404, "MCP_TOOL_NOT_FOUND", message, null);
        }
        public static ApiException internal(String message) {
            return new ApiException(500, "INTERNAL_ERROR", message, null);
        }
        public static ApiException upstream(String message) {
            return new ApiException(502, "UPSTREAM_ERROR", message, null);
        }
    }

    @FunctionalInterface
    public interface Handler {
        void handle(HttpExchange ex) throws Exception;
    }

    public static HttpHandler wrap(Handler h) {
        return ex -> {
            try {
                h.handle(ex);
            } catch (ApiException e) {
                sendError(ex, e);
            } catch (Exception e) {
                sendError(ex, ApiException.internal(e.toString()));
            }
        };
    }

    public static String requestId(HttpExchange ex) {
        String v = ex.getRequestHeaders().getFirst("X-Request-Id");
        if (v == null || v.isEmpty()) v = UUID.randomUUID().toString().replace("-", "");
        return v;
    }

    /** 显式 UTF-8 读请求体。 */
    public static String readBody(HttpExchange ex) throws IOException {
        try (InputStream is = ex.getRequestBody()) {
            return new String(is.readAllBytes(), StandardCharsets.UTF_8);
        }
    }

    public static JsonNode parse(String body) {
        if (body == null || body.isEmpty()) throw ApiException.invalid("empty JSON body");
        try {
            JsonNode n = MAPPER.readTree(body);
            if (n == null || n.isNull()) throw ApiException.invalid("JSON body must be an object");
            return n;
        } catch (JsonProcessingException e) {
            throw ApiException.invalid("JSON parse failed: " + e.getOriginalMessage());
        }
    }

    public static ObjectNode parseObject(String body) {
        JsonNode n = parse(body);
        if (!n.isObject()) throw ApiException.invalid("JSON body must be an object");
        return (ObjectNode) n;
    }

    public static void send(HttpExchange ex, int status, JsonNode body) throws IOException {
        byte[] out = body == null ? new byte[0] : MAPPER.writeValueAsBytes(body);
        ex.getResponseHeaders().set("Content-Type", "application/json; charset=UTF-8");
        if (out.length == 0) {
            ex.sendResponseHeaders(status, -1);
            ex.close();
            return;
        }
        ex.sendResponseHeaders(status, out.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(out);
        }
    }

    /** 204（JSON-RPC 通知）。 */
    public static void sendNoBody(HttpExchange ex, int status) throws IOException {
        ex.sendResponseHeaders(status, -1);
        ex.close();
    }

    public static void sendError(HttpExchange ex, ApiException e) throws IOException {
        ObjectNode err = MAPPER.createObjectNode();
        err.put("code", e.code);
        err.put("message", e.getMessage());
        err.put("requestId", requestId(ex));
        err.put("httpStatus", e.httpStatus);
        err.set("detail", e.detail);
        ObjectNode root = MAPPER.createObjectNode();
        root.set("error", err);
        send(ex, e.httpStatus, root);
    }
}
