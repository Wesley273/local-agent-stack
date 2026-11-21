package las.cp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

/**
 * models[] / rateLimits / guardrails 注册中心。
 *
 * <p>models：数组，每项 {name, backend, priority, weight, enabled}；支持多 backend 加权。
 * <p>rateLimits：map<appId|"default", {requestsPerMinute, tokensPerMinute, burst}>。
 * <p>guardrails：{blockedWords[], blockedTools[], maxPromptChars, enableInput, enableOutput}。
 * 这三项都是 §4 快照字段，写操作统一走 Snapshot.write()（revision++ / 落盘 / 事件）。
 */
public final class ModelRegistry {

    private final Snapshot snapshot;

    public ModelRegistry(Snapshot snapshot) {
        this.snapshot = snapshot;
    }

    // ------------------------------------------------------------------ models

    public ObjectNode listModels() {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("models", snapshot.models());
        out.put("revision", snapshot.revision());
        return out;
    }

    public ObjectNode getModel(String name) {
        for (JsonNode m : snapshot.models()) {
            if (name.equals(m.path("name").asText())) return (ObjectNode) m.deepCopy();
        }
        throw HttpJson.ApiException.notFound("MODEL_NOT_FOUND", "model not found: " + name);
    }

    /** POST /v1/models：追加一个 backend。 */
    public ObjectNode createModel(ObjectNode body) {
        ObjectNode rec = normalizeModel(body);
        final String name = rec.path("name").asText();
        for (JsonNode m : snapshot.models()) {
            if (name.equals(m.path("name").asText())) {
                throw HttpJson.ApiException.invalid("model already exists: " + name
                        + " (use PUT to update)");
            }
        }
        long rev = snapshot.write("POST", "models", name, root ->
                Snapshot.arr(root, "models").add(rec.deepCopy()));
        return HttpJson.ok(rev, "model", rec);
    }

    /** PUT /v1/models/{name}：更新（或 upsert）同名 model。 */
    public ObjectNode putModel(String name, ObjectNode body) {
        body.put("name", name);
        ObjectNode rec = normalizeModel(body);
        long rev = snapshot.write("PUT", "models", name, root -> {
            ArrayNode models = Snapshot.arr(root, "models");
            ArrayNode kept = HttpJson.MAPPER.createArrayNode();
            boolean replaced = false;
            for (JsonNode m : models) {
                if (name.equals(m.path("name").asText())) {
                    kept.add(rec.deepCopy());
                    replaced = true;
                } else kept.add(m.deepCopy());
            }
            if (!replaced) kept.add(rec.deepCopy());
            root.set("models", kept);
        });
        return HttpJson.ok(rev, "model", rec);
    }

    /** PUT /v1/models：整体替换（body {"models":[...]} 或直接数组）。 */
    public ObjectNode putModels(JsonNode body) {
        ArrayNode arr = asArray(body, "models");
        ArrayNode norm = HttpJson.MAPPER.createArrayNode();
        for (JsonNode m : arr) if (m.isObject()) norm.add(normalizeModel((ObjectNode) m));
        long rev = snapshot.write("PUT", "models", "*", root -> root.set("models", norm.deepCopy()));
        return HttpJson.ok(rev, "models", norm);
    }

    public ObjectNode deleteModel(String name) {
        boolean found = false;
        for (JsonNode m : snapshot.models()) {
            if (name.equals(m.path("name").asText())) { found = true; break; }
        }
        if (!found) throw HttpJson.ApiException.notFound("MODEL_NOT_FOUND", "model not found: " + name);
        long rev = snapshot.write("DELETE", "models", name, root -> {
            ArrayNode models = Snapshot.arr(root, "models");
            ArrayNode kept = HttpJson.MAPPER.createArrayNode();
            for (JsonNode m : models) if (!name.equals(m.path("name").asText())) kept.add(m.deepCopy());
            root.set("models", kept);
        });
        return HttpJson.ok(rev, null, null);
    }

    // ------------------------------------------------------------------ rateLimits

    public ObjectNode listRateLimits() {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("rateLimits", snapshot.rateLimits());
        out.put("revision", snapshot.revision());
        return out;
    }

    public ObjectNode getRateLimit(String key) {
        ObjectNode rl = snapshot.rateLimits();
        JsonNode n = rl.get(key);
        if (n == null) throw HttpJson.ApiException.notFound("INVALID_REQUEST",
                "rateLimit not found: " + key);
        return (ObjectNode) n.deepCopy();
    }

    /** PUT /v1/ratelimits/{key}：创建/更新一条限流规则（E21 用它改 app-demo）。 */
    public ObjectNode putRateLimit(String key, ObjectNode body) {
        ObjectNode rec = normalizeRateLimit(body);
        long rev = snapshot.write("PUT", "rateLimits", key, root -> {
            ObjectNode rl = Snapshot.obj(root, "rateLimits");
            rl.set(key, rec.deepCopy());
        });
        return HttpJson.ok(rev, "rateLimit", rec);
    }

    /** PUT /v1/ratelimits：整体替换（body {"rateLimits":{...}} 或直接对象）。 */
    public ObjectNode putRateLimits(JsonNode body) {
        JsonNode src = body.isObject() && body.has("rateLimits") ? body.get("rateLimits") : body;
        if (!src.isObject()) throw HttpJson.ApiException.invalid("rateLimits must be an object");
        ObjectNode norm = HttpJson.MAPPER.createObjectNode();
        src.fields().forEachRemaining(e -> {
            if (e.getValue().isObject()) norm.set(e.getKey(), normalizeRateLimit((ObjectNode) e.getValue()));
        });
        long rev = snapshot.write("PUT", "rateLimits", "*", root -> root.set("rateLimits", norm.deepCopy()));
        return HttpJson.ok(rev, "rateLimits", norm);
    }

    public ObjectNode deleteRateLimit(String key) {
        if (!snapshot.rateLimits().has(key)) {
            throw HttpJson.ApiException.notFound("INVALID_REQUEST", "rateLimit not found: " + key);
        }
        long rev = snapshot.write("DELETE", "rateLimits", key, root ->
                Snapshot.obj(root, "rateLimits").remove(key));
        return HttpJson.ok(rev, null, null);
    }

    // ------------------------------------------------------------------ guardrails

    public ObjectNode getGuardrails() {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.set("guardrails", snapshot.guardrails());
        out.put("revision", snapshot.revision());
        return out;
    }

    /** PUT /v1/guardrails：整体替换或局部合并。 */
    public ObjectNode putGuardrails(ObjectNode body) {
        JsonNode src = body.has("guardrails") && body.get("guardrails").isObject()
                ? body.get("guardrails") : body;
        if (!src.isObject()) throw HttpJson.ApiException.invalid("guardrails must be an object");
        ObjectNode rec = normalizeGuardrails((ObjectNode) src);
        long rev = snapshot.write("PUT", "guardrails", "*", root -> root.set("guardrails", rec.deepCopy()));
        return HttpJson.ok(rev, "guardrails", rec);
    }

    // ------------------------------------------------------------------ helpers

    private ObjectNode normalizeModel(ObjectNode body) {
        ObjectNode rec = HttpJson.MAPPER.createObjectNode();
        String name = text(body, "name");
        if (name.isEmpty()) throw HttpJson.ApiException.invalid("model.name is required");
        rec.put("name", name);
        rec.put("backend", text(body, "backend"));
        rec.put("priority", body.has("priority") ? body.get("priority").asInt() : 10);
        rec.put("weight", body.has("weight") ? body.get("weight").asInt() : 100);
        rec.put("enabled", !body.has("enabled") || body.get("enabled").asBoolean(true));
        return rec;
    }

    private ObjectNode normalizeRateLimit(ObjectNode body) {
        ObjectNode rec = HttpJson.MAPPER.createObjectNode();
        rec.put("requestsPerMinute", body.has("requestsPerMinute")
                ? body.get("requestsPerMinute").asInt() : 120);
        rec.put("tokensPerMinute", body.has("tokensPerMinute")
                ? body.get("tokensPerMinute").asInt() : 60000);
        rec.put("burst", body.has("burst") ? body.get("burst").asInt() : 20);
        return rec;
    }

    private ObjectNode normalizeGuardrails(ObjectNode body) {
        ObjectNode rec = HttpJson.MAPPER.createObjectNode();
        ArrayNode bw = HttpJson.MAPPER.createArrayNode();
        if (body.has("blockedWords") && body.get("blockedWords").isArray()) {
            for (JsonNode w : body.get("blockedWords")) bw.add(w.asText());
        }
        rec.set("blockedWords", bw);
        ArrayNode bt = HttpJson.MAPPER.createArrayNode();
        if (body.has("blockedTools") && body.get("blockedTools").isArray()) {
            for (JsonNode t : body.get("blockedTools")) bt.add(t.asText());
        }
        rec.set("blockedTools", bt);
        rec.put("maxPromptChars", body.has("maxPromptChars")
                ? body.get("maxPromptChars").asInt() : 8000);
        rec.put("enableInput", !body.has("enableInput") || body.get("enableInput").asBoolean(true));
        rec.put("enableOutput", !body.has("enableOutput") || body.get("enableOutput").asBoolean(true));
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
