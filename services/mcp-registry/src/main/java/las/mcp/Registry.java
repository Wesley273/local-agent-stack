package las.mcp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * 统一工具注册表（高码 / 低码 / 三方，契约 §5.1 / 能力矩阵 C1/C3/C4）。
 *
 * <p>内存索引：name -> Tool（LinkedHashMap 保序）。落盘：data/mcp-registry.json
 * （原子 tmp + move）。同时维护已导入 spec 列表（GET /registry/specs）。
 * tool 带 version / origin / specId，支持按名称检索、版本管理、删除、持久化恢复。
 */
public final class Registry {

    private final Map<String, ObjectNode> tools = new LinkedHashMap<>();
    private final ArrayNode specs = HttpJson.MAPPER.createArrayNode();
    private final Path file;
    private final Path dir;
    private final Path tmp;

    public Registry(String path) {
        this.file = Paths.get(path);
        Path parent = file.toAbsolutePath().getParent();
        this.dir = parent == null ? Paths.get(".") : parent;
        this.tmp = dir.resolve(file.getFileName().toString() + ".tmp");
        load();
    }

    public String path() {
        return file.toAbsolutePath().toString();
    }

    // ------------------------------------------------------------------ 读

    public synchronized ArrayNode list() {
        ArrayNode out = HttpJson.MAPPER.createArrayNode();
        for (ObjectNode t : tools.values()) out.add(t.deepCopy());
        return out;
    }

    /** 名称/描述模糊检索（q 参数）。 */
    public synchronized ArrayNode search(String q) {
        ArrayNode out = HttpJson.MAPPER.createArrayNode();
        String needle = q == null ? "" : q.toLowerCase();
        for (ObjectNode t : tools.values()) {
            if (needle.isEmpty()
                    || t.path("name").asText().toLowerCase().contains(needle)
                    || t.path("description").asText().toLowerCase().contains(needle)) {
                out.add(t.deepCopy());
            }
        }
        return out;
    }

    public synchronized ObjectNode get(String name) {
        ObjectNode t = tools.get(name);
        if (t == null) throw HttpJson.ApiException.toolNotFound("tool not found: " + name);
        return t.deepCopy();
    }

    public synchronized boolean has(String name) {
        return tools.containsKey(name);
    }

    public synchronized ArrayNode specs() {
        return specs.deepCopy();
    }

    public synchronized int size() {
        return tools.size();
    }

    // ------------------------------------------------------------------ 写

    /** 注册/替换一批工具；返回实际写入的名字列表。 */
    public synchronized List<String> putAll(List<ObjectNode> batch, boolean replace) {
        if (replace) tools.clear();
        List<String> names = new java.util.ArrayList<>();
        for (ObjectNode t : batch) {
            ObjectNode norm = Tool.from(t);
            String name = norm.path("name").asText();
            tools.put(name, norm);   // 同名覆盖（版本更新）
            names.add(name);
        }
        save();
        return names;
    }

    /** 手动注册单个工具（高码/低码/三方统一入口）。 */
    public synchronized ObjectNode register(ObjectNode body) {
        ObjectNode norm = Tool.from(body);
        tools.put(norm.path("name").asText(), norm);
        save();
        return norm.deepCopy();
    }

    public synchronized ObjectNode delete(String name) {
        ObjectNode removed = tools.remove(name);
        if (removed == null) throw HttpJson.ApiException.toolNotFound("tool not found: " + name);
        save();
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.put("removed", true);
        out.put("name", name);
        return out;
    }

    /** 记录一次导入的 spec 摘要（source 名、工具数、时间）。 */
    public synchronized void addSpec(String source, int count, String baseUrl) {
        ObjectNode s = HttpJson.MAPPER.createObjectNode();
        s.put("source", source == null ? "" : source);
        s.put("baseUrl", baseUrl == null ? "" : baseUrl);
        s.put("toolCount", count);
        s.put("importedAt", java.time.OffsetDateTime.now().toString());
        specs.add(s);
        save();
    }

    // ------------------------------------------------------------------ 持久化（原子写）

    private void save() {
        try {
            Files.createDirectories(dir);
            ObjectNode root = HttpJson.MAPPER.createObjectNode();
            root.put("version", 1);
            ArrayNode arr = HttpJson.MAPPER.createArrayNode();
            for (ObjectNode t : tools.values()) arr.add(t.deepCopy());
            root.set("tools", arr);
            root.set("specs", specs.deepCopy());
            byte[] bytes = HttpJson.MAPPER.writerWithDefaultPrettyPrinter().writeValueAsBytes(root);
            Files.write(tmp, bytes);
            try {
                Files.move(tmp, file, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
            } catch (AtomicMoveNotSupportedException e) {
                Files.move(tmp, file, StandardCopyOption.REPLACE_EXISTING);
            }
        } catch (IOException e) {
            throw new RuntimeException("persist registry failed: " + file + " : " + e, e);
        }
    }

    private void load() {
        if (!Files.exists(file)) return;
        try {
            JsonNode root = HttpJson.MAPPER.readTree(
                    new String(Files.readAllBytes(file), StandardCharsets.UTF_8));
            if (root == null || !root.isObject()) return;
            if (root.has("tools") && root.get("tools").isArray()) {
                for (JsonNode t : root.get("tools")) {
                    if (t.isObject()) {
                        ObjectNode norm = Tool.from(t);
                        tools.put(norm.path("name").asText(), norm);
                    }
                }
            }
            if (root.has("specs") && root.get("specs").isArray()) {
                for (JsonNode s : root.get("specs")) specs.add(s.deepCopy());
            }
        } catch (Exception e) {
            System.err.println("[registry] load failed, start empty: " + e);
        }
    }
}
