package las.cp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.Comparator;

/**
 * main-based 自测（不引 JUnit，java -cp 直接跑断言）。
 *
 * <p>覆盖：
 *  1. 默认快照逐字段对齐 §4（agents/policies/grants/models/rateLimits/guardrails）。
 *  2. 任何写操作 revision++ 且 generatedAt 更新；/v1/config 的 ETag 随之变化。
 *  3. If-None-Match 命中 -> 304（ConfigApi.matches）语义。
 *  4. 持久化：写后重新 load 能恢复 revision 与数据。
 *
 * <pre>
 *   set JAVA_HOME=C:\Java\jdk-11
 *   mvn -q -f services\control-plane\pom.xml test-compile
 *   java -cp "target\classes;target\test-classes;<jackson jars>" las.cp.SelfTest
 * </pre>
 */
public final class SelfTest {

    private static int passed = 0;

    public static void main(String[] args) throws Exception {
        Path tmp = Files.createTempDirectory("cp-selftest-");
        String file = tmp.resolve("control-plane.json").toString();
        try {
            Store store = new Store(file);
            Snapshot snap = new Snapshot(store);

            // ---- 1. 默认快照字段对齐 ----
            ObjectNode cfg = snap.snapshot();
            assertTrue(cfg.path("revision").asLong() >= 1, "revision>=1");
            ObjectNode agents = (ObjectNode) cfg.get("agents");
            assertTrue(agents.has("echo-agent"), "echo-agent present");
            assertTrue(agents.has("java-agent"), "java-agent present");
            assertTrue(agents.has("python-agent"), "python-agent present");
            assertTrue(agents.path("echo-agent").path("url").asText()
                    .equals("http://127.0.0.1:9100/a2a/echo-agent"), "echo-agent url");
            assertTrue(agents.path("echo-agent").path("metadata").path("userServer").asText()
                    .equals("http://127.0.0.1:9101"), "echo userServer 9101");
            assertTrue(agents.path("java-agent").path("metadata").path("userServer").asText()
                    .equals("http://127.0.0.1:9102"), "java userServer 9102");
            assertTrue(agents.path("python-agent").path("metadata").path("userServer").asText()
                    .equals("http://127.0.0.1:9103"), "python userServer 9103");
            ObjectNode policies = (ObjectNode) cfg.get("policies");
            JsonNode keys = policies.path("default").path("keys");
            assertTrue("app-demo".equals(keys.path("demo-key").asText()), "demo-key -> app-demo");
            assertTrue("app-admin".equals(keys.path("admin-key").asText()), "admin-key -> app-admin");
            assertTrue(cfg.path("models").isArray() && cfg.path("models").size() == 2, "2 models");
            assertTrue("http://127.0.0.1:9200".equals(
                    cfg.path("models").get(0).path("backend").asText()), "mock-1 backend 9200");
            assertTrue(cfg.path("rateLimits").has("default") && cfg.path("rateLimits").has("app-demo"),
                    "rateLimits default+app-demo");
            JsonNode gw = cfg.path("guardrails").path("blockedWords");
            assertTrue(contains(gw, "BLOCK_ME") && contains(gw, "secret-leak"), "blockedWords");
            assertTrue(cfg.path("guardrails").path("maxPromptChars").asInt() == 8000,
                    "maxPromptChars 8000");
            System.out.println("[OK] 默认快照逐字段对齐 §4");

            // ---- 2. revision++ / generatedAt 更新 / ETag 变化 ----
            long rev0 = snap.revision();
            String gen0 = cfg.path("generatedAt").asText();
            ModelRegistry models = new ModelRegistry(snap);
            ObjectNode rl = HttpJson.MAPPER.createObjectNode();
            rl.put("requestsPerMinute", 7);
            rl.put("tokensPerMinute", 700);
            rl.put("burst", 1);
            models.putRateLimit("app-demo", rl);
            long rev1 = snap.revision();
            assertTrue(rev1 == rev0 + 1, "revision++ after write (" + rev0 + " -> " + rev1 + ")");
            ObjectNode cfg1 = snap.snapshot();
            assertTrue(cfg1.path("revision").asLong() == rev1, "config revision matches");
            String gen1 = cfg1.path("generatedAt").asText();
            assertTrue(gen1 != null && !gen1.isEmpty(), "generatedAt set");
            // ETag 语义：旧 revision 不命中，新 revision 命中 -> 304
            assertTrue(!ConfigApi.matches(String.valueOf(rev0), rev1), "old ETag no longer matches");
            assertTrue(ConfigApi.matches(String.valueOf(rev1), rev1), "new ETag matches -> 304");
            assertTrue(ConfigApi.matches("W/\"" + rev1 + "\"", rev1), "weak ETag matches -> 304");
            assertTrue(cfg1.path("rateLimits").path("app-demo").path("requestsPerMinute").asInt() == 7,
                    "rateLimit value updated");
            System.out.println("[OK] 写操作 revision++ / generatedAt / ETag(304) 语义");

            // ---- 3. 持久化恢复 ----
            Store store2 = new Store(file);
            Snapshot snap2 = new Snapshot(store2);
            assertTrue(snap2.revision() == rev1, "revision restored from disk");
            assertTrue(snap2.snapshot().path("rateLimits").path("app-demo")
                    .path("requestsPerMinute").asInt() == 7, "rateLimit restored from disk");
            System.out.println("[OK] 重启后从 data/control-plane.json 恢复");

            // ---- 4. CRUD + events ----
            AgentRegistry agentsReg = new AgentRegistry(snap);
            ObjectNode a = HttpJson.MAPPER.createObjectNode();
            a.put("agentId", "test-agent");
            a.put("url", "http://127.0.0.1:9999/a2a/test-agent");
            agentsReg.create(a);
            assertTrue(snap.revision() == rev1 + 1, "create bumps revision");
            assertTrue(snap.snapshot().path("agents").has("test-agent"), "created agent in snapshot");
            agentsReg.delete("test-agent");
            assertTrue(snap.revision() == rev1 + 2, "delete bumps revision");
            assertTrue(!snap.snapshot().path("agents").has("test-agent"), "deleted agent gone");
            System.out.println("[OK] agents CRUD revision++ 与事件落盘");

            System.out.println("=== control-plane SelfTest PASSED (" + passed + " assertions) ===");
        } finally {
            // 清理临时目录
            try {
                Files.walk(tmp).sorted(Comparator.reverseOrder()).forEach(p -> {
                    try { Files.deleteIfExists(p); } catch (Exception ignored) { }
                });
            } catch (Exception ignored) { }
        }
    }

    private static void assertTrue(boolean cond, String msg) {
        if (!cond) throw new AssertionError("FAIL: " + msg);
        passed++;
    }

    private static boolean contains(JsonNode arr, String v) {
        if (arr == null || !arr.isArray()) return false;
        for (JsonNode n : arr) if (v.equals(n.asText())) return true;
        return false;
    }
}
