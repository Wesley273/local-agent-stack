package las.cp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpExchange;

import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;

/**
 * CRUD REST 路由（一个 /v1/ 前缀 handler 内手动解析，避免注册大量 context）。
 *
 * <p>覆盖端点：
 * <pre>
 *   /v1/agents                GET / POST
 *   /v1/agents/{id}           GET / PUT / PATCH / DELETE
 *   /v1/agents/{id}/skills    GET
 *   /v1/agents/{id}/enable    POST  {"enabled":true/false}
 *   /v1/skills                GET / PUT / POST
 *   /v1/policies              GET / POST
 *   /v1/policies/{id}         GET / PUT / DELETE
 *   /v1/policies/{id}/keys    PUT  {"key":"...","appId":"..."}
 *   /v1/grants                GET / POST / PUT / DELETE
 *   /v1/models                GET / POST / PUT
 *   /v1/models/{name}         GET / PUT / DELETE
 *   /v1/ratelimits            GET / PUT
 *   /v1/ratelimits/{key}      GET / PUT / DELETE
 *   /v1/guardrails            GET / PUT
 *   /v1/webhooks              GET / POST / DELETE
 * </pre>
 * 所有写操作 -> Snapshot.write -> revision++ / generatedAt / 落盘 / 事件，GET /v1/config 立即变化。
 */
public final class CrudApi {

    private final AgentRegistry agents;
    private final PolicyRegistry policies;
    private final ModelRegistry models;

    public CrudApi(AgentRegistry agents, PolicyRegistry policies, ModelRegistry models) {
        this.agents = agents;
        this.policies = policies;
        this.models = models;
    }

    /** 处理 /v1/ 下除 config/events/health 之外的所有路径。 */
    public void handle(HttpExchange ex) throws Exception {
        String method = ex.getRequestMethod().toUpperCase();
        String path = ex.getRequestURI().getPath();
        String[] seg = split(path);           // seg[0]=="" (前导斜杠), seg[1]=="v1", ...
        Map<String, String> q = query(ex);

        if (seg.length < 3) throw HttpJson.ApiException.invalid("unknown path: " + path);
        String resource = seg[2];
        String id = seg.length > 3 ? decode(seg[3]) : null;
        String sub = seg.length > 4 ? decode(seg[4]) : null;

        switch (resource) {
            case "agents":      handleAgents(ex, method, id, sub, q); return;
            case "skills":      handleSkills(ex, method); return;
            case "policies":    handlePolicies(ex, method, id, sub); return;
            case "grants":      handleGrants(ex, method, q); return;
            case "models":      handleModels(ex, method, id); return;
            case "ratelimits":  handleRateLimits(ex, method, id); return;
            case "guardrails":  handleGuardrails(ex, method); return;
            case "webhooks":    handleWebhooks(ex, method, q); return;
            default: throw HttpJson.ApiException.invalid("unknown resource: /v1/" + resource);
        }
    }

    // ------------------------------------------------------------------ agents

    private void handleAgents(HttpExchange ex, String m, String id, String sub,
                              Map<String, String> q) throws Exception {
        if (id == null) {
            switch (m) {
                case "GET":  HttpJson.send(ex, 200, agents.list()); return;
                case "POST": HttpJson.send(ex, 201, agents.create(body(ex))); return;
                default: throw methodNotAllowed(m);
            }
        }
        // 子资源
        if (sub != null) {
            if (sub.equals("skills") && m.equals("GET")) {
                HttpJson.send(ex, 200, agents.skills(id)); return;
            }
            if (sub.equals("enable") && (m.equals("POST") || m.equals("PUT"))) {
                ObjectNode b = body(ex);
                boolean enabled = b.has("enabled") && b.get("enabled").asBoolean(true);
                HttpJson.send(ex, 200, agents.setEnabled(id, enabled)); return;
            }
            throw HttpJson.ApiException.invalid("unknown agent sub-resource: " + sub);
        }
        switch (m) {
            case "GET":    HttpJson.send(ex, 200, agents.get(id)); return;
            case "PUT":    HttpJson.send(ex, 200, agents.put(id, body(ex))); return;
            case "PATCH":  HttpJson.send(ex, 200, agents.patch(id, body(ex))); return;
            case "POST":   HttpJson.send(ex, 200, agents.patch(id, body(ex))); return; // 兼容
            case "DELETE": HttpJson.send(ex, 200, agents.delete(id)); return;
            default: throw methodNotAllowed(m);
        }
    }

    private void handleSkills(HttpExchange ex, String m) throws Exception {
        switch (m) {
            case "GET": {
                ObjectNode out = HttpJson.MAPPER.createObjectNode();
                out.set("agents", agents.list().get("agents"));
                HttpJson.send(ex, 200, out); return;
            }
            case "PUT":
            case "POST": HttpJson.send(ex, 200, agents.putSkills(body(ex))); return;
            default: throw methodNotAllowed(m);
        }
    }

    // ------------------------------------------------------------------ policies

    private void handlePolicies(HttpExchange ex, String m, String id, String sub) throws Exception {
        if (id == null) {
            switch (m) {
                case "GET":  HttpJson.send(ex, 200, policies.listPolicies()); return;
                case "POST": HttpJson.send(ex, 201, policies.createPolicy(body(ex))); return;
                default: throw methodNotAllowed(m);
            }
        }
        if (sub != null && sub.equals("keys") && (m.equals("PUT") || m.equals("POST"))) {
            ObjectNode b = body(ex);
            HttpJson.send(ex, 200, policies.putKey(id,
                    b.path("key").asText(""), b.path("appId").asText("")));
            return;
        }
        switch (m) {
            case "GET":    HttpJson.send(ex, 200, policies.getPolicy(id)); return;
            case "PUT":    HttpJson.send(ex, 200, policies.putPolicy(id, body(ex))); return;
            case "DELETE": HttpJson.send(ex, 200, policies.deletePolicy(id)); return;
            default: throw methodNotAllowed(m);
        }
    }

    private void handleGrants(HttpExchange ex, String m, Map<String, String> q) throws Exception {
        switch (m) {
            case "GET":  HttpJson.send(ex, 200, policies.listGrants()); return;
            case "POST": HttpJson.send(ex, 201, policies.createGrant(body(ex))); return;
            case "PUT":  HttpJson.send(ex, 200, policies.putGrants(HttpJson.parse(read(ex)))); return;
            case "DELETE": HttpJson.send(ex, 200,
                    policies.deleteGrant(q.get("appId"), q.get("agentId"))); return;
            default: throw methodNotAllowed(m);
        }
    }

    // ------------------------------------------------------------------ models

    private void handleModels(HttpExchange ex, String m, String id) throws Exception {
        if (id == null) {
            switch (m) {
                case "GET":  HttpJson.send(ex, 200, models.listModels()); return;
                case "POST": HttpJson.send(ex, 201, models.createModel(body(ex))); return;
                case "PUT":  HttpJson.send(ex, 200, models.putModels(HttpJson.parse(read(ex)))); return;
                default: throw methodNotAllowed(m);
            }
        }
        switch (m) {
            case "GET":    HttpJson.send(ex, 200, models.getModel(id)); return;
            case "PUT":    HttpJson.send(ex, 200, models.putModel(id, body(ex))); return;
            case "DELETE": HttpJson.send(ex, 200, models.deleteModel(id)); return;
            default: throw methodNotAllowed(m);
        }
    }

    // ------------------------------------------------------------------ rateLimits / guardrails

    private void handleRateLimits(HttpExchange ex, String m, String id) throws Exception {
        if (id == null) {
            switch (m) {
                case "GET": HttpJson.send(ex, 200, models.listRateLimits()); return;
                case "PUT": HttpJson.send(ex, 200, models.putRateLimits(HttpJson.parse(read(ex)))); return;
                default: throw methodNotAllowed(m);
            }
        }
        switch (m) {
            case "GET":    HttpJson.send(ex, 200, models.getRateLimit(id)); return;
            case "PUT":    HttpJson.send(ex, 200, models.putRateLimit(id, body(ex))); return;
            case "DELETE": HttpJson.send(ex, 200, models.deleteRateLimit(id)); return;
            default: throw methodNotAllowed(m);
        }
    }

    private void handleGuardrails(HttpExchange ex, String m) throws Exception {
        switch (m) {
            case "GET": HttpJson.send(ex, 200, models.getGuardrails()); return;
            case "PUT": HttpJson.send(ex, 200, models.putGuardrails(body(ex))); return;
            default: throw methodNotAllowed(m);
        }
    }

    private void handleWebhooks(HttpExchange ex, String m, Map<String, String> q) throws Exception {
        switch (m) {
            case "GET":    HttpJson.send(ex, 200, policies.listWebhooks()); return;
            case "POST":   HttpJson.send(ex, 201, policies.createWebhook(body(ex))); return;
            case "DELETE": HttpJson.send(ex, 200, policies.deleteWebhook(q.get("url"))); return;
            default: throw methodNotAllowed(m);
        }
    }

    // ------------------------------------------------------------------ helpers

    private static HttpJson.ApiException methodNotAllowed(String m) {
        return new HttpJson.ApiException(405, "INVALID_REQUEST",
                "method not allowed: " + m + " (契约 §0.3 未定义 405 专用码，用 INVALID_REQUEST)", null);
    }

    private static String read(HttpExchange ex) throws Exception {
        return HttpJson.readBody(ex);
    }

    private static ObjectNode body(HttpExchange ex) throws Exception {
        return HttpJson.parseObject(read(ex));
    }

    private static String[] split(String path) {
        // "/v1/agents/echo-agent/skills" -> ["", "v1", "agents", "echo-agent", "skills"]
        String[] raw = path.split("/", -1);
        return raw;
    }

    private static String decode(String s) {
        try {
            return URLDecoder.decode(s, StandardCharsets.UTF_8.name());
        } catch (Exception e) {
            return s;
        }
    }

    private static Map<String, String> query(HttpExchange ex) {
        Map<String, String> m = new HashMap<>();
        String q = ex.getRequestURI().getRawQuery();
        if (q == null) return m;
        for (String p : q.split("&")) {
            int i = p.indexOf('=');
            try {
                String k = decode(i < 0 ? p : p.substring(0, i));
                String v = i < 0 ? "" : decode(p.substring(i + 1));
                m.put(k, v);
            } catch (Exception ignored) { }
        }
        return m;
    }
}
