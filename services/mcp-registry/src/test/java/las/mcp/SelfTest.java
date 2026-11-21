package las.mcp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Comparator;

/**
 * main-based 自测（不引 JUnit，java -cp 直接跑断言）。
 *
 * <p>覆盖：
 *  1. SchemaResolver：$ref "#/components/schemas/X" 递归展开正确（无残留 $ref）。
 *  2. allOf 合并（BaseUser + 扩展 properties / required 合并）。
 *  3. 嵌套对象 / 数组 items 保留。
 *  4. 循环引用防栈溢出（User.manager 自引用）。
 *  5. OpenApiConverter：imported>=2、skipped 记录无 operationId 的、path 参数强制 required、
 *     annotations（get readOnly / delete destructive / put+get idempotent）、source 记录。
 *
 * <pre>
 *   set JAVA_HOME=C:\Java\jdk-11
 *   mvn -q -f services\mcp-registry\pom.xml test-compile
 *   java -cp "target\classes;target\test-classes;<jackson jars>" las.mcp.SelfTest
 * </pre>
 */
public final class SelfTest {

    private static int passed = 0;

    public static void main(String[] args) throws Exception {
        JsonNode spec = loadSample();
        SchemaResolver resolver = new SchemaResolver(spec);

        // ---- 1. $ref 展开：展开前 vs 展开后 ----
        JsonNode before = spec.at("/components/schemas/User");
        assertTrue(before.path("properties").path("address").has("$ref"),
                "展开前 User.address 是 $ref");
        ObjectNode after = resolver.resolve(spec.at("/components/schemas/Address"));
        assertTrue(!after.has("$ref"), "展开后无 $ref");
        assertTrue(after.path("type").asText().equals("object"), "Address.type=object");
        assertTrue(after.path("properties").has("city"), "Address.city 保留");
        assertTrue(after.path("properties").path("geo").path("type").asText().equals("object"),
                "嵌套对象 geo 保留");
        assertTrue(after.path("properties").path("geo").path("properties").has("lat"),
                "嵌套对象 geo.lat 保留");
        assertTrue(!after.toString().contains("$ref"), "展开结果字符串里不含 $ref");
        System.out.println("[OK] $ref 递归展开：Address 展开前后对比通过");
        System.out.println("     展开前: " + spec.at("/components/schemas/Address").toString().substring(0,
                Math.min(120, spec.at("/components/schemas/Address").toString().length())) + " ...");
        System.out.println("     展开后: " + truncate(after.toString(), 160));

        // ---- 2. allOf 合并 ----
        ObjectNode merged = resolver.resolve(spec.at("/components/schemas/CreateUserRequest"));
        assertTrue(!merged.has("$ref") && !merged.has("allOf"), "allOf 合并后无 $ref/allOf");
        assertTrue(merged.path("properties").has("name"), "allOf 合并 BaseUser.name");
        assertTrue(merged.path("properties").has("email"), "allOf 合并 BaseUser.email");
        assertTrue(merged.path("properties").has("address"), "allOf 合并扩展.address");
        assertTrue(merged.path("properties").path("address").path("properties").has("city"),
                "allOf 内部 $ref(Address) 也已展开");
        assertTrue(merged.path("properties").path("address").path("properties").path("geo")
                .path("properties").has("lat"), "allOf 内嵌套对象 geo.lat 保留");
        assertTrue(merged.path("properties").path("roles").path("type").asText().equals("array"),
                "数组 roles 保留");
        assertTrue(merged.path("properties").path("roles").path("items").path("type").asText()
                .equals("string"), "数组 items 保留");
        assertTrue(contains(merged.path("required"), "name")
                && contains(merged.path("required"), "email")
                && contains(merged.path("required"), "address"), "required 合并（BaseUser + 扩展）");
        System.out.println("[OK] allOf 合并：properties/required/数组/嵌套对象全部保留");

        // ---- 3. 循环引用防栈溢出 ----
        // User.manager -> $ref User 自引用：解析到第二层时命中 ref 栈并终止，
        // 整棵树里出现 circular 标记且不会 StackOverflowError。
        ObjectNode user = resolver.resolve(spec.at("/components/schemas/User"));
        assertTrue(user.toString().contains("circular $ref"),
                "自引用 manager 在解析链中被标记为 circular（未栈溢出）");
        assertTrue(user.path("properties").has("id") && user.path("properties").has("name"),
                "循环引用的父层属性仍完整保留");
        System.out.println("[OK] 循环引用检测：User.manager 自引用安全终止并打标记");

        // ---- 4. 转换器整体行为 ----
        OpenApiConverter.Result r = OpenApiConverter.convert(spec,
                "http://127.0.0.1:9200", "selftest");
        assertTrue(r.response.path("imported").asInt() >= 2, "imported>=2 (E18)");
        assertTrue(r.response.path("skipped").size() >= 2, "无 operationId 的进入 skipped[]");
        boolean sawSkip = false;
        for (JsonNode s : r.response.path("skipped")) {
            if (s.path("reason").asText().equals("no operationId")) sawSkip = true;
        }
        assertTrue(sawSkip, "skipped.reason = no operationId");
        System.out.println("[OK] 转换器 imported=" + r.response.path("imported").asInt()
                + ", skipped=" + r.response.path("skipped").size()
                + ", tools=" + r.response.path("tools").toString());

        // 找到 getUserById / createUser / deleteUser / replaceUser
        ObjectNode getUser = find(r.tools, "getUserById");
        ObjectNode createUser = find(r.tools, "createUser");
        ObjectNode deleteUser = find(r.tools, "deleteUser");
        ObjectNode replaceUser = find(r.tools, "replaceUser");
        assertTrue(getUser != null, "getUserById 生成");
        assertTrue(createUser != null, "createUser 生成");
        assertTrue(deleteUser != null, "deleteUser 生成");
        assertTrue(replaceUser != null, "replaceUser 生成");

        // path 参数强制 required + in 标注
        ObjectNode idProp = (ObjectNode) getUser.path("inputSchema").path("properties").path("id");
        assertTrue(idProp.path("in").asText().equals("path"), "path 参数 in=path");
        assertTrue(idProp.path("required").asBoolean(), "path 参数强制 required");
        assertTrue(contains(getUser.path("inputSchema").path("required"), "id"),
                "id 进入 inputSchema.required");
        assertTrue(getUser.path("inputSchema").path("properties").path("verbose")
                .path("in").asText().equals("query"), "query 参数 in=query");
        assertTrue(getUser.path("inputSchema").path("properties").path("X-Trace-Id")
                .path("in").asText().equals("header"), "header 参数 in=header");

        // annotations
        assertTrue(getUser.path("annotations").path("readOnlyHint").asBoolean(), "get readOnlyHint");
        assertTrue(getUser.path("annotations").path("idempotentHint").asBoolean(), "get idempotentHint");
        assertTrue(deleteUser.path("annotations").path("destructiveHint").asBoolean(),
                "delete destructiveHint");
        assertTrue(replaceUser.path("annotations").path("idempotentHint").asBoolean(),
                "put idempotentHint");

        // source
        assertTrue(getUser.path("source").path("path").asText().equals("/users/{id}"), "source.path");
        assertTrue(getUser.path("source").path("method").asText().equals("get"), "source.method");
        assertTrue(getUser.path("source").path("operationId").asText().equals("getUserById"),
                "source.operationId");
        assertTrue(getUser.path("source").path("baseUrl").asText().equals("http://127.0.0.1:9200"),
                "source.baseUrl");

        // requestBody 的 allOf+$ref 已展开进 createUser.inputSchema
        ObjectNode cp = (ObjectNode) createUser.path("inputSchema").path("properties");
        assertTrue(cp.has("name") && cp.has("email") && cp.has("address"),
                "createUser: requestBody allOf 展开为顶层 properties");
        assertTrue(cp.path("address").path("properties").has("city"),
                "createUser: 嵌套 Address 已展开");
        assertTrue(cp.path("address").path("in").asText().equals("body"), "body 参数 in=body");
        assertTrue(contains(createUser.path("inputSchema").path("required"), "address"),
                "createUser: required 合并自 allOf");
        assertTrue(cp.path("roles").path("items").path("type").asText().equals("string"),
                "createUser: 数组 items 保留");
        System.out.println("[OK] inputSchema/annotations/source 全部符合 §5.2");

        // ---- 5. 持久化 + 回放结果结构 ----
        Path tmp = Files.createTempDirectory("mcp-selftest-");
        try {
            Registry reg = new Registry(tmp.resolve("mcp-registry.json").toString());
            reg.putAll(r.tools, false);
            assertTrue(reg.size() >= 2, "Registry 落盘工具数");
            Registry reg2 = new Registry(tmp.resolve("mcp-registry.json").toString());
            assertTrue(reg2.size() == reg.size(), "重启后工具恢复");
            ObjectNode replayResult = Replay.result("hello", false);
            assertTrue(replayResult.path("isError").asBoolean() == false, "回放 isError=false");
            assertTrue(replayResult.path("content").get(0).path("text").asText().equals("hello"),
                    "回放 content[0].text");
            System.out.println("[OK] 工具持久化 + 回放结果结构");
        } finally {
            try {
                Files.walk(tmp).sorted(Comparator.reverseOrder()).forEach(p -> {
                    try { Files.deleteIfExists(p); } catch (Exception ignored) { }
                });
            } catch (Exception ignored) { }
        }

        System.out.println("=== mcp-registry SelfTest PASSED (" + passed + " assertions) ===");
    }

    private static JsonNode loadSample() throws Exception {
        // 优先从 classpath 资源读，其次从源码目录读
        try (InputStream is = SelfTest.class.getResourceAsStream("/sample-openapi.json")) {
            if (is != null) {
                return HttpJson.MAPPER.readTree(new String(is.readAllBytes(), StandardCharsets.UTF_8));
            }
        }
        Path p = Path.of("src/main/java/las/mcp/sample-openapi.json");
        if (!Files.exists(p)) p = Path.of("services/mcp-registry/src/main/java/las/mcp/sample-openapi.json");
        return HttpJson.MAPPER.readTree(new String(Files.readAllBytes(p), StandardCharsets.UTF_8));
    }

    private static ObjectNode find(java.util.List<ObjectNode> tools, String name) {
        for (ObjectNode t : tools) if (name.equals(t.path("name").asText())) return t;
        return null;
    }

    private static boolean contains(JsonNode arr, String v) {
        if (arr == null || !arr.isArray()) return false;
        for (JsonNode n : arr) if (v.equals(n.asText())) return true;
        return false;
    }

    private static String truncate(String s, int n) {
        return s.length() <= n ? s : s.substring(0, n) + " ...";
    }

    private static void assertTrue(boolean cond, String msg) {
        if (!cond) throw new AssertionError("FAIL: " + msg);
        passed++;
    }
}
