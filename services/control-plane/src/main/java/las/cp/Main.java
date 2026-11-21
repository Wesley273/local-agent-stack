package las.cp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpServer;

import java.net.InetSocketAddress;
import java.util.concurrent.Executors;

/**
 * 控制面启动入口（端口 9000，config/ports.env CP_PORT）。
 *
 * <pre>
 *   # 编译
 *   set JAVA_HOME=C:\Java\jdk-11
 *   mvn -B clean compile
 *   # 运行（二选一）
 *   mvn -q exec:java
 *   java -cp "target\classes;C:\Users\...\.m2\repository\com\fasterxml\jackson\core\jackson-databind\2.17.2\jackson-databind-2.17.2.jar;...jackson-core...jar;...jackson-annotations...jar" las.cp.Main 9000
 * </pre>
 *
 * <p>端点：
 *   GET  /health
 *   GET  /v1/config            (ETag=revision / If-None-Match 304)
 *   GET  /v1/events
 *   CRUD /v1/agents|skills|policies|grants|models|ratelimits|guardrails|webhooks
 *   POST /rpc                  (JSON-RPC 2.0 管理快捷方式)
 *
 * <p>所有写操作 revision++ 并原子落盘到 data/control-plane.json，重启可恢复（E21）。
 */
public final class Main {

    public static void main(String[] args) throws Exception {
        int port = args.length > 0 ? Integer.parseInt(args[0]) : 9000;
        String dataFile = args.length > 1 ? args[1] : "data/control-plane.json";

        Store store = new Store(dataFile);
        Snapshot snapshot = new Snapshot(store);
        AgentRegistry agents = new AgentRegistry(snapshot);
        PolicyRegistry policies = new PolicyRegistry(snapshot);
        ModelRegistry models = new ModelRegistry(snapshot);
        Events events = new Events(snapshot, store);
        ConfigApi config = new ConfigApi(snapshot);
        CrudApi crud = new CrudApi(agents, policies, models);
        Health health = new Health(snapshot);

        JsonRpc.Dispatcher dispatcher = new JsonRpc.Dispatcher()
                .register("cp.config", p -> snapshot.snapshot())
                .register("cp.health", p -> {
                    ObjectNode r = HttpJson.MAPPER.createObjectNode();
                    r.put("status", "UP");
                    r.put("service", "control-plane");
                    r.put("revision", snapshot.revision());
                    return r;
                })
                .register("cp.agents", p -> agents.list().get("agents"))
                .register("cp.policies", p -> policies.listPolicies().get("policies"))
                .register("cp.grants", p -> policies.listGrants().get("grants"))
                .register("cp.models", p -> models.listModels().get("models"))
                .register("cp.rateLimits", p -> models.listRateLimits().get("rateLimits"))
                .register("cp.guardrails", p -> models.getGuardrails().get("guardrails"))
                .register("cp.events", p -> events.list())
                .register("cp.bump", p -> {
                    long rev = snapshot.write("RPC", "bump", "*", root -> {});
                    ObjectNode r = HttpJson.MAPPER.createObjectNode();
                    r.put("revision", rev);
                    return r;
                });

        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", port), 0);
        server.createContext("/health", HttpJson.wrap(health::handle));
        server.createContext("/v1/config", HttpJson.wrap(config::handle));
        server.createContext("/v1/events", HttpJson.wrap(ex -> ConfigApi.events(ex, events)));
        server.createContext("/v1/", HttpJson.wrap(crud::handle));
        server.createContext("/rpc", HttpJson.wrap(ex -> JsonRpc.handle(ex, dispatcher)));
        server.setExecutor(Executors.newFixedThreadPool(8));
        server.start();

        System.out.println("[control-plane] UP on http://127.0.0.1:" + port
                + " | revision=" + snapshot.revision()
                + " | data=" + store.path()
                + " | JDK=" + System.getProperty("java.version"));
        Runtime.getRuntime().addShutdownHook(new Thread(() -> server.stop(0)));
        Thread.currentThread().join();
    }

    private Main() {}
}
