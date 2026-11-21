package las.sdk;

import java.io.ByteArrayInputStream;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.function.Consumer;

/**
 * SSE 跨 chunk 拼接解析器（契约 §7 硬要求）。
 *
 * 关键点：一个 data: 帧可能被 TCP 拆成多次读，禁止按单次 read 假设完整帧。
 * 这里维护一个可增长的缓冲区，只有遇到空行（帧结束）才派发一个完整事件；
 * 未闭合的尾部会保留到下一批字节继续拼接。同时支持 NDJSON 风格的超长单行
 * （128KB+ 不截断）。
 *
 * 用法：
 *   SSE.Parser p = new SSE.Parser(ev -> handle(ev));
 *   p.feed(chunkBytes);      // 可多次、任意切分
 *   p.feed(chunkBytes2);
 *   p.end();                  // 流结束，冲刷残余
 */
public final class SSE {

    public record Event(String name, String data) {}

    public static final class Parser {
        private final Consumer<Event> sink;
        private final StringBuilder lineBuf = new StringBuilder();
        private String eventName = "";
        private final List<String> dataLines = new ArrayList<>();

        public Parser(Consumer<Event> sink) { this.sink = sink; }

        /** 喂入任意切分的字节块；内部按 '\n' 切行并保留跨 chunk 残片。 */
        public void feed(byte[] chunk) {
            if (chunk == null || chunk.length == 0) return;
            feed(new String(chunk, StandardCharsets.UTF_8));
        }

        public void feed(String s) {
            for (int i = 0; i < s.length(); i++) {
                char c = s.charAt(i);
                if (c == '\n') {
                    onLine(lineBuf.toString());
                    lineBuf.setLength(0);
                } else if (c != '\r') {
                    lineBuf.append(c);
                }
            }
            // 不处理 lineBuf 残留：等下一批或 end() 再闭合
        }

        private void onLine(String line) {
            if (line.isEmpty()) {
                // 空行 = 帧结束
                if (!dataLines.isEmpty() || !eventName.isEmpty()) {
                    String data = String.join("\n", dataLines);
                    sink.accept(new Event(eventName, data));
                    eventName = "";
                    dataLines.clear();
                }
                return;
            }
            if (line.startsWith(":")) return; // 注释/心跳
            int colon = line.indexOf(':');
            String field = colon < 0 ? line : line.substring(0, colon);
            String value = colon < 0 ? "" : line.substring(colon + 1);
            if (value.startsWith(" ")) value = value.substring(1);
            switch (field) {
                case "event" -> eventName = value;
                case "data" -> dataLines.add(value);
                default -> { /* id / retry 忽略 */ }
            }
        }

        /** 流结束：若还有未以空行闭合的帧，按最后一行补发（尽力）。 */
        public void end() {
            if (lineBuf.length() > 0) onLine(lineBuf.toString());
            lineBuf.setLength(0);
            onLine(""); // 强制闭合
        }
    }

    /** 便捷方法：把一个 SSE 字节流（可来自 HttpClient 的 InputStream）按帧解析。 */
    public static void parse(InputStream in, Consumer<Event> sink) throws Exception {
        Parser p = new Parser(sink);
        byte[] buf = new byte[64 * 1024];
        int n;
        while ((n = in.read(buf)) != -1) {
            p.feed(java.util.Arrays.copyOf(buf, n));
        }
        p.end();
    }

    /** 从完整字符串解析（测试用）。 */
    public static List<Event> parseAll(String body) {
        List<Event> out = new ArrayList<>();
        Parser p = new Parser(out::add);
        p.feed(body);
        p.end();
        return out;
    }

    /** 把一段文本转成 InputStream，便于复用 parse。 */
    public static InputStream asStream(String s) {
        return new ByteArrayInputStream(s.getBytes(StandardCharsets.UTF_8));
    }
}
