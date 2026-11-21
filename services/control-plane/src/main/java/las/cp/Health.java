package las.cp;

import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpExchange;

/** GET /health（契约 §0.4）。 */
public final class Health {

    private final Snapshot snapshot;

    public Health(Snapshot snapshot) {
        this.snapshot = snapshot;
    }

    public void handle(HttpExchange ex) throws Exception {
        ObjectNode r = HttpJson.MAPPER.createObjectNode();
        r.put("status", "UP");
        r.put("service", "control-plane");
        r.put("version", "1.0.0");
        r.put("revision", snapshot.revision());
        r.put("now", Snapshot.now());
        HttpJson.send(ex, 200, r);
    }
}
