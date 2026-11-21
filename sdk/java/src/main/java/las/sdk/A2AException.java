package las.sdk;

/**
 * Java SDK 统一错误类型（契约 §7）。
 * 对应官方 SDK 的 org.a2aproject.sdk.spec.A2AClientError 与契约 §0.3 错误信封，
 * 这里做企业级 SDK 的轻量封装，保留 code / message / requestId / httpStatus。
 */
public class A2AException extends RuntimeException {

    private final String code;
    private final String requestId;
    private final int httpStatus;

    public A2AException(String code, String message, String requestId, int httpStatus) {
        super(message);
        this.code = code;
        this.requestId = requestId;
        this.httpStatus = httpStatus;
    }

    public A2AException(String code, String message, String requestId, int httpStatus, Throwable cause) {
        super(message, cause);
        this.code = code;
        this.requestId = requestId;
        this.httpStatus = httpStatus;
    }

    public String code() { return code; }
    public String requestId() { return requestId; }
    public int httpStatus() { return httpStatus; }

    /** UPSTREAM_TIMEOUT 需映射为语言原生 timeout 语义；调用方可用此方法判定。 */
    public boolean isTimeout() { return "UPSTREAM_TIMEOUT".equals(code); }

    @Override
    public String toString() {
        return "A2AException{" + "code='" + code + '\'' + ", message='" + getMessage() + '\''
                + ", requestId='" + requestId + '\'' + ", httpStatus=" + httpStatus + '}';
    }
}
