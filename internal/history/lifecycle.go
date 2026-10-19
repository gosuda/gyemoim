package history

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Candidate identifies a considered target, independently of transmission numbers.
// Numbers are positive and increasing across both started and skipped candidates.
type Candidate struct {
	Number   int
	Provider ProviderSnapshot
}

// AttemptResult is the terminal result of a started candidate, including failures
// before transmission. Reason is a non-secret machine code; SafeError must already
// be sanitized. Usage is diagnostic only, never added to request-level aggregates.
type AttemptResult struct {
	Outcome           string
	Reason            string
	HTTPStatus        int
	UpstreamRequestID string
	SafeError         string
	Usage             *Usage
	Timings           Timings
}

type candidateState struct {
	Candidate
	transmission int
}

type candidateLine struct {
	baseLine
	Candidate int              `json:"candidate"`
	Provider  ProviderSnapshot `json:"provider"`
}

type attemptEndLine struct {
	candidateLine
	Attempt           int     `json:"attempt"`
	Outcome           string  `json:"outcome"`
	Reason            string  `json:"reason"`
	HTTPStatus        int     `json:"http_status"`
	UpstreamRequestID string  `json:"upstream_request_id,omitempty"`
	SafeError         string  `json:"safe_error"`
	Usage             *Usage  `json:"usage"`
	Timings           Timings `json:"timings"`
}

type candidateSkipLine struct {
	candidateLine
	Reason string `json:"reason"`
}

func validCandidate(c Candidate) bool {
	return c.Number > 0 && validProvider(c.Provider)
}

func validProvider(p ProviderSnapshot) bool {
	// Lifecycle metadata is intentionally small, even for caller-supplied targets.
	for _, s := range []string{p.ID, p.Name, p.UpstreamModel} {
		if strings.TrimSpace(s) == "" || len(s) > 1024 || !utf8.ValidString(s) {
			return false
		}
	}
	return true
}

func validReason(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func validAttemptResult(result AttemptResult, transmission int) bool {
	if !validOutcome(result.Outcome) || !validReason(result.Reason) || result.HTTPStatus < 0 || result.HTTPStatus > 599 ||
		result.HTTPStatus > 0 && result.HTTPStatus < 100 || !validUpstreamRequestID(result.UpstreamRequestID) ||
		len(result.SafeError) > maxSafeError || !utf8.ValidString(result.SafeError) || !validUsage(result.Usage) || !validTimings(result.Timings) {
		return false
	}
	if transmission > 0 {
		return true
	}
	// Only authentication/preparation can have been observed without a send fence.
	t := result.Timings
	return result.Outcome != "completed" && result.HTTPStatus == 0 && result.UpstreamRequestID == "" && result.Usage == nil &&
		t.ConnectionRequestedNS == nil && t.ConnectionEstablishedNS == nil && t.ConnectionReused == nil &&
		t.RequestTransmissionNS == nil && t.FirstEventNS == nil && t.FirstOutputNS == nil &&
		t.StreamCompletionNS == nil && t.DownstreamDeliveryNS == nil
}

func (h *Request) candidateLine(c Candidate, kind string) candidateLine {
	now := time.Now()
	return candidateLine{baseLine: baseLine{SchemaVersion: schemaVersion, Type: kind, RequestID: h.id,
		StartedAt: h.startedAt, TimestampUTC: now.UTC(), ElapsedNS: h.elapsedNS(now)}, Candidate: c.Number, Provider: c.Provider}
}

// StartAttempt durably records selection before authentication/preparation begins.
// Explicit lifecycle and legacy bare Transmit calls cannot be mixed on a request.
func (h *Request) StartAttempt(c Candidate) error {
	return h.recordCandidate(c, "")
}

// SkipCandidate durably records a target not attempted (for example, cooldown).
// A failure must stop candidate advancement; no provider work may bypass the fence.
func (h *Request) SkipCandidate(c Candidate, reason string) error {
	if !validReason(reason) {
		return fmt.Errorf("%w: invalid skip reason", ErrInvalidRecord)
	}
	return h.recordCandidate(c, reason)
}

func (h *Request) recordCandidate(c Candidate, skipReason string) error {
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
	}
	if !validCandidate(c) {
		return fmt.Errorf("%w: invalid candidate", ErrInvalidRecord)
	}
	r := h.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ended {
		return ErrRequestEnded
	}
	if r.active[h.id] != h {
		return ErrRecordingUnavailable
	}
	if h.openCandidate != nil || c.Number <= h.lastCandidate || !h.lifecycle && h.transmissions > 0 {
		return fmt.Errorf("%w: candidate must increase and previous attempt must end", ErrInvalidRecord)
	}
	var line any = h.candidateLine(c, "attempt_start")
	if skipReason != "" {
		line = candidateSkipLine{candidateLine: h.candidateLine(c, "candidate_skip"), Reason: skipReason}
	}
	if err := r.writeLocked(line, true); err != nil {
		return err
	}
	h.lifecycle, h.lastCandidate = true, c.Number
	if skipReason == "" {
		h.openCandidate = &candidateState{Candidate: c}
	}
	return nil
}

// EndAttempt durably closes the matching candidate. Attempt is inferred from its
// successful Transmit fence, or zero when no transmission was authorized. Finish
// response/event recording before this call; late records are rejected.
func (h *Request) EndAttempt(candidate int, result AttemptResult) error {
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
	}
	h.recorder.mu.Lock()
	defer h.recorder.mu.Unlock()
	if h.ended {
		return ErrRequestEnded
	}
	return h.endAttemptLocked(candidate, result)
}

func (h *Request) endAttemptLocked(candidate int, result AttemptResult) error {
	c := h.openCandidate
	if c == nil || candidate != c.Number || !validAttemptResult(result, c.transmission) {
		return fmt.Errorf("%w: invalid attempt result or candidate", ErrInvalidRecord)
	}
	if result.Usage != nil {
		result.Usage = cloneUsage(result.Usage)
	}
	line := attemptEndLine{candidateLine: h.candidateLine(c.Candidate, "attempt_end"), Attempt: c.transmission,
		Outcome: result.Outcome, Reason: result.Reason, HTTPStatus: result.HTTPStatus, UpstreamRequestID: result.UpstreamRequestID,
		SafeError: result.SafeError, Usage: result.Usage, Timings: cloneTimings(result.Timings)}
	if err := h.recorder.writeLocked(line, true); err != nil {
		return err
	}
	h.openCandidate = nil
	return nil
}
