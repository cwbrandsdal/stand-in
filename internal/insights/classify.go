package insights

import (
	"strings"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Error classes stored in requests.error_class. The dashboard names them; the hub never stores the
// upstream error body itself.
const (
	errorClassFastMode        = "fast_mode"
	errorClassRateLimit       = "rate_limit"
	errorClassOverloaded      = "overloaded"
	errorClassUpstreamAuth    = "upstream_auth"
	errorClassContextLength   = "context_length"
	errorClassModelNotFound   = "model_not_found"
	errorClassBadRequest      = "bad_request"
	errorClassClientCancelled = "client_cancelled"
	errorClassStreamTruncated = "stream_truncated"
	errorClassUpstreamError   = "upstream_error"
	errorClassOther           = "other"
)

// classifyFailure names why a request failed from its status, failure code and upstream error body.
func classifyFailure(statusCode int, failCode, body string) string {
	lower := strings.ToLower(body)
	contains := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(lower, part) {
				return true
			}
		}
		return false
	}
	switch {
	case failCode == coreusage.FailureStreamTruncatedCode:
		return errorClassStreamTruncated
	// Anthropic refuses fast mode without usage credits with a 429 that is not a rate limit.
	case contains("fast request rejected") || (strings.Contains(lower, "fast") && contains("usage credits", "credits are required")):
		return errorClassFastMode
	case statusCode == 499:
		return errorClassClientCancelled
	case statusCode == 529 || contains("overloaded_error", "overloaded"):
		return errorClassOverloaded
	case statusCode == 429 || contains("rate_limit_error", "usage_limit_reached", "rate limit"):
		return errorClassRateLimit
	case statusCode == 401 || statusCode == 403 || contains("token_invalidated", "authentication_error", "invalid_grant", "token has expired", "refresh token"):
		return errorClassUpstreamAuth
	case contains("prompt is too long", "context_length_exceeded", "maximum context length", "context window"):
		return errorClassContextLength
	case statusCode == 404 || contains("model_not_found", "unknown provider for model", "not_found_error"):
		return errorClassModelNotFound
	case statusCode >= 500:
		return errorClassUpstreamError
	case statusCode >= 400:
		return errorClassBadRequest
	}
	return errorClassOther
}
