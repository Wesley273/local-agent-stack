package las.cp;

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
 * HTTP + JSON 工具层（UTF-8 显式、统一错误信封）。
 *
 * <p>平台默认是 GBK，因此这里所有读写都强制 UTF-8：
 * 请求体 new String(bytes, StandardCharsets.UTF_8)，响应头 charset=UTF-8，
 * 响应体用 Jackson writeValueAsBytes（输出 UTF-8 字节）。
 *
 * <p>统一错误信封严格按 docs/01-协议契约.md §0.3：
 * {"error":{"code","message","requestId","httpStatus","detail"}}，
 * code 只能取 §0.3 表内的值，不许自造。
 */
public final class HttpJson {

    /** 全局共享的 ObjectMapper（线程安全）。 */
    public static final ObjectMapper MAPPER = new ObjectMapper();

    private HttpJson() {}

    // ------------------------------------------------------------------ 错误

    /** 业务异常：携带契约 §0.3 的 code / httpStatus，由 wrap() 统一转成错误信封。 */
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

        public static ApiException invalid(String message, JsonNode detail) {
            return new ApiException(400, "INVALID_REQUEST", message, detail);
        }

        public static ApiException notFound(String code, String message) {
            return new ApiException(404, code, message, null);
        }

        public static ApiException internal(String message) {
            return new ApiException(500, "INTERNAL_ERROR", message, null);
        }
    }

    /** 处理器接口：实现 handle()，由 wrap() 兜底异常。 */
    @FunctionalInterface
    public interface Handler {
        void handle(HttpExchange ex) throws Exception;
    }

    /** 把 Handler 包装成 HttpHandler，统一异常 -> 错误信封。 */
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

    // ------------------------------------------------------------------ 请求

    /** X-Request-Id 透传；缺失则生成无横线 UUID（契约 §0.2）。 */
    public static String requestId(HttpExchange ex) {
        String v = ex.getRequestHeaders().getFirst("X-Request-Id");
        if (v == null || v.isEmpty()) {
            v = UUID.randomUUID().toString().replace("-", "");
        }
        return v;
    }

    /** 显式 UTF-8 读请求体。 */
    public static String readBody(HttpExchange ex) throws IOException {
        try (InputStream is = ex.getRequestBody()) {
            byte[] raw = is.readAllBytes();   // JDK 9+
            return new String(raw, StandardCharsets.UTF_8);
        }
    }

    /** 解析 JSON；失败抛 INVALID_REQUEST(400)。 */
    public static JsonNode parse(String body) {
        if (body == null || body.isEmpty()) {
            throw ApiException.invalid("empty JSON body");
        }
        try {
            JsonNode n = MAPPER.readTree(body);
            if (n == null || n.isNull()) {
                throw ApiException.invalid("JSON body must be an object");
            }
            return n;
        } catch (JsonProcessingException e) {
            throw ApiException.invalid("JSON parse failed: " + e.getOriginalMessage());
        }
    }

    /** 解析并断言是 object。 */
    public static ObjectNode parseObject(String body) {
        JsonNode n = parse(body);
        if (!n.isObject()) {
            throw ApiException.invalid("JSON body must be an object");
        }
        return (ObjectNode) n;
    }

    // ------------------------------------------------------------------ 响应

    /** 发送 JSON 响应（UTF-8）。body=null 表示不发送响应体。 */
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

    /** 发送无 body 响应（用于 304 Not Modified）。调用前自行设置 ETag。 */
    public static void sendNoBody(HttpExchange ex, int status) throws IOException {
        ex.sendResponseHeaders(status, -1);
        ex.close();
    }

    /** 统一错误信封。 */
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

    /** 写操作统一的成功响应。 */
    public static ObjectNode ok(long revision, String key, JsonNode value) {
        ObjectNode r = MAPPER.createObjectNode();
        r.put("ok", true);
        r.put("revision", revision);
        if (key != null) {
            r.set(key, value == null ? MAPPER.nullNode() : value);
        }
        return r;
    }

    /** 深拷贝，避免把内部可变节点暴露给外部。 */
    public static <T extends JsonNode> T copy(T n) {
        @SuppressWarnings("unchecked")
        T c = (T) n.deepCopy();
        return c;
    }
}
