package las.mcp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.util.HashSet;
import java.util.Set;

/**
 * ★ OpenAPI $ref 递归展开引擎（契约 §5.2 规则 4，简历核心能力 C2/C5）。
 *
 * <p>职责：
 * 1. 解析 "#/components/schemas/X" 形式的本地 $ref，从整个 OpenAPI 文档根按 JSON Pointer 取目标。
 * 2. 递归展开：目标里还有 $ref / 嵌套 object / array.items 时继续展开，直到没有 $ref。
 * 3. 防循环引用：用 ref 栈（Set&lt;String&gt;）跟踪当前展开链；检测到环则保留一个
 *    {"type":"object","description":"circular ref: X"} 占位，不无限递归、不栈溢出。
 * 4. allOf 合并：把每个子 schema（先各自展开）的 properties / required / type / description
 *    合并成一个扁平 schema；数组 items、嵌套对象 properties 都保留。
 * 5. 保留原始 JSON Schema 关键字：type / format / enum / default / description /
 *    minimum / maximum / items / properties / required / additionalProperties 等原样拷贝。
 *
 * <p>展开前后对比（见 sample-openapi.json 的 User / CreateUserRequest）：
 * <pre>
 *   展开前： {"$ref":"#/components/schemas/User"}
 *   展开后： {"type":"object","properties":{"id":{"type":"string",...},
 *            "name":{...},"address":{"type":"object","properties":{...}},
 *            "tags":{"type":"array","items":{"type":"string"}}},
 *            "required":["id","name"]}
 * </pre>
 * 实现位置见 resolve()/resolveRef()/resolveAllOf() 方法，行号在 README 与最终答复里标注。
 */
public final class SchemaResolver {

    private final JsonNode root;   // 完整 OpenAPI 文档，用于解析 #/... 指针

    public SchemaResolver(JsonNode root) {
        this.root = root;
    }

    /**
     * 入口：对一个 schema 节点做完整展开（$ref / allOf / 嵌套 / 数组 items）。
     * 返回的节点与输入完全解耦（深拷贝），不含任何 $ref。
     */
    public ObjectNode resolve(JsonNode schema) {
        return resolve(schema, new HashSet<>());
    }

    /** 带 ref 栈的内部递归。 */
    private ObjectNode resolve(JsonNode schema, Set<String> stack) {
        if (schema == null || schema.isNull()) {
            return emptySchema();
        }
        if (schema.isArray()) {
            // schema 本身是数组不合法，包成 {} 防御
            return emptySchema();
        }
        if (!schema.isObject()) {
            ObjectNode o = emptySchema();
            o.put("description", "scalar schema: " + schema.toString());
            return o;
        }
        ObjectNode src = (ObjectNode) schema;

        // ---- 规则：allOf 合并优先 ----
        if (src.has("allOf") && src.get("allOf").isArray()) {
            return resolveAllOf(src, stack);
        }

        // ---- 规则：$ref 展开 ----
        if (src.has("$ref") && src.get("$ref").isTextual()) {
            String ref = src.get("$ref").asText();
            return resolveRef(ref, src, stack);
        }

        // ---- 普通 schema：递归展开 properties / items ----
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        for (java.util.Iterator<java.util.Map.Entry<String, JsonNode>> it = src.fields(); it.hasNext(); ) {
            java.util.Map.Entry<String, JsonNode> e = it.next();
            String k = e.getKey();
            JsonNode v = e.getValue();
            switch (k) {
                case "properties":
                    if (v.isObject()) {
                        ObjectNode props = HttpJson.MAPPER.createObjectNode();
                        v.fields().forEachRemaining(pe ->
                                props.set(pe.getKey(), resolve(pe.getValue(), stack)));
                        out.set("properties", props);
                    } else {
                        out.set(k, v.deepCopy());
                    }
                    break;
                case "items":
                    if (v.isObject() || (v.isArray() && v.size() > 0)) {
                        out.set("items", resolve(v, stack));
                    } else {
                        out.set(k, v.deepCopy());
                    }
                    break;
                case "additionalProperties":
                    if (v.isObject()) out.set(k, resolve(v, stack));
                    else out.set(k, v.deepCopy());
                    break;
                case "allOf":
                case "oneOf":
                case "anyOf":
                    // 这些组合关键字：逐个展开子元素，保留结构
                    if (v.isArray()) {
                        ArrayNode a = HttpJson.MAPPER.createArrayNode();
                        for (JsonNode sub : v) a.add(resolve(sub, stack));
                        out.set(k, a);
                    } else out.set(k, v.deepCopy());
                    break;
                default:
                    out.set(k, v.deepCopy());   // type/format/enum/required/description 等原样保留
            }
        }
        if (!out.has("type") && out.has("properties")) out.put("type", "object");
        return out;
    }

    /** 解析 "#/components/schemas/User" -> 取 root 里的 User 节点并递归展开。 */
    private ObjectNode resolveRef(String ref, ObjectNode original, Set<String> stack) {
        if (!ref.startsWith("#/")) {
            // 外部 $ref 不支持，保留描述说明
            ObjectNode out = emptySchema();
            out.put("description", "external $ref not resolved: " + ref);
            return out;
        }
        if (stack.contains(ref)) {
            // 防循环引用：不再递归，返回占位（用 x-circular-ref 标记，兄弟字段不覆盖）
            ObjectNode out = emptySchema();
            out.put("description", "circular $ref (stopped): " + ref);
            out.put("x-circular-ref", ref);
            return out;
        }
        JsonNode target = pointer(ref.substring(2));   // 去掉 "#/"
        if (target == null) {
            ObjectNode out = emptySchema();
            out.put("description", "unresolved $ref: " + ref);
            return out;
        }
        Set<String> next = new HashSet<>(stack);
        next.add(ref);
        ObjectNode resolved = resolve(target, next);

        // $ref 旁边若有兄弟字段（如 description），用兄弟字段覆盖/补充；
        // 但若命中的是循环引用占位，保留 circular 标记优先。
        boolean circular = resolved.has("x-circular-ref");
        for (java.util.Iterator<java.util.Map.Entry<String, JsonNode>> it = original.fields(); it.hasNext(); ) {
            java.util.Map.Entry<String, JsonNode> e = it.next();
            if (e.getKey().equals("$ref")) continue;
            if (e.getKey().equals("description") || e.getKey().equals("title")) {
                if (circular && e.getKey().equals("description")) {
                    resolved.put("description",
                            "circular $ref (stopped): " + ref + " | " + e.getValue().asText());
                } else {
                    resolved.set(e.getKey(), e.getValue().deepCopy());
                }
            }
        }
        return resolved;
    }

    /** allOf 合并：展开每个子 schema，再合并 properties / required / 其它关键字。 */
    private ObjectNode resolveAllOf(ObjectNode src, Set<String> stack) {
        ObjectNode merged = HttpJson.MAPPER.createObjectNode();
        ObjectNode props = HttpJson.MAPPER.createObjectNode();
        ArrayNode required = HttpJson.MAPPER.createArrayNode();
        Set<String> reqSeen = new HashSet<>();

        for (JsonNode sub : src.get("allOf")) {
            ObjectNode r = resolve(sub, stack);
            // 合并 properties
            if (r.has("properties") && r.get("properties").isObject()) {
                r.get("properties").fields().forEachRemaining(pe ->
                        props.set(pe.getKey(), pe.getValue().deepCopy()));
            }
            // 合并 required（去重）
            if (r.has("required") && r.get("required").isArray()) {
                for (JsonNode q : r.get("required")) {
                    String s = q.asText();
                    if (reqSeen.add(s)) required.add(s);
                }
            }
            // 合并其它关键字（type/description/... 后者不覆盖已有 type）
            r.fields().forEachRemaining(e -> {
                String k = e.getKey();
                if (k.equals("properties") || k.equals("required")) return;
                if (k.equals("type") && merged.has("type")) return;
                merged.set(k, e.getValue().deepCopy());
            });
        }

        // 顶层 allOf 同级字段（description / type 等）
        src.fields().forEachRemaining(e -> {
            String k = e.getKey();
            if (k.equals("allOf")) return;
            if (k.equals("type") && merged.has("type")) return;
            merged.set(k, e.getValue().deepCopy());
        });

        if (!props.isEmpty()) merged.set("properties", props);
        if (!required.isEmpty()) merged.set("required", required);
        if (!merged.has("type")) merged.put("type", "object");
        return merged;
    }

    /** JSON Pointer（只支持 / 分隔的 object 路径，OpenAPI 组件名不含 ~ 转义时足够）。 */
    private JsonNode pointer(String path) {
        JsonNode cur = root;
        for (String seg : path.split("/")) {
            if (seg.isEmpty()) continue;
            String key = seg.replace("~1", "/").replace("~0", "~");
            if (cur == null || !cur.isObject()) return null;
            cur = cur.get(key);
        }
        return cur;
    }

    private static ObjectNode emptySchema() {
        ObjectNode o = HttpJson.MAPPER.createObjectNode();
        o.put("type", "object");
        return o;
    }
}
