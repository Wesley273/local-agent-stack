package las.mcp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

/**
 * §5.1 MCP Tool 模型（统一高码 / 低码 / 三方工具）。
 *
 * <pre>
 * {
 *   "name": "getUserById",
 *   "description": "GET /users/{id} — 根据 ID 查询用户",
 *   "inputSchema": {"type":"object","properties":{...},"required":["id"]},
 *   "annotations": {"title","readOnlyHint","destructiveHint","idempotentHint","openWorldHint"},
 *   "source": {"type":"openapi","path","method","operationId","baseUrl"}
 * }
 * </pre>
 *
 * <p>额外字段（Registry 内部用，不出现在 tools/list 的 Tool 契约字段里可省略）：
 * version / origin(high-code|low-code|third-party) / tags / registeredAt。
 * 这些追加字段不影响 §5.1 必填字段，数据面/客户端按名字取用即可。
 */
public final class Tool {

    /** 从 JSON 构造（深拷贝）。 */
    public static ObjectNode from(JsonNode n) {
        if (n == null || !n.isObject()) {
            throw HttpJson.ApiException.invalid("tool must be an object");
        }
        ObjectNode t = ((ObjectNode) n).deepCopy();
        if (!t.has("name") || t.get("name").asText().isEmpty()) {
            throw HttpJson.ApiException.invalid("tool.name is required");
        }
        if (!t.has("inputSchema") || !t.get("inputSchema").isObject()) {
            throw HttpJson.ApiException.invalid("tool.inputSchema is required");
        }
        if (!t.has("source") || !t.get("source").isObject()) {
            ObjectNode src = HttpJson.MAPPER.createObjectNode();
            src.put("type", "manual");
            t.set("source", src);
        }
        if (!t.has("annotations") || !t.get("annotations").isObject()) {
            t.set("annotations", defaultAnnotations(""));
        }
        if (!t.has("version")) t.put("version", "1.0.0");
        if (!t.has("origin")) t.put("origin", "third-party");
        if (!t.has("registeredAt")) t.put("registeredAt", System.currentTimeMillis());
        return t;
    }

    /** 构造一个由 OpenAPI 转换来的 Tool。 */
    static ObjectNode openapi(String name, String description, ObjectNode inputSchema,
                              ObjectNode annotations, ObjectNode source, String specId) {
        ObjectNode t = HttpJson.MAPPER.createObjectNode();
        t.put("name", name);
        t.put("description", description);
        t.set("inputSchema", inputSchema);
        t.set("annotations", annotations);
        t.set("source", source);
        t.put("version", "1.0.0");
        t.put("origin", "openapi");
        t.put("specId", specId == null ? "" : specId);
        t.put("registeredAt", System.currentTimeMillis());
        return t;
    }

    static ObjectNode defaultAnnotations(String title) {
        ObjectNode a = HttpJson.MAPPER.createObjectNode();
        a.put("title", title == null ? "" : title);
        a.put("readOnlyHint", false);
        a.put("destructiveHint", false);
        a.put("idempotentHint", false);
        a.put("openWorldHint", false);
        return a;
    }

    /** source.type / path / method / baseUrl 读取助手。 */
    public static String sourceField(ObjectNode t, String f, String def) {
        JsonNode s = t.get("source");
        if (s != null && s.isObject() && s.has(f) && !s.get(f).isNull()) {
            return s.get(f).asText();
        }
        return def;
    }

    private Tool() {}
}
