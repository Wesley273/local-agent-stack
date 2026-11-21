package las.cp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.time.OffsetDateTime;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;

/**
 * §4 全量配置快照模型（唯一真相源：docs/01-协议契约.md §4）。
 *
 * <p>字段名、层级、类型逐字段对齐契约，数据面直接按此解析：
 * revision(int) / generatedAt(RFC3339) / agents(map) / policies(map) /
 * grants(array) / models(array) / rateLimits(map) / guardrails(object)。
 *
 * <p>任何写操作：revision++、generatedAt 刷新、整体原子落盘、并追加一条变更事件。
 * /v1/config 直接返回 snapshot() 的深拷贝，保证 ETag(=revision) 与内容一致。
 */
public final class Snapshot {

    private final Store store;
    private ObjectNode root;      // 完整快照（不含 _events）
    private long revision;

    public Snapshot(Store store) {
        this.store = store;
        ObjectNode loaded = store.load();
        if (loaded == null) {
            this.root = defaults();
            this.revision = root.path("revision").asLong(1);
            persist(null, null, null);   // 首次落盘（不算一次「变更事件」）
        } else {
            this.root = loaded;
            this.revision = root.path("revision").asLong(1);
            root.put("revision", revision);
        }
    }

    public long revision() {
        return revision;
    }

    // ------------------------------------------------------------------ 读

    /** 全量快照深拷贝。 */
    public synchronized ObjectNode snapshot() {
        return root.deepCopy();
    }

    /** 只读访问器（返回深拷贝，防止外部改内部状态）。 */
    public synchronized ObjectNode agents() {
        return (ObjectNode) root.get("agents").deepCopy();
    }
    public synchronized ObjectNode policies() {
        return (ObjectNode) root.get("policies").deepCopy();
    }
    public synchronized ArrayNode grants() {
        return (ArrayNode) root.get("grants").deepCopy();
    }
    public synchronized ArrayNode models() {
        return (ArrayNode) root.get("models").deepCopy();
    }
    public synchronized ObjectNode rateLimits() {
        return (ObjectNode) root.get("rateLimits").deepCopy();
    }
    public synchronized ObjectNode guardrails() {
        return (ObjectNode) root.get("guardrails").deepCopy();
    }

    // ------------------------------------------------------------------ 写（统一 bump）

    /**
     * 写操作统一入口：在 root 上执行 mutation，然后 revision++、generatedAt 刷新、
     * 落盘、记录事件。返回新的 revision。
     */
    public synchronized long write(String op, String resource, String key, Mutation m) {
        m.apply(root);
        revision++;
        root.put("revision", revision);
        root.put("generatedAt", now());
        persist(op, resource, key);
        return revision;
    }

    @FunctionalInterface
    public interface Mutation {
        void apply(ObjectNode root);
    }

    /** 追加事件 + 原子落盘。 */
    private void persist(String op, String resource, String key) {
        if (op == null) {
            store.saveWithEvents(root);
        } else {
            store.addEvent(root, op, resource, key, revision);
        }
    }

    // ------------------------------------------------------------------ helpers

    public static String now() {
        // RFC3339，带 +08:00 偏移（契约示例），用系统默认偏移
        return OffsetDateTime.now().format(DateTimeFormatter.ISO_OFFSET_DATE_TIME);
    }

    /** 取 map 字段，若缺失则建一个空 object。 */
    public static ObjectNode obj(ObjectNode parent, String field) {
        JsonNode n = parent.get(field);
        if (n == null || !n.isObject()) {
            ObjectNode o = HttpJson.MAPPER.createObjectNode();
            parent.set(field, o);
            return o;
        }
        return (ObjectNode) n;
    }

    /** 取 array 字段，若缺失则建一个空 array。 */
    public static ArrayNode arr(ObjectNode parent, String field) {
        JsonNode n = parent.get(field);
        if (n == null || !n.isArray()) {
            ArrayNode a = HttpJson.MAPPER.createArrayNode();
            parent.set(field, a);
            return a;
        }
        return (ArrayNode) n;
    }

    // ------------------------------------------------------------------ 默认快照（逐字段对齐 §4）

    public static ObjectNode defaults() {
        ObjectMapperHolder om = new ObjectMapperHolder();
        ObjectNode r = om.m.createObjectNode();
        r.put("revision", 1);
        r.put("generatedAt", now());

        // ---- agents --------------------------------------------------------
        ObjectNode agents = om.m.createObjectNode();
        agents.set("echo-agent", agent(om, "echo-agent",
                "http://127.0.0.1:9100/a2a/echo-agent",
                new String[]{"echo"},
                new String[]{"http://127.0.0.1:9101/hooks/a2a-notifications"},
                "http://127.0.0.1:9101"));
        agents.set("java-agent", agent(om, "java-agent",
                "http://127.0.0.1:9100/a2a/java-agent",
                new String[]{"echo"}, new String[]{}, "http://127.0.0.1:9102"));
        agents.set("python-agent", agent(om, "python-agent",
                "http://127.0.0.1:9100/a2a/python-agent",
                new String[]{"echo"}, new String[]{}, "http://127.0.0.1:9103"));
        r.set("agents", agents);

        // ---- policies ------------------------------------------------------
        ObjectNode policies = om.m.createObjectNode();
        ObjectNode def = om.m.createObjectNode();
        def.put("type", "apiKey");
        ObjectNode keys = om.m.createObjectNode();
        keys.put("demo-key", "app-demo");
        keys.put("admin-key", "app-admin");
        def.set("keys", keys);
        def.put("allowAnonymous", false);
        policies.set("default", def);
        r.set("policies", policies);

        // ---- grants --------------------------------------------------------
        ArrayNode grants = om.m.createArrayNode();
        grants.add(grant(om, "app-demo", "*", "*"));
        grants.add(grant(om, "app-admin", "*", "*"));
        r.set("grants", grants);

        // ---- models --------------------------------------------------------
        ArrayNode models = om.m.createArrayNode();
        models.add(model(om, "mock-1", "http://127.0.0.1:9200", 10, 100));
        models.add(model(om, "bge-m3", "http://127.0.0.1:9200", 5, 100));
        r.set("models", models);

        // ---- rateLimits ----------------------------------------------------
        ObjectNode rateLimits = om.m.createObjectNode();
        rateLimits.set("default", rateLimit(om, 120, 60000, 20));
        rateLimits.set("app-demo", rateLimit(om, 30, 10000, 5));
        r.set("rateLimits", rateLimits);

        // ---- guardrails ----------------------------------------------------
        ObjectNode guardrails = om.m.createObjectNode();
        ArrayNode blockedWords = om.m.createArrayNode();
        blockedWords.add("BLOCK_ME");
        blockedWords.add("secret-leak");
        guardrails.set("blockedWords", blockedWords);
        ArrayNode blockedTools = om.m.createArrayNode();
        blockedTools.add("dangerous_delete");
        blockedTools.add("drop_database");
        guardrails.set("blockedTools", blockedTools);
        guardrails.put("maxPromptChars", 8000);
        guardrails.put("enableInput", true);
        guardrails.put("enableOutput", true);
        r.set("guardrails", guardrails);

        return r;
    }

    private static ObjectNode agent(ObjectMapperHolder om, String id, String url,
                                    String[] skills, String[] webhooks, String userServer) {
        ObjectNode a = om.m.createObjectNode();
        a.put("agentId", id);
        a.put("url", url);
        a.put("enabled", true);
        a.put("version", "1.0.0");
        ArrayNode s = om.m.createArrayNode();
        for (String sk : skills) s.add(sk);
        a.set("skills", s);
        a.put("authPolicy", "default");
        ArrayNode w = om.m.createArrayNode();
        for (String h : webhooks) w.add(h);
        a.set("webhooks", w);
        ObjectNode meta = om.m.createObjectNode();
        meta.put("userServer", userServer);
        a.set("metadata", meta);
        return a;
    }

    private static ObjectNode grant(ObjectMapperHolder om, String appId, String agentId, String scope) {
        ObjectNode g = om.m.createObjectNode();
        g.put("appId", appId);
        g.put("agentId", agentId);
        ArrayNode scopes = om.m.createArrayNode();
        scopes.add(scope);
        g.set("scopes", scopes);
        return g;
    }

    private static ObjectNode model(ObjectMapperHolder om, String name, String backend,
                                    int priority, int weight) {
        ObjectNode m = om.m.createObjectNode();
        m.put("name", name);
        m.put("backend", backend);
        m.put("priority", priority);
        m.put("weight", weight);
        m.put("enabled", true);
        return m;
    }

    private static ObjectNode rateLimit(ObjectMapperHolder om, int rpm, int tpm, int burst) {
        ObjectNode rl = om.m.createObjectNode();
        rl.put("requestsPerMinute", rpm);
        rl.put("tokensPerMinute", tpm);
        rl.put("burst", burst);
        return rl;
    }

    /** 仅用于持有 ObjectMapper 引用，避免到处 static import。 */
    private static final class ObjectMapperHolder {
        final com.fasterxml.jackson.databind.ObjectMapper m = HttpJson.MAPPER;
    }
}
