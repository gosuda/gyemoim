// Package history writes versioned request history to owner-only NDJSON files.
// Rotation and crash recovery are intentionally left to the next storage task.
package history

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	schemaVersion  = 1
	maxEventBytes  = 64 << 20
	maxRequestID   = 128
	maxSafeError   = 8 << 10
	activeFileName = "active.ndjson"
)

var (
	// ErrRecordingUnavailable is returned when the history sink cannot accept a record.
	ErrRecordingUnavailable = errors.New("request recording is unavailable")
	// ErrEventTooLarge rejects one oversized SSE frame without degrading the recorder.
	ErrEventTooLarge = errors.New("response event exceeds the 64 MiB recording limit")
	// ErrInvalidRecord reports caller data that cannot be represented by this schema.
	ErrInvalidRecord = errors.New("invalid request history record")
	// ErrRequestEnded rejects operations after a request has been finalized.
	ErrRequestEnded = errors.New("request history handle has ended")
)

// ServiceAccountSnapshot is the non-secret admission-time identity written to history.
type ServiceAccountSnapshot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ModelSnapshot is the admission-time named model and routing configuration version.
type ModelSnapshot struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  int    `json:"version"`
	Strategy string `json:"strategy"`
}

// Start contains the non-secret snapshots needed to attribute an admitted request.
// IncomingRequest must be the complete request JSON after removing managed HTTP
// authentication fields; arbitrary user-provided request content is preserved.
type Start struct {
	RequestID       string
	ServiceAccount  ServiceAccountSnapshot
	Model           ModelSnapshot
	IncomingRequest json.RawMessage
}

// ProviderSnapshot identifies the actual provider target used for one upstream attempt.
type ProviderSnapshot struct {
	ID            string
	Name          string
	UpstreamModel string
}

// Attempt is the target and effective request body for one upstream transmission.
type Attempt struct {
	Number           int
	Provider         ProviderSnapshot
	EffectiveRequest json.RawMessage
}

// Usage stores provider-reported token counts. A nil count means unknown; a pointer
// to zero means the provider reported zero. Cached/reasoning counts are subsets.
type Usage struct {
	InputTokens           *int64 `json:"input_tokens"`
	OutputTokens          *int64 `json:"output_tokens"`
	CachedInputTokens     *int64 `json:"cached_input_tokens"`
	ReasoningOutputTokens *int64 `json:"reasoning_output_tokens"`
}

// Timings contains monotonic nanosecond offsets from request admission. Nil fields
// mean that a stage was not observed, rather than that it took zero time.
type Timings struct {
	AuthenticationPreparationNS *int64 `json:"authentication_preparation_ns"`
	ConnectionEstablishedNS     *int64 `json:"connection_established_ns"`
	RequestTransmissionNS       *int64 `json:"request_transmission_ns"`
	FirstEventNS                *int64 `json:"first_event_ns"`
	FirstOutputNS               *int64 `json:"first_output_ns"`
	StreamCompletionNS          *int64 `json:"stream_completion_ns"`
	DownstreamDeliveryNS        *int64 `json:"downstream_delivery_ns"`
}

// RecorderStatus contains only safe operational counters and state.
type RecorderStatus struct {
	State                  string `json:"state"`
	PotentiallyLostRecords uint64 `json:"potentiallyLostRecords"`
	ActiveRequests         int    `json:"activeRequests"`
	BytesWritten           int64  `json:"bytesWritten"`
}

// Recorder serializes every record and durability fence under one mutex so records
// from concurrent requests cannot interleave. A storage error permanently degrades
// this process's recorder; recovery and active-file rotation belong to T06.
type Recorder struct {
	mu                     sync.Mutex
	file                   *os.File
	active                 map[string]*Request
	state                  string
	failure                error
	closed                 bool
	potentiallyLostRecords uint64
	bytesWritten           int64
	unsyncedRecords        uint64
	closeErr               error
}

// Request is a handle for one admitted inference request. It is safe for concurrent
// calls; End is idempotent and unregisters the request even if recording has failed.
type Request struct {
	recorder       *Recorder
	id             string
	startedAt      time.Time
	monotonicStart time.Time
	ended          bool
	lastAttempt    int
	eventSequence  uint64
}

// Open prepares dataDir/history/active.ndjson. On filesystem failure it returns a
// degraded recorder and a diagnostic error; callers should keep the UI running and
// pass the recorder to the UI and gateway admission path. No existing file is replaced.
func Open(dataDir string) (*Recorder, error) {
	r := &Recorder{
		active: make(map[string]*Request),
		state:  "degraded",
	}
	if strings.TrimSpace(dataDir) == "" {
		return r, r.degrade(errors.New("history data directory is empty"))
	}
	dir := filepath.Join(dataDir, "history")
	_, dirStatErr := os.Stat(dir)
	historyDirCreated := errors.Is(dirStatErr, os.ErrNotExist)
	if dirStatErr != nil && !historyDirCreated {
		return r, r.degrade(fmt.Errorf("inspect history directory: %w", dirStatErr))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return r, r.degrade(fmt.Errorf("create history directory: %w", err))
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return r, r.degrade(fmt.Errorf("set history directory permissions: %w", err))
	}
	if historyDirCreated {
		if err := syncDirectory(dataDir); err != nil {
			return r, r.degrade(fmt.Errorf("sync parent of history directory: %w", err))
		}
	}
	path := filepath.Join(dir, activeFileName)
	_, statErr := os.Stat(path)
	createdNewFile := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !createdNewFile {
		return r, r.degrade(fmt.Errorf("inspect history file: %w", statErr))
	}
	if err := validateAppendFile(path); err != nil {
		return r, r.degrade(fmt.Errorf("existing history file is not a clean NDJSON file: %w", err))
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return r, r.degrade(fmt.Errorf("open history file: %w", err))
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return r, r.degrade(fmt.Errorf("set history file permissions: %w", err))
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return r, r.degrade(fmt.Errorf("inspect history file: %w", err))
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return r, r.degrade(errors.New("history path is not a regular file"))
	}
	if createdNewFile {
		if err := syncDirectory(dir); err != nil {
			_ = file.Close()
			return r, r.degrade(fmt.Errorf("sync history directory: %w", err))
		}
	}
	r.file = file
	r.bytesWritten = info.Size()
	r.state = "ready"
	return r, nil
}

func (r *Recorder) degrade(err error) error {
	r.failure = err
	r.state = "degraded"
	return err
}

// validateAppendFile ensures an existing active file is regular and ends at a newline.
// T06 will validate and recover record contents and incomplete crash tails.
func validateAppendFile(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	if info.Size() == 0 {
		return nil
	}
	if _, err := file.Seek(-1, io.SeekEnd); err != nil {
		return err
	}
	var last [1]byte
	if _, err := io.ReadFull(file, last[:]); err != nil {
		return err
	}
	if last[0] != '\n' {
		return errors.New("file does not end with a newline")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// Begin durably records the admission snapshot before returning a handle. A caller
// must obtain a successful handle before invoking any upstream provider.
func (r *Recorder) Begin(start Start) (*Request, error) {
	if r == nil {
		return nil, ErrRecordingUnavailable
	}
	if start.RequestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("create history request id: %w", err)
		}
		start.RequestID = id
	}
	if !validRequestID(start.RequestID) || strings.TrimSpace(start.ServiceAccount.ID) == "" || strings.TrimSpace(start.ServiceAccount.Name) == "" ||
		strings.TrimSpace(start.Model.ID) == "" || strings.TrimSpace(start.Model.Name) == "" || start.Model.Version < 1 ||
		strings.TrimSpace(start.Model.Strategy) == "" || len(start.IncomingRequest) == 0 || !validJSONObject(start.IncomingRequest) {
		return nil, fmt.Errorf("%w: admission snapshot is incomplete", ErrInvalidRecord)
	}
	start.IncomingRequest = append(json.RawMessage(nil), start.IncomingRequest...)
	now := time.Now()
	handle := &Request{
		recorder:       r,
		id:             start.RequestID,
		startedAt:      now.UTC(),
		monotonicStart: now,
	}
	line := startLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "request_start",
			RequestID:     handle.id,
			StartedAt:     handle.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     0,
		},
		ServiceAccount:  start.ServiceAccount,
		Model:           start.Model,
		IncomingRequest: start.IncomingRequest,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.active[handle.id]; exists {
		return nil, fmt.Errorf("%w: request id is already active", ErrInvalidRecord)
	}
	if err := r.writeLocked(line, true); err != nil {
		return nil, err
	}
	r.active[handle.id] = handle
	return handle, nil
}

// ID returns the stable gateway request ID associated with this handle.
func (h *Request) ID() string {
	if h == nil {
		return ""
	}
	return h.id
}

// StartedAt returns the UTC wall-clock admission timestamp shared by every record.
func (h *Request) StartedAt() time.Time {
	if h == nil {
		return time.Time{}
	}
	return h.startedAt
}

// ElapsedNS returns the request's monotonic nanosecond offset from admission.
func (h *Request) ElapsedNS() int64 {
	if h == nil {
		return 0
	}
	return h.elapsedNS(time.Now())
}

// Transmit durably records an attempt before the caller transmits its effective
// request upstream. Attempt numbers start at one and increase for each request.
func (h *Request) Transmit(attempt Attempt) error {
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
	}
	if attempt.Number < 1 || strings.TrimSpace(attempt.Provider.ID) == "" || strings.TrimSpace(attempt.Provider.Name) == "" ||
		strings.TrimSpace(attempt.Provider.UpstreamModel) == "" || len(attempt.EffectiveRequest) == 0 || !validJSONObject(attempt.EffectiveRequest) {
		return fmt.Errorf("%w: transmission snapshot is incomplete", ErrInvalidRecord)
	}
	attempt.EffectiveRequest = append(json.RawMessage(nil), attempt.EffectiveRequest...)
	r := h.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ended {
		return ErrRequestEnded
	}
	if r.active[h.id] != h {
		return ErrRecordingUnavailable
	}
	if attempt.Number <= h.lastAttempt {
		return fmt.Errorf("%w: attempt number must increase", ErrInvalidRecord)
	}
	now := time.Now()
	line := transmitLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "upstream_transmission",
			RequestID:     h.id,
			StartedAt:     h.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     h.elapsedNS(now),
		},
		Attempt:          attempt.Number,
		ProviderID:       attempt.Provider.ID,
		ProviderName:     attempt.Provider.Name,
		UpstreamModel:    attempt.Provider.UpstreamModel,
		EffectiveRequest: attempt.EffectiveRequest,
	}
	err := r.writeLocked(line, true)
	if err == nil {
		h.lastAttempt = attempt.Number
	}
	return err
}

// Event records one complete upstream SSE frame immediately. Sequence numbers are
// assigned by the handle, beginning at one; an event is limited individually, so
// the total stream may exceed 64 MiB.
func (h *Request) Event(frame []byte, eventName string) error {
	if len(frame) > maxEventBytes {
		return ErrEventTooLarge
	}
	if !utf8.Valid(frame) {
		return fmt.Errorf("%w: SSE frame is not UTF-8 text", ErrInvalidRecord)
	}
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
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
	if h.lastAttempt == 0 {
		return fmt.Errorf("%w: response event has no recorded transmission", ErrInvalidRecord)
	}
	h.eventSequence++
	now := time.Now()
	line := eventLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "response_event",
			RequestID:     h.id,
			StartedAt:     h.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     h.elapsedNS(now),
		},
		Attempt:   h.lastAttempt,
		EventName: eventName,
		Sequence:  h.eventSequence,
		WireText:  string(frame),
	}
	return r.writeLocked(line, false)
}

// End records a final request outcome and always unregisters the handle. Outcomes
// are completed, failed, cancelled, or incomplete. safeError must be a short,
// pre-sanitized explanation and must not contain bodies, credentials, or URLs.
func (h *Request) End(outcome string, httpStatus int, safeError string, usage *Usage, timings Timings) error {
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
	}
	r := h.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ended {
		return nil
	}
	if !validOutcome(outcome) || httpStatus < 0 || httpStatus > 599 || len(safeError) > maxSafeError || !utf8.ValidString(safeError) || !validTimings(timings) || !validUsage(usage) {
		writeErr := r.writeEndLocked(h, "incomplete", 0, "request ended with invalid final details", nil, Timings{})
		h.ended = true
		delete(r.active, h.id)
		if writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("%w: invalid end details", ErrInvalidRecord)
	}
	if usage != nil {
		usage = cloneUsage(usage)
	}
	timings = cloneTimings(timings)
	err := r.writeEndLocked(h, outcome, httpStatus, safeError, usage, timings)
	h.ended = true
	delete(r.active, h.id)
	return err
}

func (r *Recorder) writeEndLocked(h *Request, outcome string, httpStatus int, safeError string, usage *Usage, timings Timings) error {
	if r.active[h.id] != h {
		return ErrRecordingUnavailable
	}
	now := time.Now()
	line := endLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "request_end",
			RequestID:     h.id,
			StartedAt:     h.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     h.elapsedNS(now),
		},
		Attempt:    h.lastAttempt,
		Outcome:    outcome,
		HTTPStatus: httpStatus,
		SafeError:  safeError,
		Usage:      usage,
		Timings:    timings,
	}
	return r.writeLocked(line, true)
}

func (h *Request) elapsedNS(now time.Time) int64 {
	elapsed := now.Sub(h.monotonicStart).Nanoseconds()
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func (r *Recorder) writeLocked(record any, durable bool) error {
	if r.closed || r.file == nil || r.failure != nil {
		r.potentiallyLostRecords++
		return ErrRecordingUnavailable
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode request history record: %w", err)
	}
	encoded = append(encoded, '\n')
	remaining := encoded
	for len(remaining) > 0 {
		n, writeErr := r.file.Write(remaining)
		if n < 0 || n > len(remaining) {
			r.markWriteFailureLocked(errors.New("history writer returned an invalid byte count"))
			return ErrRecordingUnavailable
		}
		r.bytesWritten += int64(n)
		remaining = remaining[n:]
		if writeErr != nil {
			r.markWriteFailureLocked(writeErr)
			return ErrRecordingUnavailable
		}
		if n == 0 {
			r.markWriteFailureLocked(io.ErrShortWrite)
			return ErrRecordingUnavailable
		}
	}
	r.unsyncedRecords++
	if !durable {
		return nil
	}
	if err := r.file.Sync(); err != nil {
		r.markSyncFailureLocked(err)
		return ErrRecordingUnavailable
	}
	r.unsyncedRecords = 0
	return nil
}

func (r *Recorder) markWriteFailureLocked(err error) {
	r.failure = err
	r.state = "degraded"
	r.potentiallyLostRecords += r.unsyncedRecords + 1
	r.unsyncedRecords = 0
}

func (r *Recorder) markSyncFailureLocked(err error) {
	r.failure = err
	r.state = "degraded"
	r.potentiallyLostRecords += r.unsyncedRecords
	r.unsyncedRecords = 0
}

// Status returns storage health without returning error strings, paths, or request data.
func (r *Recorder) Status() RecorderStatus {
	if r == nil {
		return RecorderStatus{State: "degraded"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.state
	if r.closed {
		state = "closed"
	}
	return RecorderStatus{
		State:                  state,
		PotentiallyLostRecords: r.potentiallyLostRecords,
		ActiveRequests:         len(r.active),
		BytesWritten:           r.bytesWritten,
	}
}

// Close completes any still-active requests as incomplete, syncs pending event
// records, and closes the active file. It is safe to call more than once.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	for id, h := range r.active {
		if !h.ended {
			_ = r.writeEndLocked(h, "incomplete", 0, "request ended when recorder closed", nil, Timings{})
			h.ended = true
		}
		delete(r.active, id)
	}
	if r.file != nil {
		if r.unsyncedRecords > 0 && r.failure == nil {
			if err := r.file.Sync(); err != nil {
				r.markSyncFailureLocked(err)
			}
		}
		if err := r.file.Close(); err != nil {
			if r.closeErr == nil {
				r.closeErr = fmt.Errorf("close request history file: %w", err)
			}
		}
		r.file = nil
	}
	r.closed = true
	if r.closeErr != nil {
		return r.closeErr
	}
	if r.failure != nil {
		r.closeErr = ErrRecordingUnavailable
		return r.closeErr
	}
	return nil
}

type baseLine struct {
	SchemaVersion int       `json:"schema_version"`
	Type          string    `json:"type"`
	RequestID     string    `json:"request_id"`
	StartedAt     time.Time `json:"started_at"`
	TimestampUTC  time.Time `json:"timestamp_utc"`
	ElapsedNS     int64     `json:"elapsed_ns"`
}

type startLine struct {
	baseLine
	ServiceAccount  ServiceAccountSnapshot `json:"service_account"`
	Model           ModelSnapshot          `json:"model"`
	IncomingRequest json.RawMessage        `json:"incoming_request"`
}

type transmitLine struct {
	baseLine
	Attempt          int             `json:"attempt"`
	ProviderID       string          `json:"provider_id"`
	ProviderName     string          `json:"provider_name"`
	UpstreamModel    string          `json:"upstream_model"`
	EffectiveRequest json.RawMessage `json:"effective_request"`
}

type eventLine struct {
	baseLine
	Attempt   int    `json:"attempt"`
	EventName string `json:"event_name"`
	Sequence  uint64 `json:"sequence"`
	WireText  string `json:"wire_text"`
}

type endLine struct {
	baseLine
	Attempt    int     `json:"attempt"`
	Outcome    string  `json:"outcome"`
	HTTPStatus int     `json:"http_status"`
	SafeError  string  `json:"safe_error"`
	Usage      *Usage  `json:"usage"`
	Timings    Timings `json:"timings"`
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > maxRequestID {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && char != '-' && char != '_' && char != '.' && char != ':' {
			return false
		}
	}
	return true
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded), nil
}

func validOutcome(outcome string) bool {
	switch outcome {
	case "completed", "failed", "cancelled", "incomplete":
		return true
	default:
		return false
	}
}

func validUsage(usage *Usage) bool {
	if usage == nil {
		return true
	}
	for _, value := range []*int64{usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.ReasoningOutputTokens} {
		if value != nil && *value < 0 {
			return false
		}
	}
	return true
}

func cloneUsage(usage *Usage) *Usage {
	copyCount := func(source *int64) *int64 {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	return &Usage{
		InputTokens:           copyCount(usage.InputTokens),
		OutputTokens:          copyCount(usage.OutputTokens),
		CachedInputTokens:     copyCount(usage.CachedInputTokens),
		ReasoningOutputTokens: copyCount(usage.ReasoningOutputTokens),
	}
}

func validTimings(timings Timings) bool {
	for _, value := range []*int64{
		timings.AuthenticationPreparationNS,
		timings.ConnectionEstablishedNS,
		timings.RequestTransmissionNS,
		timings.FirstEventNS,
		timings.FirstOutputNS,
		timings.StreamCompletionNS,
		timings.DownstreamDeliveryNS,
	} {
		if value != nil && *value < 0 {
			return false
		}
	}
	return true
}

func validJSONObject(raw []byte) bool {
	if !json.Valid(raw) {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	return object != nil
}

func cloneTimings(timings Timings) Timings {
	copyOffset := func(source *int64) *int64 {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	return Timings{
		AuthenticationPreparationNS: copyOffset(timings.AuthenticationPreparationNS),
		ConnectionEstablishedNS:     copyOffset(timings.ConnectionEstablishedNS),
		RequestTransmissionNS:       copyOffset(timings.RequestTransmissionNS),
		FirstEventNS:                copyOffset(timings.FirstEventNS),
		FirstOutputNS:               copyOffset(timings.FirstOutputNS),
		StreamCompletionNS:          copyOffset(timings.StreamCompletionNS),
		DownstreamDeliveryNS:        copyOffset(timings.DownstreamDeliveryNS),
	}
}
