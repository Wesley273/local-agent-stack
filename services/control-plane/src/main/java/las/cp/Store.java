package las.cp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.util.ArrayList;
import java.util.List;

/**
 * 单文件 JSON 持久化 + 内存索引。
 *
 * <p>写入用「临时文件 + 原子 move」：先写 data/control-plane.json.tmp，
 * 再 ATOMIC_MOVE 到目标（不支持 ATOMIC_MOVE 时降级为 REPLACE_EXISTING），
 * 保证数据面/其它进程读到的永远是完整文件，不会读到写一半的 JSON。
 *
 * <p>磁盘格式就是 §4 快照本身（revision/generatedAt/agents/...），
 * 启动时直接 load 成 Snapshot，重启可恢复所有 CRUD 的结果与 revision。
 */
public final class Store {

    private final Path file;
    private final Path dir;
    private final Path tmp;
    private final ObjectMapper mapper = HttpJson.MAPPER;

    /** 最近变更事件（内存 + 落盘，最多 100 条），用于 /v1/events 与 E21 验证。 */
    private final List<ObjectNode> events = new ArrayList<>();

    public Store(String path) {
        this.file = Paths.get(path);
        Path parent = file.toAbsolutePath().getParent();
        this.dir = parent == null ? Paths.get(".") : parent;
        this.tmp = dir.resolve(file.getFileName().toString() + ".tmp");
    }

    /** 磁盘文件绝对路径，便于日志。 */
    public String path() {
        return file.toAbsolutePath().toString();
    }

    /** 原子写入 UTF-8 字节。 */
    public synchronized void save(ObjectNode root) {
        try {
            Files.createDirectories(dir);
            byte[] bytes = mapper.writerWithDefaultPrettyPrinter().writeValueAsBytes(root);
            Files.write(tmp, bytes);                     // 显式 UTF-8：Jackson 输出即 UTF-8 字节
            try {
                Files.move(tmp, file, StandardCopyOption.REPLACE_EXISTING,
                        StandardCopyOption.ATOMIC_MOVE);
            } catch (AtomicMoveNotSupportedException e) {
                Files.move(tmp, file, StandardCopyOption.REPLACE_EXISTING);
            }
        } catch (IOException e) {
            throw new RuntimeException("persist failed: " + file + " : " + e, e);
        }
    }

    /** 读取磁盘快照；文件不存在或损坏时返回 null（由调用方回退到默认快照）。 */
    public synchronized ObjectNode load() {
        if (!Files.exists(file)) {
            return null;
        }
        try {
            byte[] bytes = Files.readAllBytes(file);
            JsonNode n = mapper.readTree(new String(bytes, StandardCharsets.UTF_8));
            if (n != null && n.isObject()) {
                // 恢复事件列表
                JsonNode ev = n.get("_events");
                events.clear();
                if (ev != null && ev.isArray()) {
                    for (JsonNode e : ev) {
                        if (e.isObject()) events.add(((ObjectNode) e).deepCopy());
                    }
                }
                ObjectNode root = (ObjectNode) n;
                root.remove("_events");   // 事件不暴露在 /v1/config 快照里
                return root;
            }
        } catch (Exception e) {
            System.err.println("[store] load failed, fallback to defaults: " + e);
        }
        return null;
    }

    /** 保存时把事件列表一并落盘（放在 _events 字段，读取时剥离）。 */
    public synchronized void saveWithEvents(ObjectNode snapshot) {
        ObjectNode root = snapshot.deepCopy();
        com.fasterxml.jackson.databind.node.ArrayNode arr = mapper.createArrayNode();
        for (ObjectNode e : events) arr.add(e.deepCopy());
        root.set("_events", arr);
        save(root);
    }

    /** 追加变更事件并落盘。在 Snapshot 锁内调用。 */
    public synchronized void addEvent(ObjectNode snapshot, String op, String resource,
                                      String key, long revision) {
        ObjectNode e = mapper.createObjectNode();
        e.put("op", op);
        e.put("resource", resource);
        e.put("key", key == null ? "" : key);
        e.put("revision", revision);
        e.put("at", java.time.OffsetDateTime.now().toString());
        events.add(e);
        if (events.size() > 100) events.remove(0);
        saveWithEvents(snapshot);
    }

    /** 事件列表副本。 */
    public synchronized List<ObjectNode> events() {
        List<ObjectNode> out = new ArrayList<>();
        for (ObjectNode e : events) out.add(e.deepCopy());
        return out;
    }
}
