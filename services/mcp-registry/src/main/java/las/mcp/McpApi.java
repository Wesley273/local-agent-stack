package las.mcp;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

/**
 * POST /mcp 的 JSON-RPC 2.0 方法实现（契约 §5.1）。
 *
 * <table>
 *   <tr><td>initialize</td><td>protocolVersion 2024-11-05 + capabilities + serverInfo</td></tr>
 *   <tr><td>tools/list</td><td>{"tools":[Tool...],"nextCursor":""}</td></tr>
 *   <tr><td>tools/call</td><td>真实 HTTP 回放，见 Replay</td></tr>
 *   <tr><td>tools/get</td><td>单个 Tool</td></tr>
 *   <tr><td>ping</td><td>{}</td></tr>
 * </table>
 *
 * <p>错误码 -32700/-32600/-32601/-32602/-32603 由 JsonRpc.Dispatcher 处理；
 * 这里只抛 JsonRpcException / HttpJson.ApiException 表达业务错误。
 */
public final class McpApi {

    private final Registry registry;

    public McpApi(Registry registry) {
        this.registry = registry;
    }

    /** 把方法注册进 Dispatcher。 */
    public JsonRpc.Dispatcher dispatcher() {
        return new JsonRpc.Dispatcher()
                .register("initialize", this::initialize)
                .register("tools/list", this::toolsList)
                .register("tools/call", this::toolsCall)
                .register("tools/get", this::toolsGet)
                .register("ping", p -> HttpJson.MAPPER.createObjectNode());
    }

    // ------------------------------------------------------------------ 方法

    public JsonNode initialize(JsonNode params) {
        ObjectNode result = HttpJson.MAPPER.createObjectNode();
        result.put("protocolVersion", "2024-11-05");
        ObjectNode caps = HttpJson.MAPPER.createObjectNode();
        ObjectNode toolsCap = HttpJson.MAPPER.createObjectNode();
        toolsCap.put("listChanged", false);
        caps.set("tools", toolsCap);
        result.set("capabilities", caps);
        ObjectNode serverInfo = HttpJson.MAPPER.createObjectNode();
        serverInfo.put("name", "mcp-registry");
        serverInfo.put("version", "1.0.0");
        result.set("serverInfo", serverInfo);
        return result;
    }

    public JsonNode toolsList(JsonNode params) {
        ObjectNode result = HttpJson.MAPPER.createObjectNode();
        // 支持 cursor / q 检索（契约 cursor 可选）
        String q = params.path("q").asText("");
        result.set("tools", q.isEmpty() ? registry.list() : registry.search(q));
        result.put("nextCursor", "");
        return result;
    }

    /** tools/call：参数 name + arguments -> Replay 真实 HTTP 回放。 */
    public JsonNode toolsCall(JsonNode params) {
        String name = params.path("name").asText("");
        if (name.isEmpty()) {
            throw JsonRpc.JsonRpcException.invalidParams("tools/call requires params.name");
        }
        ObjectNode tool = registry.get(name);   // 不存在 -> MCP_TOOL_NOT_FOUND(-32602 data.code)
        JsonNode arguments = params.has("arguments") ? params.get("arguments")
                : HttpJson.MAPPER.createObjectNode();
        return Replay.call(tool, arguments);
    }

    public JsonNode toolsGet(JsonNode params) {
        String name = params.path("name").asText("");
        if (name.isEmpty()) {
            throw JsonRpc.JsonRpcException.invalidParams("tools/get requires params.name");
        }
        return registry.get(name);
    }

    /** 便于测试：是否支持某方法。 */
    public static boolean supports(String method) {
        return method.equals("initialize") || method.equals("tools/list")
                || method.equals("tools/call") || method.equals("tools/get")
                || method.equals("ping");
    }
}
