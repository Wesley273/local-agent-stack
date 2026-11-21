package las.mcp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.io.IOException;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.Map;

/**
 * ★ tools/call 真实 HTTP 回放（契约 §5.2 末段 / 能力矩阵 C6 / E19）。
 *
 * <p>用 Tool.source 把 name + arguments 构造成真实 HTTP 请求：
 *  - path 参数：替换 URL 里的 {id} 占位符（URL 编码）
 *  - query 参数：拼接 ?a=1&b=2
 *  - header 参数：写入请求头（in:header）
 *  - body 参数：组装成 JSON 对象发送（in:body 的所有属性）
 *  - 打到 source.baseUrl（默认 http://127.0.0.1:9200 的 /tools/echo 端点）
 *
 * <p>返回 {"content":[{"type":"text","text":<HTTP响应体>}],"isError":false}；
 * HTTP 非 2xx → isError:true（仍把响应体放进 content.text，便于排查）。
 * 连接失败 → isError:true + text 说明（不抛 MCP JSON-RPC error，保持契约的 isError 语义）。
 *
 * <p>这是真的发 HTTP 请求，不是返回假数据。
 */
public final class Replay {

    /** 回放超时（毫秒）。 */
    private static final int TIMEOUT = 10000;

    private Replay() {}

    /** 执行一个工具调用，返回 §5.1 的 tools/call result。 */
    public static ObjectNode call(ObjectNode tool, JsonNode arguments) {
        JsonNode args = (arguments == null || arguments.isNull())
                ? HttpJson.MAPPER.createObjectNode() : arguments;
        if (!args.isObject()) {
            throw HttpJson.ApiException.invalid("arguments must be an object");
        }

        String baseUrl = Tool.sourceField(tool, "baseUrl", "http://127.0.0.1:9200");
        String method = Tool.sourceField(tool, "method", "get").toUpperCase();
        String path = Tool.sourceField(tool, "path", "/");
        String replayPath = Tool.sourceField(tool, "replayPath", "");

        ObjectNode props = tool.path("inputSchema").path("properties").isObject()
                ? (ObjectNode) tool.get("inputSchema").get("properties")
                : HttpJson.MAPPER.createObjectNode();

        // ---- 1. path 参数替换 ----
        String resolvedPath = path;
        ObjectNode body = HttpJson.MAPPER.createObjectNode();
        StringBuilder query = new StringBuilder();
        java.util.Map<String, String> headers = new java.util.LinkedHashMap<>();

        for (java.util.Iterator<Map.Entry<String, JsonNode>> it = args.fields(); it.hasNext(); ) {
            Map.Entry<String, JsonNode> e = it.next();
            String key = e.getKey();
            JsonNode val = e.getValue();
            JsonNode prop = props.get(key);
            String in = prop != null && prop.isObject() ? prop.path("in").asText("query") : "query";
            if (in.equals("path")) {
                resolvedPath = resolvedPath.replace("{" + key + "}", enc(val.asText()));
            } else if (in.equals("header")) {
                headers.put(key, val.asText());
            } else if (in.equals("body")) {
                body.set(key, val.deepCopy());
            } else { // query（未知参数默认 query）
                if (query.length() > 0) query.append('&');
                query.append(enc(key)).append('=').append(enc(val.asText()));
            }
        }

        // ---- 2. 目标 URL ----
        // 契约要求 baseUrl 默认指向 Mock LLM 的 /tools/echo 端点：
        // 若 source 指定了 replayPath（或 baseUrl 未显式给出 path），用 replayPath 作为实际请求路径，
        // 同时把原始 OpenAPI path/method 作为 query 元信息带过去，便于 Mock 回显验证。
        String targetPath;
        if (!replayPath.isEmpty()) {
            targetPath = replayPath;
            if (query.length() > 0) query.append('&');
            query.append("_tool_path=").append(enc(resolvedPath));
            query.append("&_tool_method=").append(enc(method));
        } else {
            targetPath = resolvedPath;
        }
        String urlStr = joinUrl(baseUrl, targetPath);
        if (query.length() > 0) urlStr += (urlStr.contains("?") ? "&" : "?") + query;

        // ---- 3. 真实 HTTP 请求 ----
        HttpURLConnection conn = null;
        int status;
        String responseText;
        try {
            URL url = new URL(urlStr);
            conn = (HttpURLConnection) url.openConnection();
            conn.setRequestMethod(method);
            conn.setConnectTimeout(TIMEOUT);
            conn.setReadTimeout(TIMEOUT);
            conn.setRequestProperty("Accept", "application/json");
            conn.setRequestProperty("X-Request-Id",
                    java.util.UUID.randomUUID().toString().replace("-", ""));
            for (Map.Entry<String, String> h : headers.entrySet()) {
                conn.setRequestProperty(h.getKey(), h.getValue());
            }
            if (!body.isEmpty() && (method.equals("POST") || method.equals("PUT")
                    || method.equals("PATCH") || method.equals("DELETE"))) {
                byte[] out = HttpJson.MAPPER.writeValueAsBytes(body);
                conn.setDoOutput(true);
                conn.setRequestProperty("Content-Type", "application/json; charset=UTF-8");
                conn.setFixedLengthStreamingMode(out.length);
                try (OutputStream os = conn.getOutputStream()) {
                    os.write(out);
                }
            } else {
                conn.setDoOutput(false);
            }
            status = conn.getResponseCode();
            java.io.InputStream is = status >= 200 && status < 300
                    ? conn.getInputStream() : conn.getErrorStream();
            if (is == null) {
                responseText = "";
            } else {
                try (java.io.InputStream s = is) {
                    responseText = new String(s.readAllBytes(), StandardCharsets.UTF_8);
                }
            }
        } catch (IOException e) {
            // 回放连接失败：按契约用 isError:true 表达，不抛 JSON-RPC 错误
            return result("UPSTREAM_ERROR: " + e, true);
        } finally {
            if (conn != null) conn.disconnect();
        }

        boolean isError = status < 200 || status >= 300;
        // 非 2xx 时把状态码也放进文本，方便 E19 排查
        String text = isError ? "HTTP " + status + " " + responseText : responseText;
        return result(text, isError);
    }

    /** 组装 §5.1 的 tools/call 结果对象。 */
    public static ObjectNode result(String text, boolean isError) {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        ArrayNode content = HttpJson.MAPPER.createArrayNode();
        ObjectNode item = HttpJson.MAPPER.createObjectNode();
        item.put("type", "text");
        item.put("text", text == null ? "" : text);
        content.add(item);
        out.set("content", content);
        out.put("isError", isError);
        return out;
    }

    private static String joinUrl(String base, String path) {
        String b = base.endsWith("/") ? base.substring(0, base.length() - 1) : base;
        String p = path.startsWith("/") ? path : "/" + path;
        // base 可能已含路径（如 http://host/tools/echo），此时直接拼接
        return b + p;
    }

    private static String enc(String s) {
        try {
            return URLEncoder.encode(s, StandardCharsets.UTF_8.name());
        } catch (Exception e) {
            return s;
        }
    }
}
