// Package provider implements upstream Responses API contracts independently of
// gateway routing and credential storage.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	openAIResponsesURL = "https://api.openai.com/v1/responses"
	maxSSEFrameBytes   = 64 << 20
	maxErrorBodyBytes  = 64 << 20
)

var (
	ErrMissingManagedToken = errors.New("managed provider token is unavailable")
	ErrInvalidRequestID    = errors.New("gateway request ID is invalid")
	ErrInvalidPrepared     = errors.New("prepared upstream request is invalid")
	ErrUpstreamUnavailable = errors.New("upstream transport is unavailable")
	ErrUpstreamRead        = errors.New("upstream response could not be read")
	ErrStreamRead          = errors.New("upstream event stream could not be read")
	ErrInvalidUTF8         = errors.New("upstream event stream is not UTF-8")
	ErrEventTooLarge       = errors.New("upstream event exceeds the 64 MiB limit")
	ErrNoEventStream       = errors.New("upstream response has no event stream")
	ErrResponseClosed      = errors.New("upstream response is closed")
)

// Adapter is the small gateway-facing provider contract. Implementations prepare
// an effective upstream request and return one bounded HTTP/SSE response. Provider
// adapters never read credentials from storage; the executor supplies the managed
// bearer token for each request.
type Adapter interface {
	Prepare(upstreamModel string, incoming json.RawMessage) (PreparedRequest, error)
	Send(ctx context.Context, managedToken, gatewayRequestID string, prepared PreparedRequest, trace *Trace) (*UpstreamResponse, error)
}

// PreparedRequest records the client streaming preference and the complete,
// normalized request sent upstream. EffectiveJSON is JSON object data and is safe
// for the history transmission record; it contains no HTTP authorization header.
type PreparedRequest struct {
	EffectiveJSON json.RawMessage
	ClientStream  bool
}

// CapabilityError is a client-facing incompatibility in the SIWC Responses
// contract. Param and Code are suitable for a standard OpenAI error envelope.
type CapabilityError struct {
	Message string
	Param   string
	Code    string
}

func (e *CapabilityError) Error() string {
	if e == nil {
		return "request capability is unsupported"
	}
	return e.Message
}

// OpenAIResponsesAdapter sends fixed-host HTTPS requests to the public Responses
// endpoint. Its injectable client is useful for an in-process test transport; the
// production constructor uses a shared, endpoint-restricted transport.
type OpenAIResponsesAdapter struct {
	client *http.Client
}

// NewOpenAIResponsesAdapter creates an adapter. Passing no client selects the
// production client. An injected client may replace transport behavior, but the
// adapter still fixes the URL, request method, headers, and redirect policy.
func NewOpenAIResponsesAdapter(client ...*http.Client) *OpenAIResponsesAdapter {
	if len(client) > 0 && client[0] != nil {
		injectedCopy := *client[0]
		injected := &injectedCopy
		injected.Timeout = 0 // SSE lifetime is controlled by the request context.
		injected.Jar = nil
		injected.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if injected.Transport == nil {
			injected.Transport = restrictedResponsesTransport{base: productionResponsesTransport()}
		} else {
			injected.Transport = restrictedResponsesTransport{base: injected.Transport}
		}
		return &OpenAIResponsesAdapter{client: injected}
	}
	return &OpenAIResponsesAdapter{client: productionResponsesClient()}
}

func (a *OpenAIResponsesAdapter) Prepare(upstreamModel string, incoming json.RawMessage) (PreparedRequest, error) {
	if !utf8.Valid(incoming) {
		return PreparedRequest{}, capability("", "invalid_json", "The request body must be valid UTF-8 JSON.")
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return PreparedRequest{}, capability("model", "invalid_value", "The configured upstream model is invalid.")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(incoming, &fields); err != nil || fields == nil {
		return PreparedRequest{}, capability("", "invalid_json", "The request body must be a JSON object.")
	}

	modelRaw, exists := fields["model"]
	if !exists {
		return PreparedRequest{}, capability("model", "missing_required_parameter", "The request must include a model.")
	}
	var requestedModel string
	if err := json.Unmarshal(modelRaw, &requestedModel); err != nil || strings.TrimSpace(requestedModel) == "" {
		return PreparedRequest{}, capability("model", "invalid_type", "The model field must be a non-empty string.")
	}

	var clientStream bool
	if raw, exists := fields["stream"]; exists {
		var err error
		clientStream, err = strictBool(raw)
		if err != nil {
			return PreparedRequest{}, capability("stream", "invalid_type", "The stream field must be a boolean.")
		}
	}
	if raw, exists := fields["store"]; exists {
		if _, err := strictBool(raw); err != nil {
			return PreparedRequest{}, capability("store", "invalid_type", "The store field must be a boolean.")
		}
	}

	if raw, exists := fields["input"]; !exists {
		return PreparedRequest{}, capability("input", "missing_required_parameter", "Input must be a complete array of conversation items.")
	} else {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return PreparedRequest{}, capability("input", "input_must_be_array", "Input must be a complete array of conversation items.")
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil || items == nil {
			return PreparedRequest{}, capability("input", "input_must_be_array", "Input must be a complete array of conversation items.")
		}
		for index, item := range items {
			var message map[string]json.RawMessage
			if json.Unmarshal(item, &message) != nil || message == nil {
				continue // The upstream API owns validation of other item shapes.
			}
			if role, ok := message["role"]; ok {
				var roleName string
				if json.Unmarshal(role, &roleName) == nil && roleName == "system" {
					return PreparedRequest{}, capability("input", "unsupported_value", "System role message items are not supported; use instructions or developer messages.")
				}
			}
			if inputUsesAudioOrVideo(message) {
				return PreparedRequest{}, capability("input", "unsupported_value", "Audio and video input are not supported by this provider adapter.")
			}
			var itemType string
			if rawType, ok := message["type"]; ok && json.Unmarshal(rawType, &itemType) == nil && itemType == "additional_tools" {
				if err := checkToolDefinitions(item, "input["+itoa(index)+"].additional_tools"); err != nil {
					return PreparedRequest{}, err
				}
			}
		}
	}

	for _, field := range deniedFields {
		if _, exists := fields[field]; exists {
			return PreparedRequest{}, capability(field, "unsupported_parameter", "The request uses a parameter that is unavailable with Sign in with ChatGPT.")
		}
	}
	if _, exists := fields["connectors"]; exists {
		return PreparedRequest{}, capability("connectors", "unsupported_tool", "Connector tools are not supported by this provider adapter.")
	}
	if raw, exists := fields["tools"]; exists {
		if err := checkToolDefinitions(raw, "tools"); err != nil {
			return PreparedRequest{}, err
		}
	}
	if raw, exists := fields["additional_tools"]; exists {
		if err := checkToolDefinitions(raw, "additional_tools"); err != nil {
			return PreparedRequest{}, err
		}
	}

	// Copy the raw values so unknown fields, nested data, and flat function/custom
	// tools pass through unchanged. Only the documented endpoint adaptations apply.
	effective := make(map[string]json.RawMessage, len(fields)+1)
	for key, value := range fields {
		effective[key] = value
	}
	upstreamModelJSON, _ := json.Marshal(upstreamModel)
	effective["model"] = upstreamModelJSON
	effective["stream"] = json.RawMessage("true")
	effective["store"] = json.RawMessage("false")
	payload, err := json.Marshal(effective)
	if err != nil {
		return PreparedRequest{}, capability("", "invalid_json", "The request body could not be prepared.")
	}
	return PreparedRequest{EffectiveJSON: payload, ClientStream: clientStream}, nil
}

// Send returns response metadata for every received HTTP status. For an unexpected
// successful non-SSE response it returns both a bounded response and ErrNoEventStream
// so the executor can report an explicit gateway error while retaining the body.
func (a *OpenAIResponsesAdapter) Send(ctx context.Context, managedToken, gatewayRequestID string, prepared PreparedRequest, trace *Trace) (*UpstreamResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(managedToken) == "" {
		return nil, ErrMissingManagedToken
	}
	if !validRequestIDHeader(gatewayRequestID) {
		return nil, ErrInvalidRequestID
	}
	if !validPreparedJSON(prepared.EffectiveJSON) {
		return nil, ErrInvalidPrepared
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(prepared.EffectiveJSON))
	if err != nil {
		return nil, ErrInvalidPrepared
	}
	// Construct only gateway-owned upstream headers. Client Authorization and all
	// other harness headers are deliberately outside this provider API boundary.
	request.Header.Set("Authorization", "Bearer "+managedToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-Client-Request-Id", gatewayRequestID)
	request.Header["User-Agent"] = []string{""} // suppress net/http's default User-Agent.
	if trace != nil {
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace.clientTrace()))
	}
	response, err := a.client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		// Do not expose URL, request, or transport details in an error that may be
		// returned to a harness or recorded in history.
		return nil, ErrUpstreamUnavailable
	}

	result := &UpstreamResponse{
		StatusCode:        response.StatusCode,
		ContentType:       safeHeader(response.Header.Get("Content-Type"), 512),
		UpstreamRequestID: upstreamRequestID(response.Header),
		RetryAfter:        retryAfter(response.Header.Get("Retry-After")),
		trace:             trace,
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.ErrorBody, result.ErrorBodyTruncated, result.ErrorBodyReadFailed = readBoundedErrorBody(response.Body)
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return result, nil
	}
	mediaType, _, mediaTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaTypeErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		result.UnexpectedContentType = true
		result.ErrorBody, result.ErrorBodyTruncated, result.ErrorBodyReadFailed = readBoundedErrorBody(response.Body)
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return result, ErrNoEventStream
	}
	if response.Body == nil {
		result.body = io.NopCloser(bytes.NewReader(nil))
	} else {
		result.body = response.Body
	}
	return result, nil
}

var deniedFields = []string{
	"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata",
	"moderation", "multi_agent", "prompt", "prompt_cache_retention", "previous_response_id",
	"safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user",
	"programmatic_tool_calling",
}

func capability(param, code, message string) *CapabilityError {
	return &CapabilityError{Message: message, Param: param, Code: code}
}

func strictBool(raw json.RawMessage) (bool, error) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("value is not a JSON boolean")
	}
}

func validPreparedJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return false
	}
	var fields map[string]json.RawMessage
	return json.Unmarshal(trimmed, &fields) == nil && fields != nil
}

func validRequestIDHeader(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

type restrictedResponsesTransport struct {
	base http.RoundTripper
}

func (t restrictedResponsesTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Host != "api.openai.com" ||
		request.URL.Port() != "" || request.URL.User != nil || request.URL.Path != "/v1/responses" ||
		request.URL.RawQuery != "" || request.URL.Fragment != "" {
		return nil, errors.New("unexpected Responses endpoint")
	}
	return t.base.RoundTrip(request)
}

var responsesTransportOnce sync.Once
var sharedResponsesTransport http.RoundTripper
var responsesClientOnce sync.Once
var sharedResponsesClient *http.Client

func productionResponsesTransport() http.RoundTripper {
	responsesTransportOnce.Do(func() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		transport.TLSHandshakeTimeout = 10 * time.Second
		transport.ResponseHeaderTimeout = 30 * time.Second
		transport.ExpectContinueTimeout = time.Second
		transport.IdleConnTimeout = 90 * time.Second
		transport.MaxIdleConns = 64
		transport.MaxIdleConnsPerHost = 16
		transport.ForceAttemptHTTP2 = true
		transport.DisableCompression = true
		sharedResponsesTransport = restrictedResponsesTransport{base: transport}
	})
	return sharedResponsesTransport
}

func productionResponsesClient() *http.Client {
	responsesClientOnce.Do(func() {
		sharedResponsesClient = &http.Client{
			Transport:     productionResponsesTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			// Deliberately no Client.Timeout: a healthy SSE response can be long-lived.
		}
	})
	return sharedResponsesClient
}

func readBoundedErrorBody(body io.ReadCloser) ([]byte, bool, bool) {
	if body == nil {
		return nil, false, false
	}
	data, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes+1))
	truncated := len(data) > maxErrorBodyBytes
	if truncated {
		data = data[:maxErrorBodyBytes]
	}
	return data, truncated, err != nil
}

func safeHeader(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > limit {
		return ""
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return ""
		}
	}
	return value
}

func upstreamRequestID(header http.Header) string {
	for _, name := range []string{"X-Request-Id", "OpenAI-Request-Id"} {
		if value := safeHeader(header.Get(name), 256); value != "" {
			return value
		}
	}
	return ""
}

func retryAfter(value string) string {
	value = safeHeader(value, 128)
	if value == "" {
		return ""
	}
	if _, err := time.ParseDuration(value + "s"); err == nil {
		return value
	}
	if _, err := http.ParseTime(value); err == nil {
		return value
	}
	return ""
}

func inputUsesAudioOrVideo(message map[string]json.RawMessage) bool {
	if rawType, ok := message["type"]; ok {
		var itemType string
		if json.Unmarshal(rawType, &itemType) == nil && unsupportedInputModality(itemType) {
			return true
		}
	}
	contentRaw, ok := message["content"]
	if !ok {
		return false
	}
	var content []json.RawMessage
	if json.Unmarshal(contentRaw, &content) != nil {
		return false
	}
	for _, part := range content {
		var contentPart map[string]json.RawMessage
		if json.Unmarshal(part, &contentPart) != nil {
			continue
		}
		if rawType, ok := contentPart["type"]; ok {
			var partType string
			if json.Unmarshal(rawType, &partType) == nil && unsupportedInputModality(partType) {
				return true
			}
		}
	}
	return false
}

func unsupportedInputModality(value string) bool {
	switch strings.ToLower(value) {
	case "audio", "input_audio", "input_audio_transcription", "video", "input_video":
		return true
	default:
		return false
	}
}

var unsupportedToolTypes = map[string]struct{}{
	"image_generation": {}, "file_search": {}, "code_interpreter": {},
	"computer": {}, "computer_use": {}, "computer_use_preview": {},
	"mcp": {}, "hosted_mcp": {}, "hostedmcp": {}, "connector": {}, "connectors": {}, "tool_search": {},
}

func checkToolDefinitions(raw json.RawMessage, path string) *CapabilityError {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil // Let the upstream API validate malformed tool container shapes.
	}
	return walkToolDefinition(value, path)
}

func walkToolDefinition(value any, path string) *CapabilityError {
	switch item := value.(type) {
	case []any:
		for i, child := range item {
			if err := walkToolDefinition(child, path+"["+itoa(i)+"]"); err != nil {
				return err
			}
		}
	case map[string]any:
		rawType, typePresent := item["type"]
		toolType, typeIsString := rawType.(string)
		if typeIsString {
			if _, unsupported := unsupportedToolTypes[strings.ToLower(toolType)]; unsupported {
				return capability(path+".type", "unsupported_tool", "This tool type is not supported by the Sign in with ChatGPT Responses adapter.")
			}
		}
		if !typePresent {
			// A namespace may be represented as a map from names to definitions.
			keys := make([]string, 0, len(item))
			for key := range item {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := walkToolDefinition(item[key], path); err != nil {
					return err
				}
			}
			return nil
		}
		for _, key := range []string{"namespace", "additional_tools", "tools", "connectors"} {
			if nested, ok := item[key]; ok {
				if err := walkToolDefinition(nested, path+"."+key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}

// UpstreamResponse exposes only selected upstream metadata. ErrorBody is complete
// up to 64 MiB; ErrorBodyTruncated and ErrorBodyReadFailed make any limit or read
// failure visible instead of silently hiding it. Successful response bodies stay
// streaming and must be closed by the caller.
type UpstreamResponse struct {
	StatusCode            int
	ContentType           string
	UpstreamRequestID     string
	RetryAfter            string
	ErrorBody             []byte
	ErrorBodyTruncated    bool
	ErrorBodyReadFailed   bool
	UnexpectedContentType bool

	body   io.ReadCloser
	reader *SSEReader
	trace  *Trace
	closed bool
}

// NextEvent incrementally reads one complete SSE frame. A final unterminated frame
// is returned once with Complete=false; it is not a terminal response event.
func (r *UpstreamResponse) NextEvent() (SSEEvent, error) {
	if r == nil || r.closed {
		return SSEEvent{}, ErrResponseClosed
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 || r.body == nil {
		return SSEEvent{}, ErrNoEventStream
	}
	if r.reader == nil {
		r.reader = NewSSEReader(r.body, r.trace)
	}
	return r.reader.Next()
}

// Close releases the upstream socket or completes its keep-alive lifecycle.
func (r *UpstreamResponse) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	if r.body == nil {
		return nil
	}
	if err := r.body.Close(); err != nil {
		return ErrUpstreamRead
	}
	return nil
}

// TimingSnapshot contains monotonic offsets from NewTrace's origin or the clock
// supplied to NewTraceWithElapsed. Nil means a stage was not observed. Connection
// acquisition and reuse are tracked separately from a new dial or TLS handshake.
type TimingSnapshot struct {
	ConnectionRequestedNS   *int64
	ConnectionEstablishedNS *int64
	RequestTransmissionNS   *int64
	FirstEventNS            *int64
	FirstOutputNS           *int64
	StreamCompletionNS      *int64
	ConnectionReused        *bool
}

// Trace records low-cardinality timing only. It never retains request or response
// content, headers, token values, or transport error strings.
type Trace struct {
	elapsed func() int64
	mu      sync.Mutex
	state   TimingSnapshot
}

// NewTrace uses the caller's monotonic-bearing origin for all offsets. Callers
// should capture origin with time.Now() at request admission.
func NewTrace(origin time.Time) *Trace {
	if origin.IsZero() {
		origin = time.Now()
	}
	return NewTraceWithElapsed(func() int64 { return time.Since(origin).Nanoseconds() })
}

// NewTraceWithElapsed lets the executor use its recorder handle's monotonic
// ElapsedNS accessor, keeping provider offsets on precisely the same request clock.
func NewTraceWithElapsed(elapsed func() int64) *Trace {
	if elapsed == nil {
		origin := time.Now()
		elapsed = func() int64 { return time.Since(origin).Nanoseconds() }
	}
	return &Trace{elapsed: elapsed}
}

// ElapsedNS returns monotonic time from the supplied request clock.
func (t *Trace) ElapsedNS() int64 {
	if t == nil || t.elapsed == nil {
		return 0
	}
	elapsed := t.elapsed()
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

// Snapshot returns a detached timing snapshot safe to convert to history.Timings.
func (t *Trace) Snapshot() TimingSnapshot {
	if t == nil {
		return TimingSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return TimingSnapshot{
		ConnectionRequestedNS:   cloneInt64(t.state.ConnectionRequestedNS),
		ConnectionEstablishedNS: cloneInt64(t.state.ConnectionEstablishedNS),
		RequestTransmissionNS:   cloneInt64(t.state.RequestTransmissionNS),
		FirstEventNS:            cloneInt64(t.state.FirstEventNS),
		FirstOutputNS:           cloneInt64(t.state.FirstOutputNS),
		StreamCompletionNS:      cloneInt64(t.state.StreamCompletionNS),
		ConnectionReused:        cloneBool(t.state.ConnectionReused),
	}
}

func (t *Trace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			t.mark(&t.state.ConnectionRequestedNS)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.mark(&t.state.ConnectionEstablishedNS)
			t.mu.Lock()
			if t.state.ConnectionReused == nil {
				reused := info.Reused
				t.state.ConnectionReused = &reused
			}
			t.mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				t.mark(&t.state.RequestTransmissionNS)
			}
		},
	}
}

func (t *Trace) MarkFirstEvent() {
	if t != nil {
		t.mark(&t.state.FirstEventNS)
	}
}

func (t *Trace) MarkFirstOutput() {
	if t != nil {
		t.mark(&t.state.FirstOutputNS)
	}
}

func (t *Trace) MarkStreamCompletion() {
	if t != nil {
		t.mark(&t.state.StreamCompletionNS)
	}
}

func (t *Trace) mark(target **int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if *target != nil {
		return
	}
	elapsed := t.elapsed()
	if elapsed < 0 {
		elapsed = 0
	}
	value := elapsed
	*target = &value
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// SSEEvent preserves the exact upstream frame and exposes its parsed data when
// available. Raw includes comments, unknown events, and original line terminators.
type SSEEvent struct {
	Raw              []byte
	Name             string
	Type             string
	Data             []byte
	DataJSON         json.RawMessage
	ResponseJSON     json.RawMessage
	RawTerminalJSON  json.RawMessage
	Usage            *Usage
	HasData          bool
	Complete         bool
	Terminal         bool
	Outcome          string
	JSONTypeMismatch bool
}

// Usage mirrors the provider-reported totals and subset counts. Nil pointers mean
// unknown; a pointer to zero means explicitly reported zero.
type Usage struct {
	InputTokens           *int64
	OutputTokens          *int64
	CachedInputTokens     *int64
	ReasoningOutputTokens *int64
}

// SSEReader reads one bounded event at a time and does not buffer the entire stream.
type SSEReader struct {
	reader *bufio.Reader
	trace  *Trace
	done   bool
	err    error
}

// NewSSEReader returns an incremental reader over a provider response body.
func NewSSEReader(source io.Reader, trace *Trace) *SSEReader {
	return &SSEReader{reader: bufio.NewReaderSize(source, 32<<10), trace: trace}
}

// Next returns the next event, io.EOF after a clean stream end, or a sanitized
// parser/read error. An incomplete final frame is returned before io.EOF.
func (r *SSEReader) Next() (SSEEvent, error) {
	if r == nil || r.done {
		if r != nil && r.err != nil {
			return SSEEvent{}, r.err
		}
		return SSEEvent{}, io.EOF
	}
	frame := make([]byte, 0, 4<<10)
	lineStart := 0
	for {
		fragment, readErr := r.reader.ReadSlice('\n')
		if len(fragment) > maxSSEFrameBytes-len(frame) {
			r.done = true
			return SSEEvent{}, ErrEventTooLarge
		}
		frame = append(frame, fragment...)
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			lineEnd := len(frame) - 1
			if lineEnd > lineStart && frame[lineEnd-1] == '\r' {
				lineEnd--
			}
			if lineEnd == lineStart {
				event, err := r.finish(frame, true)
				if err != nil {
					r.done = true
					return SSEEvent{}, err
				}
				return event, nil
			}
			lineStart = len(frame)
		}
		switch readErr {
		case nil, bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(frame) == 0 {
				r.done = true
				return SSEEvent{}, io.EOF
			}
			event, err := r.finish(frame, false)
			r.done = true
			if err != nil {
				return SSEEvent{}, err
			}
			return event, nil
		default:
			if len(frame) > 0 {
				event, parseErr := r.finish(frame, false)
				r.done = true
				if parseErr != nil {
					return SSEEvent{}, parseErr
				}
				if errors.Is(readErr, context.Canceled) {
					r.err = context.Canceled
				} else if errors.Is(readErr, context.DeadlineExceeded) {
					r.err = context.DeadlineExceeded
				} else {
					r.err = ErrStreamRead
				}
				return event, nil
			}
			r.done = true
			if errors.Is(readErr, context.Canceled) {
				return SSEEvent{}, context.Canceled
			}
			if errors.Is(readErr, context.DeadlineExceeded) {
				return SSEEvent{}, context.DeadlineExceeded
			}
			return SSEEvent{}, ErrStreamRead
		}
	}
}

func (r *SSEReader) finish(frame []byte, complete bool) (SSEEvent, error) {
	if !utf8.Valid(frame) {
		return SSEEvent{}, ErrInvalidUTF8
	}
	event := parseEvent(frame, complete)
	if r.trace != nil && complete {
		if event.HasData {
			r.trace.MarkFirstEvent()
		}
		if event.hasVisibleOutput() {
			r.trace.MarkFirstOutput()
		}
		if event.Terminal {
			r.trace.MarkStreamCompletion()
		}
	}
	return event, nil
}

func parseEvent(frame []byte, complete bool) SSEEvent {
	event := SSEEvent{Raw: frame, Complete: complete}
	var eventName string
	var data []byte
	hasData := false
	lineStart := 0
	for lineStart < len(frame) {
		lineEnd := bytes.IndexByte(frame[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(frame)
		} else {
			lineEnd += lineStart
		}
		contentEnd := lineEnd
		if contentEnd > lineStart && frame[contentEnd-1] == '\r' {
			contentEnd--
		}
		line := frame[lineStart:contentEnd]
		if len(line) > 0 && line[0] != ':' {
			field := line
			value := []byte(nil)
			if colon := bytes.IndexByte(line, ':'); colon >= 0 {
				field = line[:colon]
				value = line[colon+1:]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
			}
			switch string(field) {
			case "event":
				eventName = string(value)
			case "data":
				if hasData {
					data = append(data, '\n')
				}
				data = append(data, value...)
				hasData = true
			}
		}
		if lineEnd == len(frame) {
			break
		}
		lineStart = lineEnd + 1
	}
	event.Name = eventName
	event.Data = data
	event.HasData = hasData
	if hasData && json.Valid(data) {
		event.DataJSON = json.RawMessage(data)
	}
	if len(event.DataJSON) > 0 {
		var envelope struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(event.DataJSON, &envelope) == nil {
			event.Type = eventName
			if event.Type == "" {
				event.Type = envelope.Type
			}
			if event.Name == "" {
				event.Name = event.Type
			}
			if eventName != "" && envelope.Type != "" && eventName != envelope.Type {
				event.JSONTypeMismatch = true
			}
		}
	}
	if event.Type == "" {
		event.Type = eventName
	}
	if complete && !event.JSONTypeMismatch && jsonObject(event.DataJSON) {
		switch event.Type {
		case "response.completed":
			var envelope struct {
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(event.DataJSON, &envelope) == nil && responseStatusIs(envelope.Response, "completed") {
				event.Terminal, event.Outcome = true, "completed"
			}
		case "response.failed":
			var envelope struct {
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(event.DataJSON, &envelope) == nil && responseStatusIs(envelope.Response, "failed") {
				event.Terminal, event.Outcome = true, "failed"
			}
		case "error":
			event.Terminal, event.Outcome = true, "failed"
		case "response.incomplete":
			var envelope struct {
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(event.DataJSON, &envelope) == nil && responseStatusIs(envelope.Response, "incomplete") {
				event.Terminal, event.Outcome = true, "incomplete"
			}
		}
		if event.Terminal {
			var envelope struct {
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(event.DataJSON, &envelope) == nil && jsonObject(envelope.Response) {
				event.ResponseJSON = envelope.Response
				event.Usage = responseUsage(envelope.Response)
			}
			event.RawTerminalJSON = event.DataJSON
			if event.Usage == nil {
				event.Usage = responseUsage(event.DataJSON)
			}
		}
	}
	return event
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func responseStatusIs(raw json.RawMessage, expected string) bool {
	if !jsonObject(raw) {
		return false
	}
	var response struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(raw, &response) == nil && response.Status == expected
}

func (e SSEEvent) hasVisibleOutput() bool {
	if !e.Complete || e.JSONTypeMismatch || len(e.DataJSON) == 0 {
		return false
	}
	switch e.Type {
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.refusal.delta":
	default:
		return false
	}
	var envelope struct {
		Delta json.RawMessage `json:"delta"`
	}
	if json.Unmarshal(e.DataJSON, &envelope) != nil || len(envelope.Delta) == 0 {
		return false
	}
	var delta string
	return json.Unmarshal(envelope.Delta, &delta) == nil && delta != ""
}

type responseUsageJSON struct {
	InputTokens           json.RawMessage `json:"input_tokens"`
	OutputTokens          json.RawMessage `json:"output_tokens"`
	CachedInputTokens     json.RawMessage `json:"cached_input_tokens"`
	ReasoningOutputTokens json.RawMessage `json:"reasoning_output_tokens"`
	InputDetails          struct {
		CachedTokens json.RawMessage `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens json.RawMessage `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func responseUsage(raw json.RawMessage) *Usage {
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if !jsonObject(raw) || json.Unmarshal(raw, &envelope) != nil || len(envelope.Usage) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Usage), []byte("null")) {
		return nil
	}
	var decoded responseUsageJSON
	if json.Unmarshal(envelope.Usage, &decoded) != nil || !jsonObject(envelope.Usage) {
		return nil
	}
	cachedRaw := decoded.CachedInputTokens
	if len(cachedRaw) == 0 {
		cachedRaw = decoded.InputDetails.CachedTokens
	}
	reasoningRaw := decoded.ReasoningOutputTokens
	if len(reasoningRaw) == 0 {
		reasoningRaw = decoded.OutputDetails.ReasoningTokens
	}
	return &Usage{
		InputTokens:           optionalCount(decoded.InputTokens),
		OutputTokens:          optionalCount(decoded.OutputTokens),
		CachedInputTokens:     optionalCount(cachedRaw),
		ReasoningOutputTokens: optionalCount(reasoningRaw),
	}
}

func optionalCount(raw json.RawMessage) *int64 {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 {
		return nil
	}
	return &value
}
