package las.cp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.util.ArrayList;
import java.util.List;

/**
 * Agent 注册中心：agents CRUD / 版本 / skills / 元数据 / enabled / webhooks / authPolicy。
 *
 * <p>所有写操作走 Snapshot.write()，统一 revision++ + generatedAt 刷新 + 原子落盘 + 事件。
 * 读操作返回深拷贝，不暴露内部可变状态。
 */
public final class AgentRegistry {

    private final Snapshot snapshot;

    public AgentRegistry(Snapshot snapshot) {
        this.snapshot = snapshot;
    }

    /** GET /v1/agents -> {"agents":{...},"revision":N}（map 形态，对齐 §4）。 */
    public ObjectNode list() {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("agents", snapshot.agents());
        out.put("revision", snapshot.revision());
        return out;
    }

    /** GET /v1/agents/{id} -> 单个 agent（不存在 404 AGENT_NOT_FOUND）。 */
    public ObjectNode get(String id) {
        ObjectNode agents = snapshot.agents();
        JsonNode a = agents.get(id);
        if (a == null) {
            throw HttpJson.ApiException.notFound("AGENT_NOT_FOUND",
                    "agent not registered: " + id);
        }
        return (ObjectNode) a.deepCopy();
    }

    /** POST /v1/agents：创建（id 已存在 -> 400）。 */
    public ObjectNode create(ObjectNode body) {
        String id = text(body, "agentId");
        if (id.isEmpty()) id = text(body, "id");
        if (id.isEmpty()) throw HttpJson.ApiException.invalid("agentId is required");
        final String fid = id;
        ensureAbsent(fid);
        ObjectNode rec = normalizeAgent(body, fid, null);
        long rev = snapshot.write("POST", "agents", fid, root -> {
            ObjectNode agents = Snapshot.obj(root, "agents");
            agents.set(fid, rec.deepCopy());
        });
        return HttpJson.ok(rev, "agent", rec);
    }

    /** PUT /v1/agents/{id}：全量替换（可与 POST 同 body；保留未提供的默认字段）。 */
    public ObjectNode put(String id, ObjectNode body) {
        ObjectNode existing = snapshot.agents();
        ObjectNode old = existing.has(id) && existing.get(id).isObject()
                ? (ObjectNode) existing.get(id) : null;
        ObjectNode rec = normalizeAgent(body, id, old);
        long rev = snapshot.write("PUT", "agents", id, root -> {
            ObjectNode agents = Snapshot.obj(root, "agents");
            agents.set(id, rec.deepCopy());
        });
        return HttpJson.ok(rev, "agent", rec);
    }

    /** PATCH /v1/agents/{id}：局部更新（enabled / version / skills / metadata / ...）。 */
    public ObjectNode patch(String id, ObjectNode body) {
        ObjectNode existing = snapshot.agents();
        if (!existing.has(id)) {
            throw HttpJson.ApiException.notFound("AGENT_NOT_FOUND", "agent not registered: " + id);
        }
        ObjectNode[] holder = new ObjectNode[1];
        long rev = snapshot.write("PATCH", "agents", id, root -> {
            ObjectNode agents = Snapshot.obj(root, "agents");
            ObjectNode cur = (ObjectNode) agents.get(id);
            ObjectNode merged = cur.deepCopy();
            body.fields().forEachRemaining(e -> merged.set(e.getKey(), e.getValue().deepCopy()));
            if (!merged.has("agentId")) merged.put("agentId", id);
            agents.set(id, merged);
            holder[0] = merged.deepCopy();
        });
        return HttpJson.ok(rev, "agent", holder[0]);
    }

    /** DELETE /v1/agents/{id}。 */
    public ObjectNode delete(String id) {
        if (!snapshot.agents().has(id)) {
            throw HttpJson.ApiException.notFound("AGENT_NOT_FOUND", "agent not registered: " + id);
        }
        long rev = snapshot.write("DELETE", "agents", id, root -> {
            ObjectNode agents = Snapshot.obj(root, "agents");
            agents.remove(id);
        });
        return HttpJson.ok(rev, "removed", HttpJson.MAPPER.getNodeFactory().booleanNode(true));
    }

    /** 切换 enabled 开关（便于 E21 热更新验证）。 */
    public ObjectNode setEnabled(String id, boolean enabled) {
        ObjectNode patch = HttpJson.MAPPER.createObjectNode();
        patch.put("enabled", enabled);
        return patch(id, patch);
    }

    /** skills 子资源：GET /v1/agents/{id}/skills。 */
    public ObjectNode skills(String id) {
        ObjectNode a = get(id);
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("skills", a.has("skills") ? a.get("skills").deepCopy()
                : HttpJson.MAPPER.createArrayNode());
        out.put("revision", snapshot.revision());
        return out;
    }

    /** 追加 / 覆盖 skills：PUT /v1/skills 形如 {"agentId":"...","skills":[...]}。 */
    public ObjectNode putSkills(ObjectNode body) {
        String id = text(body, "agentId");
        if (id.isEmpty()) throw HttpJson.ApiException.invalid("agentId is required");
        if (!snapshot.agents().has(id)) {
            throw HttpJson.ApiException.notFound("AGENT_NOT_FOUND", "agent not registered: " + id);
        }
        ArrayNode skills = body.has("skills") && body.get("skills").isArray()
                ? (ArrayNode) body.get("skills").deepCopy() : HttpJson.MAPPER.createArrayNode();
        long rev = snapshot.write("PUT", "skills", id, root -> {
            ObjectNode agents = Snapshot.obj(root, "agents");
            ((ObjectNode) agents.get(id)).set("skills", skills.deepCopy());
        });
        return HttpJson.ok(rev, "skills", skills);
    }

    // ------------------------------------------------------------------ helpers

    /** 规范化 agent 记录，补齐 §4 所有字段；old 非空时用于保留缺省值。 */
    private ObjectNode normalizeAgent(ObjectNode body, String id, ObjectNode old) {
        ObjectNode r = HttpJson.MAPPER.createObjectNode();
        r.put("agentId", id);
        r.put("url", pick(body, old, "url", ""));
        r.put("enabled", body.has("enabled") ? body.get("enabled").asBoolean()
                : (old != null && old.has("enabled") ? old.get("enabled").asBoolean(true) : true));
        r.put("version", pick(body, old, "version", "1.0.0"));
        // skills
        if (body.has("skills") && body.get("skills").isArray()) {
            r.set("skills", body.get("skills").deepCopy());
        } else if (old != null && old.has("skills")) {
            r.set("skills", old.get("skills").deepCopy());
        } else {
            r.set("skills", HttpJson.MAPPER.createArrayNode());
        }
        r.put("authPolicy", pick(body, old, "authPolicy", "default"));
        // webhooks
        if (body.has("webhooks") && body.get("webhooks").isArray()) {
            r.set("webhooks", body.get("webhooks").deepCopy());
        } else if (old != null && old.has("webhooks")) {
            r.set("webhooks", old.get("webhooks").deepCopy());
        } else {
            r.set("webhooks", HttpJson.MAPPER.createArrayNode());
        }
        // metadata（含 userServer）
        ObjectNode meta = HttpJson.MAPPER.createObjectNode();
        if (old != null && old.has("metadata") && old.get("metadata").isObject()) {
            old.get("metadata").fields().forEachRemaining(e -> meta.set(e.getKey(), e.getValue().deepCopy()));
        }
        if (body.has("metadata") && body.get("metadata").isObject()) {
            body.get("metadata").fields().forEachRemaining(e -> meta.set(e.getKey(), e.getValue().deepCopy()));
        }
        r.set("metadata", meta);
        // 其余扩展字段（如 description）也保留
        List<String> known = new ArrayList<>(java.util.Arrays.asList(
                "agentId", "id", "url", "enabled", "version", "skills",
                "authPolicy", "webhooks", "metadata"));
        body.fields().forEachRemaining(e -> {
            if (!known.contains(e.getKey())) r.set(e.getKey(), e.getValue().deepCopy());
        });
        return r;
    }

    private void ensureAbsent(String id) {
        if (snapshot.agents().has(id)) {
            throw HttpJson.ApiException.invalid("agent already exists: " + id
                    + " (use PUT to update)");
        }
    }

    private static String text(ObjectNode n, String f) {
        JsonNode v = n.get(f);
        return v == null || v.isNull() ? "" : v.asText();
    }

    private static String pick(ObjectNode body, ObjectNode old, String f, String def) {
        JsonNode v = body.get(f);
        if (v != null && !v.isNull()) return v.asText();
        if (old != null && old.has(f) && !old.get(f).isNull()) return old.get(f).asText();
        return def;
    }
}
