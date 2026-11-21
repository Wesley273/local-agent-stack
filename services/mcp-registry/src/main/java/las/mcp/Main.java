package las.mcp;

import com.sun.net.httpserver.HttpServer;

import java.net.InetSocketAddress;
import java.util.concurrent.Executors;

/**
 * MCP 注册中心启动入口（端口 9300，config/ports.env MCP_PORT）。
 *
 * <pre>
 *   # 编译
 *   set JAVA_HOME=C:\Java\jdk-11
 *   mvn -B clean compile
 *   # 运行（二选一）
 *   mvn -q exec:java
 *   java -cp "target\classes;<jackson-databind-2.17.2.jar>;<jackson-core-2.17.2.jar>;<jackson-annotations-2.17.2.jar>" las.mcp.Main 9300
 * </pre>
 *
 * <p>端点：
 *   GET  /health
 *   POST /mcp                       (JSON-RPC 2.0: initialize/tools/list/tools/call/tools/get/ping)
 *   POST /registry/import          (OpenAPI → MCP 转换 + 落盘)
 *   POST /registry/validate        (只校验不导入)
 *   GET  /registry/tools           (?q= 检索)
 *   GET  /registry/tools/{name}
 *   POST /registry/tools           (手动注册)
 *   DELETE /registry/tools/{name}
 *   GET  /registry/specs
 *
 * <p>工具持久化到 data/mcp-registry.json；tools/call 用 source 回放真实 HTTP 请求
 * （默认打到 http://127.0.0.1:9200/tools/echo，E19）。
 */
public final class Main {

    public static void main(String[] args) throws Exception {
        int port = args.length > 0 ? Integer.parseInt(args[0]) : 9300;
        String dataFile = args.length > 1 ? args[1] : "data/mcp-registry.json";

        Registry registry = new Registry(dataFile);
        McpApi mcp = new McpApi(registry);
        ImportApi imports = new ImportApi(registry);
        Health health = new Health(registry);

        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", port), 0);
        server.createContext("/health", HttpJson.wrap(health::handle));
        server.createContext("/mcp", HttpJson.wrap(ex -> JsonRpc.handle(ex, mcp.dispatcher())));
        server.createContext("/registry/", HttpJson.wrap(imports::handle));
        server.setExecutor(Executors.newFixedThreadPool(8));
        server.start();

        System.out.println("[mcp-registry] UP on http://127.0.0.1:" + port
                + " | tools=" + registry.size()
                + " | data=" + registry.path()
                + " | JDK=" + System.getProperty("java.version"));
        Runtime.getRuntime().addShutdownHook(new Thread(() -> server.stop(0)));
        Thread.currentThread().join();
    }

    private Main() {}
}
