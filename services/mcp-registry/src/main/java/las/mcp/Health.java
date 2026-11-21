package las.mcp;

import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpExchange;

/** GET /health（契约 §0.4）。 */
public final class Health {

    private final Registry registry;

    public Health(Registry registry) {
        this.registry = registry;
    }

    public void handle(HttpExchange ex) throws Exception {
        ObjectNode r = HttpJson.MAPPER.createObjectNode();
        r.put("status", "UP");
        r.put("service", "mcp-registry");
        r.put("version", "1.0.0");
        r.put("tools", registry.size());
        r.put("now", java.time.OffsetDateTime.now().toString());
        HttpJson.send(ex, 200, r);
    }
}
