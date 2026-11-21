package a2asdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// errors.go — maps every transport / JSON-RPC / context error into the typed
// *a2a.A2AError (contract §0.3 / §7), and maps UPSTREAM_TIMEOUT / deadlines
// into the Go-native net.Error timeout (§7: "超时映射成原生 timeout").
// ---------------------------------------------------------------------------

// httpError carries a non-2xx HTTP response so mapError can decode the
// unified §0.3 error envelope from its body.
type httpError struct {
	status int
	body   []byte
}

func (e *httpError) Error() string {
	return fmt.Sprintf("http %d: %s", e.status, strings.TrimSpace(string(e.body)))
}

// IsTimeout reports whether err is a native Go timeout. It checks
// context.DeadlineExceeded, net.Error.Timeout() and wrapped errors.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// TimeoutError wraps a timeout into a native net.Error so callers can do
//
//	var ne net.Error; errors.As(err, &ne) && ne.Timeout()
//
// while still exposing the mapped *A2AError via errors.As.
type TimeoutError struct {
	Err       error
	RequestID string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("UPSTREAM_TIMEOUT: %v (requestId=%s)", e.Err, e.RequestID)
}

// Timeout implements net.Error.
func (e *TimeoutError) Timeout() bool { return true }

// Temporary implements net.Error.
func (e *TimeoutError) Temporary() bool { return true }

// Unwrap lets errors.As find the native timeout cause.
func (e *TimeoutError) Unwrap() error { return e.Err }

var _ net.Error = (*TimeoutError)(nil)

// codeFromHTTP maps an HTTP status to the contract error code table (§0.3).
func codeFromHTTP(status int) shared.ErrorCode {
	switch status {
	case http.StatusBadRequest:
		return shared.CodeInvalidRequest
	case http.StatusUnauthorized:
		return shared.CodeAuthRequired
	case http.StatusForbidden:
		return shared.CodeForbidden
	case http.StatusNotFound:
		return shared.CodeAgentNotFound
	case http.StatusTooManyRequests:
		return shared.CodeRateLimited
	case http.StatusBadGateway:
		return shared.CodeUpstreamError
	case http.StatusGatewayTimeout:
		return shared.CodeUpstreamTimeout
	default:
		return shared.CodeInternalError
	}
}

// codeFromEnvelope extracts the unified error envelope from a JSON body.
func codeFromEnvelope(body []byte) (shared.ErrorCode, string, string) {
	var env shared.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
		return "", "", ""
	}
	return env.Error.Code, env.Error.Message, env.Error.RequestID
}

// mapError converts ANY error into *shared.A2AError. Timeouts additionally
// become native net.Error timeouts (wrapped in TimeoutError).
func mapError(err error, requestID string) error {
	if err == nil {
		return nil
	}
	// Already mapped?
	if ae, ok := shared.AsA2AError(err); ok {
		return ae
	}
	// Timeout first (contract §7).
	if IsTimeout(err) {
		return &TimeoutError{Err: err, RequestID: requestID}
	}
	// HTTP error with unified envelope.
	var he *httpError
	if errors.As(err, &he) {
		code, msg, rid := codeFromEnvelope(he.body)
		if code == "" {
			code = codeFromHTTP(he.status)
			msg = he.Error()
		}
		if rid == "" {
			rid = requestID
		}
		return &shared.A2AError{Code: code, Message: msg, RequestID: rid}
	}
	// JSON-RPC error reasons from the official SDK (a2a.ErrorReason).
	msg := err.Error()
	code := shared.CodeInternalError
	switch {
	case strings.Contains(msg, "task not found"), strings.Contains(msg, "not found"):
		code = shared.CodeTaskNotFound
	case strings.Contains(msg, "method not found"):
		code = shared.CodeInvalidRequest
	case strings.Contains(msg, "invalid params"), strings.Contains(msg, "invalid request"):
		code = shared.CodeInvalidRequest
	case strings.Contains(msg, "unauthenticated"):
		code = shared.CodeAuthRequired
	case strings.Contains(msg, "permission denied"):
		code = shared.CodeForbidden
	case strings.Contains(msg, "parse error"):
		code = shared.CodeInvalidRequest
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"):
		code = shared.CodeUpstreamError
	}
	return &shared.A2AError{Code: code, Message: msg, RequestID: requestID}
}
