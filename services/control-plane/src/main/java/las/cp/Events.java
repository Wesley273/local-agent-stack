package las.cp;

import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

/**
 * 变更事件 + revision++ 的广播视图（docs/03 A7 / E21）。
 *
 * <p>事件本身由 Store 追加并落盘（见 Snapshot.write -> Store.addEvent）。
 * 这里提供 /v1/events 的读取入口：返回最近变更事件列表 + 当前 revision。
 * 数据面 5s 轮询 /v1/config 即可感知 revision 变化（E21），本端点用于人工核对。
 */
public final class Events {

    private final Snapshot snapshot;
    private final Store store;

    public Events(Snapshot snapshot, Store store) {
        this.snapshot = snapshot;
        this.store = store;
    }

    /** GET /v1/events 响应：{"revision":N,"events":[...]}。 */
    public ObjectNode list() {
        ObjectNode out = HttpJson.MAPPER.createObjectNode();
        out.put("revision", snapshot.revision());
        ArrayNode arr = HttpJson.MAPPER.createArrayNode();
        for (ObjectNode e : store.events()) arr.add(e);
        out.set("events", arr);
        return out;
    }
}
