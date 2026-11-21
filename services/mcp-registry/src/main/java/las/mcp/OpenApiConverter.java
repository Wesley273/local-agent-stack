package las.mcp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.util.ArrayList;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;

/**
 * ★ OpenAPI 3.0 → MCP Tool 自动转换引擎（契约 §5.2，简历核心能力 C2/C5）。
 *
 * <p>规则逐条实现：
 * 1. 遍历 paths 的每个 method（get/post/put/delete/patch），跳过无 operationId 的，记录 skipped[]。
 * 2. name = operationId；若重名加 "_get"/"_post" 等后缀。
 * 3. description = summary || description || "METHOD path"，拼接 path 与 method。
 * 4. inputSchema.properties 来源：
 *    - parameters[]（in: path|query|header），required 取参数自身 required；path 参数强制 required
 *    - requestBody.content["application/json"].schema.properties（in: body）
 *    - $ref 递归展开 / allOf 合并 / 数组 / 嵌套对象保留 —— 委托 SchemaResolver。
 * 5. annotations：get→readOnlyHint=true；delete→destructiveHint=true；put/get→idempotentHint=true。
 * 6. source 记录 path/method/operationId/baseUrl，供 Replay 回放。
 *
 * <p>返回 {"imported":N,"tools":[names...],"skipped":[{"path","method","reason"}]}，
 * 内部同时产出 Tool 列表供 Registry 落盘。
 */
public final class OpenApiConverter {

    private static final Set<String> METHODS = new HashSet<>(
            java.util.Arrays.asList("get", "post", "put", "delete", "patch"));

    /** 转换结果。 */
    public static final class Result {
        public final List<ObjectNode> tools = new ArrayList<>();
        public final ObjectNode response = HttpJson.MAPPER.createObjectNode();
    }

    private OpenApiConverter() {}

    /**
     * 主入口。
     * @param spec   OpenAPI 3.0 文档（JsonNode）
     * @param baseUrl source.baseUrl（回放目标，默认 http://127.0.0.1:9200）
     * @param specId  导入来源标识（写入 tool.specId）
     */
    public static Result convert(JsonNode spec, String baseUrl, String specId) {
        if (spec == null || !spec.isObject()) {
            throw HttpJson.ApiException.invalid("spec must be an OpenAPI object");
        }
        if (!spec.has("paths") || !spec.get("paths").isObject()) {
            throw HttpJson.ApiException.invalid("spec.paths is required (OpenAPI 3.0)");
        }
        SchemaResolver resolver = new SchemaResolver(spec);
        Result result = new Result();
        ArrayNode names = HttpJson.MAPPER.createArrayNode();
        ArrayNode skipped = HttpJson.MAPPER.createArrayNode();
        Set<String> usedNames = new HashSet<>();

        JsonNode paths = spec.get("paths");
        // LinkedHashMap 保持 paths 的声明顺序，输出稳定
        Map<String, JsonNode> ordered = new LinkedHashMap<>();
        paths.fields().forEachRemaining(e -> ordered.put(e.getKey(), e.getValue()));

        for (Map.Entry<String, JsonNode> pathEntry : ordered.entrySet()) {
            String path = pathEntry.getKey();
            JsonNode pathItem = pathEntry.getValue();
            if (!pathItem.isObject()) continue;

            // pathItem 级 parameters（OpenAPI 允许把公共参数提上来）
            List<JsonNode> commonParams = new ArrayList<>();
            if (pathItem.has("parameters") && pathItem.get("parameters").isArray()) {
                for (JsonNode p : pathItem.get("parameters")) commonParams.add(p);
            }

            Map<String, JsonNode> ops = new LinkedHashMap<>();
            pathItem.fields().forEachRemaining(e -> {
                if (METHODS.contains(e.getKey().toLowerCase())) ops.put(e.getKey(), e.getValue());
            });

            for (Map.Entry<String, JsonNode> opEntry : ops.entrySet()) {
                String method = opEntry.getKey().toLowerCase();
                JsonNode op = opEntry.getValue();
                if (!op.isObject()) continue;

                // 规则 1：跳过无 operationId
                if (!op.has("operationId") || op.get("operationId").asText().isEmpty()) {
                    skipped.add(skip(path, method, "no operationId"));
                    continue;
                }
                String operationId = op.get("operationId").asText();

                // 规则 2：重名加 _method 后缀
                String name = operationId;
                if (usedNames.contains(name)) {
                    name = operationId + "_" + method;
                    int i = 2;
                    while (usedNames.contains(name)) name = operationId + "_" + method + i++;
                }
                usedNames.add(name);

                // 规则 3：description
                String summary = op.path("summary").asText("");
                String desc = op.path("description").asText("");
                String head = !summary.isEmpty() ? summary : (!desc.isEmpty() ? desc : method.toUpperCase() + " " + path);
                String fullDesc = method.toUpperCase() + " " + path + " — " + head;

                // 规则 4：inputSchema
                ObjectNode inputSchema = buildInputSchema(op, commonParams, resolver);

                // 规则 5：annotations
                ObjectNode annotations = buildAnnotations(op, method, operationId);

                // 规则 6：source 记录原始 path/method/operationId/baseUrl，供 tools/call 回放。
                // 契约 §5.2 约定 baseUrl 默认指向 Mock LLM 的 /tools/echo 端点：
                // 当 baseUrl 是 9200 时追加 replayPath，回放打到 /tools/echo 并把原始
                // OpenAPI path/method 作为 query 元信息带过去；显式给其它 baseUrl 时按真实 API 回放。
                ObjectNode source = HttpJson.MAPPER.createObjectNode();
                source.put("type", "openapi");
                source.put("path", path);
                source.put("method", method);
                source.put("operationId", operationId);
                String effectiveBase = baseUrl == null || baseUrl.isEmpty()
                        ? "http://127.0.0.1:9200" : baseUrl;
                source.put("baseUrl", effectiveBase);
                if (effectiveBase.contains(":9200")) {
                    source.put("replayPath", "/tools/echo");
                }

                ObjectNode tool = Tool.openapi(name, fullDesc, inputSchema, annotations, source, specId);
                result.tools.add(tool);
                names.add(name);
            }
        }

        result.response.put("imported", result.tools.size());
        result.response.set("tools", names);
        result.response.set("skipped", skipped);
        return result;
    }

    // ------------------------------------------------------------------ inputSchema

    /**
     * 组装 inputSchema.properties：
     *  - parameters[] 的 in=path|query|header 变成顶层属性（带 in/required/description）
     *  - requestBody 的 application/json schema 经 SchemaResolver 展开后，其 properties 提升为顶层属性
     *    （每个属性带 in:"body"），required 合并进 inputSchema.required。
     */
    private static ObjectNode buildInputSchema(JsonNode op, List<JsonNode> commonParams,
                                               SchemaResolver resolver) {
        ObjectNode input = HttpJson.MAPPER.createObjectNode();
        input.put("type", "object");
        ObjectNode props = HttpJson.MAPPER.createObjectNode();
        ArrayNode required = HttpJson.MAPPER.createArrayNode();
        Set<String> reqSeen = new HashSet<>();

        // ---- parameters（合并 pathItem 级 + operation 级，operation 级同名覆盖）----
        Map<String, JsonNode> paramMap = new LinkedHashMap<>();
        for (JsonNode p : commonParams) {
            if (p.isObject() && p.has("name")) paramMap.put(p.get("name").asText(), p);
        }
        if (op.has("parameters") && op.get("parameters").isArray()) {
            for (JsonNode p : op.get("parameters")) {
                if (p.isObject() && p.has("name")) paramMap.put(p.get("name").asText(), p);
            }
        }
        for (Map.Entry<String, JsonNode> pe : paramMap.entrySet()) {
            String pname = pe.getKey();
            JsonNode p = pe.getValue();
            String in = p.path("in").asText("query");
            if (!(in.equals("path") || in.equals("query") || in.equals("header"))) continue;
            ObjectNode prop = HttpJson.MAPPER.createObjectNode();
            // 参数 schema 可能带 $ref -> 展开
            JsonNode pSchema = p.has("schema") ? p.get("schema") : HttpJson.MAPPER.createObjectNode();
            ObjectNode resolved = resolver.resolve(pSchema);
            resolved.fields().forEachRemaining(e -> prop.set(e.getKey(), e.getValue().deepCopy()));
            if (!prop.has("type")) prop.put("type", "string");
            prop.put("in", in);
            String pDesc = p.path("description").asText("");
            if (!pDesc.isEmpty()) prop.put("description", pDesc + " (" + in + ")");
            boolean req = in.equals("path") || p.path("required").asBoolean(false);  // path 强制 required
            prop.put("required", req);
            props.set(pname, prop);
            if (req && reqSeen.add(pname)) required.add(pname);
        }

        // ---- requestBody.content["application/json"].schema ----
        JsonNode bodySchema = null;
        if (op.has("requestBody") && op.get("requestBody").isObject()) {
            JsonNode content = op.get("requestBody").get("content");
            if (content != null && content.isObject()) {
                JsonNode appJson = content.get("application/json");
                if (appJson == null) {
                    // 取第一个 content 类型
                    java.util.Iterator<JsonNode> it = content.elements();
                    if (it.hasNext()) appJson = it.next();
                }
                if (appJson != null && appJson.isObject() && appJson.has("schema")) {
                    bodySchema = appJson.get("schema");
                }
            }
        }
        if (bodySchema != null) {
            ObjectNode resolved = resolver.resolve(bodySchema);   // ★ $ref / allOf / 嵌套 / 数组 在此展开
            if (resolved.has("properties") && resolved.get("properties").isObject()) {
                resolved.get("properties").fields().forEachRemaining(e -> {
                    ObjectNode prop = (ObjectNode) e.getValue().deepCopy();
                    prop.put("in", "body");
                    props.set(e.getKey(), prop);
                });
            }
            if (resolved.has("required") && resolved.get("required").isArray()) {
                for (JsonNode q : resolved.get("required")) {
                    String s = q.asText();
                    if (reqSeen.add(s)) required.add(s);
                }
            }
            // 若 body 本身是数组 schema（无 properties），放一个 body 属性保留结构
            if (!resolved.has("properties") && resolved.has("type")) {
                ObjectNode prop = resolved.deepCopy();
                prop.put("in", "body");
                props.set("body", prop);
            }
        }

        input.set("properties", props);
        input.set("required", required);
        return input;
    }

    // ------------------------------------------------------------------ annotations

    private static ObjectNode buildAnnotations(JsonNode op, String method, String operationId) {
        ObjectNode a = Tool.defaultAnnotations(
                op.path("summary").asText(operationId));
        if (method.equals("get")) a.put("readOnlyHint", true);
        if (method.equals("delete")) a.put("destructiveHint", true);
        if (method.equals("put") || method.equals("get")) a.put("idempotentHint", true);
        // openWorldHint：有外部 baseUrl 的写操作视为开放世界
        a.put("openWorldHint", method.equals("post") || method.equals("patch"));
        return a;
    }

    private static ObjectNode skip(String path, String method, String reason) {
        ObjectNode s = HttpJson.MAPPER.createObjectNode();
        s.put("path", path);
        s.put("method", method);
        s.put("reason", reason);
        return s;
    }
}
