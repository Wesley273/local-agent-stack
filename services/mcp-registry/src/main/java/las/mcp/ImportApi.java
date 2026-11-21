package las.mcp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpExchange;

import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * 注册中心 REST 端点（契约 §5.2）：
 * <pre>
 *   POST   /registry/import             导入 OpenAPI 文档（转换 + 落盘）
 *   POST   /registry/validate           只校验/转换不导入
 *   GET    /registry/tools              列出工具（支持 ?q= 检索）
 *   GET    /registry/tools/{name}       单个工具
 *   POST   /registry/tools              手动注册工具（高码/低码/三方统一）
 *   DELETE /registry/tools/{name}       删除工具
 *   GET    /registry/specs              已导入文档列表
 * </pre>
 */
public final class ImportApi {

    private final Registry registry;

    public ImportApi(Registry registry) {
        this.registry = registry;
    }

    /** 处理 /registry/ 前缀下的所有路径。 */
    public void handle(HttpExchange ex) throws Exception {
        String method = ex.getRequestMethod().toUpperCase();
        String path = ex.getRequestURI().getPath();
        String[] seg = path.split("/", -1);   // ["", "registry", "import", ...]
        String resource = seg.length > 2 ? seg[2] : "";
        String name = seg.length > 3 ? decode(seg[3]) : null;
        Map<String, String> q = query(ex);

        switch (resource) {
            case "import":   handleImport(ex, method); return;
            case "validate": handleValidate(ex, method); return;
            case "tools":    handleTools(ex, method, name, q); return;
            case "specs":    handleSpecs(ex, method); return;
            default: throw HttpJson.ApiException.invalid("unknown /registry path: " + path);
        }
    }

    // ------------------------------------------------------------------ import / validate

    private void handleImport(HttpExchange ex, String m) throws Exception {
        if (!m.equals("POST")) throw methodNotAllowed(m);
        ObjectNode body = HttpJson.parseObject(HttpJson.readBody(ex));
        ImportSpec in = parseSpec(body);
        OpenApiConverter.Result result = OpenApiConverter.convert(in.spec, in.baseUrl, in.source);
        List<String> names = registry.putAll(result.tools, in.replace);
        registry.addSpec(in.source, names.size(), in.baseUrl);

        // 返回契约要求的 {"imported":N,"tools":[...],"skipped":[...]}
        ObjectNode out = result.response.deepCopy();
        out.put("source", in.source == null ? "" : in.source);
        out.put("baseUrl", in.baseUrl == null ? "" : in.baseUrl);
        HttpJson.send(ex, 200, out);
    }

    private void handleValidate(HttpExchange ex, String m) throws Exception {
        if (!m.equals("POST")) throw methodNotAllowed(m);
        ObjectNode body = HttpJson.parseObject(HttpJson.readBody(ex));
        ImportSpec in = parseSpec(body);
        OpenApiConverter.Result result = OpenApiConverter.convert(in.spec, in.baseUrl, in.source);
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.put("valid", true);
        out.put("imported", result.response.path("imported").asInt());
        out.set("tools", result.response.get("tools").deepCopy());
        out.set("skipped", result.response.get("skipped").deepCopy());
        // 校验端点额外返回首个工具的 schema，便于检查 $ref 是否展开
        if (!result.tools.isEmpty()) {
            out.set("sampleTool", result.tools.get(0).deepCopy());
        }
        HttpJson.send(ex, 200, out);
    }

    /** 解析 import body：{"spec":{...},"baseUrl":...,"source":...,"replace":false}。 */
    private ImportSpec parseSpec(ObjectNode body) {
        ImportSpec in = new ImportSpec();
        JsonNode specNode = body.get("spec");
        if (specNode == null && body.has("openapi")) {
            // 也允许直接把 OpenAPI 文档作为 body
            specNode = body;
        }
        if (specNode == null || !specNode.isObject()) {
            throw HttpJson.ApiException.invalid("body.spec (OpenAPI 3.0 object) is required");
        }
        in.spec = specNode;
        in.baseUrl = body.path("baseUrl").asText("http://127.0.0.1:9200");
        in.source = body.path("source").asText("openapi");
        in.replace = body.path("replace").asBoolean(false);
        return in;
    }

    private static final class ImportSpec {
        JsonNode spec;
        String baseUrl;
        String source;
        boolean replace;
    }

    // ------------------------------------------------------------------ tools / specs

    private void handleTools(HttpExchange ex, String m, String name, Map<String, String> q)
            throws Exception {
        if (name == null) {
            switch (m) {
                case "GET": {
                    ObjectNode out = HttpJson.MAPPER.createObjectNode();
                    String query = q.get("q");
                    out.set("tools", query == null ? registry.list() : registry.search(query));
                    out.put("count", registry.size());
                    HttpJson.send(ex, 200, out);
                    return;
                }
                case "POST": {
                    ObjectNode rec = registry.register(
                            HttpJson.parseObject(HttpJson.readBody(ex)));
                    HttpJson.send(ex, 201, rec);
                    return;
                }
                default: throw methodNotAllowed(m);
            }
        }
        switch (m) {
            case "GET":    HttpJson.send(ex, 200, registry.get(name)); return;
            case "DELETE": HttpJson.send(ex, 200, registry.delete(name)); return;
            default: throw methodNotAllowed(m);
        }
    }

    private void handleSpecs(HttpExchange ex, String m) throws Exception {
        if (!m.equals("GET")) throw methodNotAllowed(m);
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("specs", registry.specs());
        HttpJson.send(ex, 200, out);
    }

    // ------------------------------------------------------------------ helpers

    private static HttpJson.ApiException methodNotAllowed(String m) {
        return new HttpJson.ApiException(405, "INVALID_REQUEST",
                "method not allowed: " + m, null);
    }

    private static String decode(String s) {
        try { return URLDecoder.decode(s, StandardCharsets.UTF_8.name()); }
        catch (Exception e) { return s; }
    }

    private static Map<String, String> query(HttpExchange ex) {
        Map<String, String> m = new HashMap<>();
        String q = ex.getRequestURI().getRawQuery();
        if (q == null) return m;
        for (String p : q.split("&")) {
            int i = p.indexOf('=');
            try {
                m.put(decode(i < 0 ? p : p.substring(0, i)),
                        i < 0 ? "" : decode(p.substring(i + 1)));
            } catch (Exception ignored) { }
        }
        return m;
    }
}
