// Package apierr defines the shared error model used by the bridge, the jq
// domain layer and the MCP server. Every error carries a stable machine code so
// that AI clients can react programmatically.
package apierr

import "fmt"

// Stable error codes shared across the bridge protocol and the MCP tools.
const (
	CodeNoExtension      = "no_extension"
	CodeNotAuthenticated = "not_authenticated"
	CodeNotFound         = "not_found"
	CodeUsage            = "usage_error"
	CodeAPI              = "api_error"
	CodeNetwork          = "network_error"
	CodeTimeout          = "timeout"
	CodeInternal         = "internal_error"
)

// Error is a coded error that can be serialized into a bridge RPC error or an
// MCP tool error payload.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// WithDetail returns a copy of the error with one extra detail entry. It keeps
// the original untouched so a shared error value is never mutated.
func (e *Error) WithDetail(key string, value any) *Error {
	clone := &Error{Code: e.Code, Message: e.Message}
	if len(e.Details) > 0 {
		clone.Details = make(map[string]any, len(e.Details)+1)
		for k, v := range e.Details {
			clone.Details[k] = v
		}
	} else {
		clone.Details = make(map[string]any, 1)
	}
	clone.Details[key] = value
	return clone
}

// New builds a coded error using fmt-style formatting.
func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap converts any error into a coded error. Errors that already are *Error
// pass through unchanged.
func Wrap(code string, err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Code: code, Message: err.Error()}
}

// From maps an arbitrary error to a coded error, defaulting to internal_error.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

// Convenience constructors for the most common cases.

func NoExtension() *Error {
	return New(CodeNoExtension, "没有可用的浏览器插件连接；请在扩展中配置 token，并确保 jqhelper 已连接")
}

func NotAuthenticated() *Error {
	return New(CodeNotAuthenticated, "聚宽未登录或登录已过期；请先手动登录")
}

func Timeout(format string, args ...any) *Error {
	return New(CodeTimeout, format, args...)
}

func Usage(format string, args ...any) *Error {
	return New(CodeUsage, format, args...)
}
