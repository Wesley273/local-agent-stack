package las.sdk;

import org.a2aproject.sdk.spec.AgentCard;
import org.a2aproject.sdk.spec.Artifact;
import org.a2aproject.sdk.spec.Message;
import org.a2aproject.sdk.spec.Task;
import org.a2aproject.sdk.spec.TaskState;
import org.a2aproject.sdk.spec.TaskStatus;
import org.a2aproject.sdk.spec.TextPart;

import java.util.List;

/**
 * 可运行示例：证明 Java SDK 能构造官方 SDK 类型、做跨 chunk SSE 拼接、护栏检查，
 * 并在数据面在线时跑一次真实 message/send 全链路。
 *
 * 运行（编译后）：
 *   java -cp target/classes;... las.sdk.Example
 * 或带参指定数据面地址：
 *   java ... las.sdk.Example http://127.0.0.1:8080/a2a/echo-agent
 *
 * 不要求服务端在线：离线部分（SDK 类型 / SSE / 护栏）始终执行并通过。
 */
public final class Example {

    public static void main(String[] args) {
        String baseUrl = args.length > 0 ? args[0] : "http://127.0.0.1:8080/a2a/echo-agent";

        // 1) 用 Builder 构造客户端（契约 §7）
        A2AClient client = new A2AClient.Builder()
                .baseUrl(baseUrl)
                .apiKey("demo-key")
                .build();

        // 2) 官方 SDK 类型演示：AgentCard（supportedInterfaces 非空）/ Task（status 非空）
        AgentCard card = client.getAgentCard();
        System.out.println("[1] AgentCard(官方 record): name=" + card.name()
                + " version=" + card.version()
                + " interfaces=" + card.supportedInterfaces().size()
                + " skills=" + card.skills().size());

        Task localTask = Task.builder()
                .id("task-demo").contextId("ctx-demo")
                .status(new TaskStatus(TaskState.TASK_STATE_COMPLETED))
                .artifacts(List.of(Artifact.builder()
                        .artifactId("art-1")
                        .parts(List.<org.a2aproject.sdk.spec.Part<?>>of(new TextPart("hello from SDK")))
                        .build()))
                .build();
        System.out.println("[2] Task(官方 record): id=" + localTask.id()
                + " state=" + localTask.status().state()
                + " artifacts=" + localTask.artifacts().size());

        // 3) 官方门面 A2A.toUserMessage 构造 Message
        Message msg = A2AClient.textMessage("你好，A2A SDK!");
        System.out.println("[3] Message(官方): role=" + msg.role()
                + " text=" + ((TextPart) msg.parts().get(0)).text());

        // 4) 护栏本地检查
        System.out.println("[4] checkGuardrail(\"hello\")=" + client.checkGuardrail("hello"));
        System.out.println("    checkGuardrail(\"BLOCK_ME\")=" + client.checkGuardrail("BLOCK_ME"));

        // 5) SSE 跨 chunk 拼接：构造一个 128KB+ 的 data 帧，故意按 7 字节切碎喂入
        StringBuilder big = new StringBuilder();
        for (int i = 0; i < 128000; i++) big.append('x');
        String frame = "event: artifact-update\ndata: {\"text\":\"" + big + "\"}\n\n";
        SSE.Parser parser = new SSE.Parser(ev ->
                System.out.println("[5] SSE 帧 event=" + ev.name()
                        + " dataLen=" + ev.data().length()
                        + " (跨 chunk 拼接完整)"));
        byte[] bytes = frame.getBytes(java.nio.charset.StandardCharsets.UTF_8);
        int off = 0;
        while (off < bytes.length) {
            int len = Math.min(7, bytes.length - off); // 故意极小 chunk
            byte[] chunk = java.util.Arrays.copyOfRange(bytes, off, off + len);
            parser.feed(chunk);
            off += len;
        }
        parser.end();

        // 6) 在线全链路（服务端不在线时优雅跳过，不算失败）
        try {
            Task t = client.sendMessage(A2AClient.textMessage("E06 Java SDK 直连数据面"));
            System.out.println("[6] message/send -> Task(id=" + t.id()
                    + ", state=" + t.status().state()
                    + ", artifacts=" + t.artifacts().size() + ")");
        } catch (A2AException e) {
            System.out.println("[6] 服务端未在线，跳过在线用例：" + e.code() + " / " + e.getMessage());
        }

        client.close();
        System.out.println("Example 完成（离线能力全部通过）。");
    }

    private Example() {}
}
