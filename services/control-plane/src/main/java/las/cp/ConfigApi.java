package las.cp;

import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpExchange;

/**
 * GET /v1/config —— 数据面消费的全量配置快照（契约 §4 / §3.5 / E02 / E21）。
 *
 * <p>关键行为：
 * 1. 始终带响应头 ETag: <revision>（数据面轮询时用它做 If-None-Match）。
 * 2. 请求头 If-None-Match 等于当前 revision（支持弱/带引号/带 W/ 形式）→ 304，空 body。
 * 3. 任何写操作使 revision++ 后，这里返回值与 ETag 同步变化，数据面 5s 轮询热生效。
 */
public final class ConfigApi {

    private final Snapshot snapshot;

    public ConfigApi(Snapshot snapshot) {
        this.snapshot = snapshot;
    }

    public void handle(HttpExchange ex) throws Exception {
        if (!"GET".equalsIgnoreCase(ex.getRequestMethod())) {
            ex.getResponseHeaders().set("Allow", "GET");
            throw HttpJson.ApiException.invalid("GET /v1/config only");
        }
        long rev = snapshot.revision();
        ex.getResponseHeaders().set("ETag", String.valueOf(rev));
        ex.getResponseHeaders().set("Cache-Control", "no-cache");

        String inm = ex.getRequestHeaders().getFirst("If-None-Match");
        if (inm != null && matches(inm, rev)) {
            HttpJson.sendNoBody(ex, 304);   // 空 body，符合契约
            return;
        }
        ObjectNode body = snapshot.snapshot();
        // 保证 revision 与 ETag 完全一致（防止外部数据被改动）
        body.put("revision", rev);
        HttpJson.send(ex, 200, body);
    }

    /** If-None-Match 支持："12" / W/"12" / 12 / 逗号列表 / *。 */
    static boolean matches(String header, long rev) {
        String s = header.trim();
        if (s.equals("*")) return true;
        // 逗号分隔的多个 ETag
        for (String part : s.split(",")) {
            String p = part.trim();
            if (p.startsWith("W/")) p = p.substring(2).trim();
            if (p.length() >= 2 && p.startsWith("\"") && p.endsWith("\"")) {
                p = p.substring(1, p.length() - 1);
            }
            if (p.equals(String.valueOf(rev))) return true;
        }
        return false;
    }

    /** GET /v1/events（E21 人工核对用）。 */
    public static void events(HttpExchange ex, Events events) throws Exception {
        HttpJson.send(ex, 200, events.list());
    }
}
