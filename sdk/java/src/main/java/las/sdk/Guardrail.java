package las.sdk;

import java.util.List;

/**
 * 本地护栏检查（契约 §7 checkGuardrail）。
 * 与数据面/Sidecar 的 blockedWords/blockedTools 语义对齐，SDK 侧先做一次快速本地校验，
 * 避免明显违规的请求打到网关。命中返回一个可被 A2AClient 转成 A2AException 的结果。
 */
public final class Guardrail {

    /** 默认拦截词，与 config/ports.env 对应的 guardrails.blockedWords 一致。 */
    public static final List<String> DEFAULT_BLOCKED_WORDS = List.of("BLOCK_ME", "secret-leak");
    public static final List<String> DEFAULT_BLOCKED_TOOLS = List.of("dangerous_delete", "drop_database");

    private final List<String> blockedWords;
    private final List<String> blockedTools;
    private final int maxPromptChars;

    public Guardrail() { this(DEFAULT_BLOCKED_WORDS, DEFAULT_BLOCKED_TOOLS, 8000); }

    public Guardrail(List<String> blockedWords, List<String> blockedTools, int maxPromptChars) {
        this.blockedWords = blockedWords;
        this.blockedTools = blockedTools;
        this.maxPromptChars = maxPromptChars;
    }

    /** 检查结果：allowed=false 时带 code 与 message。 */
    public record Result(boolean allowed, String code, String message) {}

    public Result check(String text) {
        if (text == null) return new Result(true, "", "");
        if (text.length() > maxPromptChars) {
            return new Result(false, "GUARDRAIL_REJECTED",
                    "prompt exceeds maxPromptChars=" + maxPromptChars);
        }
        for (String w : blockedWords) {
            if (text.contains(w)) {
                return new Result(false, "GUARDRAIL_REJECTED", "blocked word: " + w);
            }
        }
        return new Result(true, "", "");
    }

    public Result checkTool(String toolName) {
        if (toolName == null) return new Result(true, "", "");
        for (String t : blockedTools) {
            if (t.equals(toolName)) {
                return new Result(false, "GUARDRAIL_REJECTED", "blocked tool: " + toolName);
            }
        }
        return new Result(true, "", "");
    }

    /** 契约 §7 的同名能力：命中返回 false。 */
    public boolean checkGuardrail(String text) {
        return check(text).allowed();
    }
}
