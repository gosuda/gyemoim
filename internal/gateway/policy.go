package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FailureKind is an executor classification, never an arbitrary error string.
// In particular, a context error or recording failure is NOT a transport failure.
type FailureKind string

const (
	FailureHTTP      FailureKind = "http"
	FailureTransport FailureKind = "transport"
	// FailureProtocol requires adapter-confirmed malformed/unexpected upstream
	// content or premature stream termination, not an unclassified model error.
	FailureProtocol      FailureKind = "protocol"
	FailureAuth          FailureKind = "auth"
	FailureInitialStream FailureKind = "initial_stream"
	FailureDisconnected  FailureKind = "disconnected"
	FailureRequest       FailureKind = "request"
	FailureCapability    FailureKind = "capability"
	FailureCanceled      FailureKind = "canceled"
	FailureRecording     FailureKind = "recording"
)

// Failure contains only explicit, safe facts. RetryAfter is used only to parse
// HTTP 429 delays and is never retained or exposed verbatim.
type Failure struct {
	Kind              FailureKind
	HTTPStatus        int
	RetryAfter        string
	ResponseCommitted bool
	ClientCanceled    bool
	RecordingFailed   bool
}

type FailurePolicy struct {
	Retryable   bool          `json:"retryable"`
	Reason      string        `json:"reason"`
	HTTPStatus  int           `json:"httpStatus,omitempty"`
	Cooldown    time.Duration `json:"-"`
	DelaySource string        `json:"delaySource,omitempty"`
}

// ClassifyFailure is deliberately an allowlist. Initial stream error events
// require a known upstream HTTP status; unclassified model/capability errors stop.
// Adapter-confirmed response-format failures or premature stream termination use
// FailureProtocol; known transport read/dial failures use FailureTransport.
// Commitment/cancellation/recording gates disable continuation, not observation
// of an otherwise real upstream failure (and its cooldown).
func ClassifyFailure(f Failure, now time.Time) FailurePolicy {
	p := FailurePolicy{Reason: "unclassified"}
	switch f.Kind {
	case FailureTransport, FailureProtocol, FailureAuth:
		p.Reason, p.Retryable, p.Cooldown, p.DelaySource = string(f.Kind), true, 30*time.Second, "default_30s"
	case FailureHTTP, FailureInitialStream:
		if f.HTTPStatus >= 100 && f.HTTPStatus <= 599 {
			p.HTTPStatus = f.HTTPStatus
		}
		switch {
		case f.HTTPStatus == http.StatusTooManyRequests:
			p.Reason, p.Retryable = "rate_limited", true
			p.Cooldown, p.DelaySource = retryAfterDelay(f.RetryAfter, now)
		case f.HTTPStatus == http.StatusUnauthorized || f.HTTPStatus == http.StatusForbidden:
			p.Reason, p.Retryable, p.Cooldown, p.DelaySource = "auth", true, 30*time.Second, "default_30s"
		case f.HTTPStatus == http.StatusRequestTimeout || f.HTTPStatus >= 500 && f.HTTPStatus <= 599:
			p.Reason, p.Retryable, p.Cooldown, p.DelaySource = "upstream_unavailable", true, 30*time.Second, "default_30s"
		default:
			p.Reason = "upstream_rejected"
			if f.Kind == FailureInitialStream && f.HTTPStatus < 400 {
				p.Reason = "initial_stream"
			}
		}
	case FailureDisconnected:
		// Connection eligibility comes only from the configuration snapshot.
		p.Reason, p.Retryable = "disconnected", true
	case FailureRequest, FailureCapability, FailureCanceled, FailureRecording:
		p.Reason = string(f.Kind)
	}
	if f.ResponseCommitted || f.ClientCanceled || f.RecordingFailed {
		p.Retryable = false
	}
	return p
}

func retryAfterDelay(value string, now time.Time) (time.Duration, string) {
	value = strings.TrimSpace(value)
	// Parse unsigned decimal seconds, with explicit overflow protection.
	if value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err == nil && seconds <= uint64((1<<63-1)/int64(time.Second)) {
			return time.Duration(seconds) * time.Second, "retry_after_seconds"
		}
	}
	if date, err := http.ParseTime(value); err == nil {
		delay := date.Sub(now)
		if delay < 0 {
			delay = 0
		}
		return delay, "retry_after_date"
	}
	return 60 * time.Second, "default_60s"
}
