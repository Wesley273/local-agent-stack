package las.cp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;

/**
 * policies / grants / webhooks 注册中心。
 *
 * <p>policies：map<policyId, {type, keys:map<key,appId>, allowAnonymous}>。
 * keys 即 API Key -> appId 映射（契约 §4 policies.default）。
 * <p>grants：array<{appId, agentId, scopes[]}>。
 * <p>webhooks：全局 webhook 列表，单独存 data/control-plane-webhooks.json，
 * 不写进 /v1/config 快照（§4 快照字段必须逐字段对齐，不允许额外顶层字段）；
 * 但写操作仍通过 Snapshot.write 的 no-op mutation 触发 revision++。
 */
public final class PolicyRegistry {

    private final Snapshot snapshot;
    private final Path webhookFile;

    public PolicyRegistry(Snapshot snapshot) {
        this.snapshot = snapshot;
        Path cpFile = Paths.get("data/control-plane.json").toAbsolutePath();
        this.webhookFile = cpFile.resolveSibling("control-plane-webhooks.json");
    }

    // ------------------------------------------------------------------ policies

    public ObjectNode listPolicies() {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("policies", snapshot.policies());
        out.put("revision", snapshot.revision());
        return out;
    }

    public ObjectNode getPolicy(String id) {
        ObjectNode ps = snapshot.policies();
        JsonNode p = ps.get(id);
        if (p == null) {
            throw HttpJson.ApiException.notFound("INVALID_REQUEST", "policy not found: " + id);
        }
        return (ObjectNode) p.deepCopy();
    }

    /** PUT /v1/policies/{id}：创建或替换一个 policy。 */
    public ObjectNode putPolicy(String id, ObjectNode body) {
        ObjectNode rec = normalizePolicy(body);
        long rev = snapshot.write("PUT", "policies", id, root -> {
            ObjectNode ps = Snapshot.obj(root, "policies");
            ps.set(id, rec.deepCopy());
        });
        return HttpJson.ok(rev, "policy", rec);
    }

    /** POST /v1/policies：body 带 id 或 policyId。 */
    public ObjectNode createPolicy(ObjectNode body) {
        String id = text(body, "policyId");
        if (id.isEmpty()) id = text(body, "id");
        if (id.isEmpty()) throw HttpJson.ApiException.invalid("policyId is required");
        return putPolicy(id, body);
    }

    public ObjectNode deletePolicy(String id) {
        if (!snapshot.policies().has(id)) {
            throw HttpJson.ApiException.notFound("INVALID_REQUEST", "policy not found: " + id);
        }
        long rev = snapshot.write("DELETE", "policies", id, root ->
                Snapshot.obj(root, "policies").remove(id));
        return HttpJson.ok(rev, null, null);
    }

    /** 便捷：往 policy 的 keys 里加/改一条 apiKey -> appId。 */
    public ObjectNode putKey(String policyId, String key, String appId) {
        if (key.isEmpty() || appId.isEmpty()) {
            throw HttpJson.ApiException.invalid("key and appId are required");
        }
        ObjectNode[] holder = new ObjectNode[1];
        long rev = snapshot.write("PUT", "keys", policyId + "/" + key, root -> {
            ObjectNode p = Snapshot.obj(Snapshot.obj(root, "policies"), policyId);
            if (!p.has("type")) p.put("type", "apiKey");
            if (!p.has("allowAnonymous")) p.put("allowAnonymous", false);
            ObjectNode keys = Snapshot.obj(p, "keys");
            keys.put(key, appId);
            holder[0] = p.deepCopy();
        });
        return HttpJson.ok(rev, "policy", holder[0]);
    }

    // ------------------------------------------------------------------ grants

    public ObjectNode listGrants() {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("grants", snapshot.grants());
        out.put("revision", snapshot.revision());
        return out;
    }

    /** POST /v1/grants：追加一条授权。 */
    public ObjectNode createGrant(ObjectNode body) {
        ObjectNode rec = normalizeGrant(body);
        long rev = snapshot.write("POST", "grants", rec.path("appId").asText(), root ->
                Snapshot.arr(root, "grants").add(rec.deepCopy()));
        return HttpJson.ok(rev, "grant", rec);
    }

    /** PUT /v1/grants：整体替换 grants 数组（body 为 {"grants":[...]} 或直接是数组）。 */
    public ObjectNode putGrants(JsonNode body) {
        ArrayNode arr = asArray(body, "grants");
        ArrayNode norm = HttpJson.MAPPER.createArrayNode();
        for (JsonNode g : arr) {
            if (g.isObject()) norm.add(normalizeGrant((ObjectNode) g));
        }
        long rev = snapshot.write("PUT", "grants", "*", root -> root.set("grants", norm.deepCopy()));
        return HttpJson.ok(rev, "grants", norm);
    }

    /** DELETE /v1/grants：删除匹配 appId+agentId 的授权。 */
    public ObjectNode deleteGrant(String appId, String agentId) {
        long rev = snapshot.write("DELETE", "grants", appId + "/" + agentId, root -> {
            ArrayNode grants = Snapshot.arr(root, "grants");
            ArrayNode kept = HttpJson.MAPPER.createArrayNode();
            for (JsonNode g : grants) {
                boolean match = appId.equals(g.path("appId").asText())
                        && (agentId == null || agentId.isEmpty() || agentId.equals(g.path("agentId").asText()));
                if (!match) kept.add(g.deepCopy());
            }
            root.set("grants", kept);
        });
        return HttpJson.ok(rev, null, null);
    }

    // ------------------------------------------------------------------ webhooks（全局，独立文件）

    private ArrayNode loadWebhooks() {
        try {
            if (Files.exists(webhookFile)) {
                JsonNode n = HttpJson.MAPPER.readTree(
                        new String(Files.readAllBytes(webhookFile), java.nio.charset.StandardCharsets.UTF_8));
                if (n != null && n.isArray()) return (ArrayNode) n.deepCopy();
            }
        } catch (IOException e) {
            System.err.println("[webhooks] load failed: " + e);
        }
        return HttpJson.MAPPER.createArrayNode();
    }

    private void saveWebhooks(ArrayNode arr) {
        try {
            Files.createDirectories(webhookFile.getParent());
            Files.write(webhookFile, HttpJson.MAPPER.writeValueAsBytes(arr));
        } catch (IOException e) {
            throw HttpJson.ApiException.internal("persist webhooks failed: " + e);
        }
    }

    public ObjectNode listWebhooks() {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("webhooks", loadWebhooks());
        out.put("revision", snapshot.revision());
        return out;
    }

    /** POST /v1/webhooks：body {"url":"..."} 或 {"webhooks":[...]}。 */
    public ObjectNode createWebhook(ObjectNode body) {
        ArrayNode add = HttpJson.MAPPER.createArrayNode();
        if (body.has("url")) add.add(body.get("url").asText());
        if (body.has("webhooks") && body.get("webhooks").isArray()) {
            for (JsonNode u : body.get("webhooks")) add.add(u.asText());
        }
        if (add.isEmpty()) throw HttpJson.ApiException.invalid("url is required");
        ArrayNode arr = loadWebhooks();
        for (JsonNode u : add) arr.add(u.asText());
        saveWebhooks(arr);
        // no-op mutation：不往快照加额外字段，但仍让 revision++ 触发数据面热更新
        long rev = snapshot.write("POST", "webhooks", "*", root -> {});
        return HttpJson.ok(rev, "webhooks", arr);
    }

    /** DELETE /v1/webhooks：按 url 删除；不带 url 则清空。 */
    public ObjectNode deleteWebhook(String url) {
        ArrayNode arr = loadWebhooks();
        ArrayNode kept = HttpJson.MAPPER.createArrayNode();
        boolean clearAll = (url == null || url.isEmpty());
        for (JsonNode u : arr) {
            if (!clearAll && !u.asText().equals(url)) kept.add(u.asText());
        }
        saveWebhooks(kept);
        long rev = snapshot.write("DELETE", "webhooks", url == null ? "" : url, root -> {});
        return HttpJson.ok(rev, "webhooks", kept);
    }

    // ------------------------------------------------------------------ helpers

    private ObjectNode normalizePolicy(ObjectNode body) {
        ObjectNode rec = HttpJson.MAPPER.createObjectNode();
        rec.put("type", body.has("type") ? body.get("type").asText() : "apiKey");
        ObjectNode keys = HttpJson.MAPPER.createObjectNode();
        if (body.has("keys") && body.get("keys").isObject()) {
            body.get("keys").fields().forEachRemaining(e -> keys.put(e.getKey(), e.getValue().asText()));
        }
        rec.set("keys", keys);
        rec.put("allowAnonymous", body.has("allowAnonymous") && body.get("allowAnonymous").asBoolean());
        return rec;
    }

    private ObjectNode normalizeGrant(ObjectNode body) {
        ObjectNode rec = HttpJson.MAPPER.createObjectNode();
        rec.put("appId", text(body, "appId"));
        rec.put("agentId", text(body, "agentId").isEmpty() ? "*" : text(body, "agentId"));
        ArrayNode scopes = HttpJson.MAPPER.createArrayNode();
        if (body.has("scopes") && body.get("scopes").isArray()) {
            for (JsonNode s : body.get("scopes")) scopes.add(s.asText());
        } else {
            scopes.add("*");
        }
        rec.set("scopes", scopes);
        if (rec.path("appId").asText().isEmpty()) {
            throw HttpJson.ApiException.invalid("appId is required");
        }
        return rec;
    }

    private static ArrayNode asArray(JsonNode body, String field) {
        if (body.isArray()) return (ArrayNode) body.deepCopy();
        if (body.isObject() && body.has(field) && body.get(field).isArray()) {
            return (ArrayNode) body.get(field).deepCopy();
        }
        throw HttpJson.ApiException.invalid("expected an array or {\"" + field + "\":[...]}");
    }

    private static String text(ObjectNode n, String f) {
        JsonNode v = n.get(f);
        return v == null || v.isNull() ? "" : v.asText();
    }
}
