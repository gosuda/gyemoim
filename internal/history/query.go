package history

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultPageSize      = 25
	MaxPageSize          = 100
	MaxQueryDuration     = 30 * time.Second
	MaxQueryBatch        = 128
	MaxAggregateGroups   = 1000
	MaxQueryReaders      = 4
	MaxContentChunkBytes = 1 << 20
	MaxEventPageRawBytes = 1 << 20
	MaxDetailAttempts    = 128
	MaxDetailResponses   = 128
)

var (
	ErrMaintenance       = errors.New("history maintenance is in progress")
	ErrQueryBusy         = errors.New("history query concurrency limit reached")
	ErrQueryUnavailable  = errors.New("history query is unavailable")
	ErrInvalidQuery      = errors.New("invalid history query")
	ErrQueryNotFound     = errors.New("history request not found")
	ErrTooManyGroups     = errors.New("history aggregate exceeds the group limit")
	ErrAggregateOverflow = errors.New("history aggregate counter overflow")
)

var ErrQueryIndexUnavailable = errors.New("temporary history query index is unavailable")

// QueryService exposes bounded, snapshot-consistent history reads.
type QueryService struct{ recorder *Recorder }

func NewQueryService(recorder *Recorder) *QueryService { return &QueryService{recorder: recorder} }

// QueryFilter applies UTC half-open start-time bounds and exact identity filters.
type QueryFilter struct {
	From          *time.Time
	To            *time.Time
	AccountID     string
	ModelID       string
	ProviderID    string
	UpstreamModel string
	Outcome       string
}

// RequestSummary is the bounded list and aggregation representation of a request.
type RequestSummary struct {
	RequestID         string                 `json:"requestId"`
	StartedAt         time.Time              `json:"startedAt"`
	DurationNS        int64                  `json:"durationNs"`
	Outcome           string                 `json:"outcome"`
	HTTPStatus        int                    `json:"httpStatus"`
	ServiceAccount    ServiceAccountSnapshot `json:"serviceAccount"`
	Model             ModelSnapshot          `json:"model"`
	Provider          *ProviderSnapshot      `json:"provider,omitempty"`
	UpstreamRequestID string                 `json:"upstreamRequestId,omitempty"`
	Attempts          int                    `json:"upstreamAttempts"`
	SafeError         string                 `json:"safeError,omitempty"`
	Usage             *Usage                 `json:"usage"`
	Timings           Timings                `json:"timings"`
	Active            bool                   `json:"active"`
	Interrupted       bool                   `json:"interrupted"`
}

func summaryFromEnd(record Record) RequestSummary {
	summary := RequestSummary{
		RequestID: record.RequestID, StartedAt: record.StartedAt, Outcome: record.Outcome,
		DurationNS: record.ElapsedNS,
		HTTPStatus: record.HTTPStatus, UpstreamRequestID: record.UpstreamRequestID,
		Attempts: record.Attempt, SafeError: record.SafeError, Usage: record.Usage, Timings: record.Timings,
	}
	if record.ServiceAccount != nil {
		summary.ServiceAccount = *record.ServiceAccount
	}
	if record.Model != nil {
		summary.Model = *record.Model
	}
	if record.ProviderID != "" {
		summary.Provider = &ProviderSnapshot{ID: record.ProviderID, Name: record.ProviderName, UpstreamModel: record.UpstreamModel}
	}
	return summary
}

func (f QueryFilter) matches(summary RequestSummary) bool {
	if f.From != nil && summary.StartedAt.Before(*f.From) {
		return false
	}
	if f.To != nil && !summary.StartedAt.Before(*f.To) {
		return false
	}
	if f.AccountID != "" && summary.ServiceAccount.ID != f.AccountID {
		return false
	}
	if f.ModelID != "" && summary.Model.ID != f.ModelID {
		return false
	}
	if f.ProviderID != "" && (summary.Provider == nil || summary.Provider.ID != f.ProviderID) {
		return false
	}
	if f.UpstreamModel != "" && (summary.Provider == nil || summary.Provider.UpstreamModel != f.UpstreamModel) {
		return false
	}
	if f.Outcome != "" && summary.Outcome != f.Outcome {
		return false
	}
	return true
}

type PageCursor struct {
	StartedAt time.Time `json:"startedAt"`
	RequestID string    `json:"requestId"`
}

func EncodePageCursor(cursor PageCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func DecodePageCursor(value string) (PageCursor, error) {
	if value == "" {
		return PageCursor{}, nil
	}
	if len(value) > 1400 {
		return PageCursor{}, ErrInvalidQuery
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) > 1024 {
		return PageCursor{}, ErrInvalidQuery
	}
	var cursor PageCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.StartedAt.IsZero() || cursor.RequestID == "" || !validRequestID(cursor.RequestID) {
		return PageCursor{}, ErrInvalidQuery
	}
	return cursor, nil
}

func afterCursor(summary RequestSummary, cursor PageCursor) bool {
	if cursor.StartedAt.IsZero() {
		return true
	}
	if summary.StartedAt.Before(cursor.StartedAt) {
		return true
	}
	return summary.StartedAt.Equal(cursor.StartedAt) && summary.RequestID > cursor.RequestID
}

func summaryLess(a, b RequestSummary) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.After(b.StartedAt)
	}
	return a.RequestID < b.RequestID
}

func insertSummary(items []RequestSummary, item RequestSummary, capacity int) []RequestSummary {
	for _, existing := range items {
		if existing.RequestID == item.RequestID {
			return items
		}
	}
	position := sort.Search(len(items), func(index int) bool { return !summaryLess(items[index], item) })
	if position >= capacity {
		return items
	}
	items = append(items, RequestSummary{})
	copy(items[position+1:], items[position:])
	items[position] = item
	if len(items) > capacity {
		items = items[:capacity]
	}
	return items
}

type RequestPage struct {
	Requests   []RequestSummary `json:"requests"`
	NextCursor string           `json:"nextCursor,omitempty"`
	HasMore    bool             `json:"hasMore"`
}

func (q *QueryService) List(ctx context.Context, filter QueryFilter, limit int, cursor PageCursor) (RequestPage, error) {
	if limit < 1 || limit > MaxPageSize {
		return RequestPage{}, ErrInvalidQuery
	}
	ctx, cancel := context.WithTimeout(ctx, MaxQueryDuration)
	defer cancel()
	snapshot, err := q.recorder.querySnapshot(ctx)
	if err != nil {
		return RequestPage{}, err
	}
	defer snapshot.Close()
	items := make([]RequestSummary, 0, limit+1)
	collect := func(summary RequestSummary) error {
		if !filter.matches(summary) || !afterCursor(summary, cursor) {
			return nil
		}
		items = insertSummary(items, summary, limit+1)
		return nil
	}
	if err := scanSummaries(ctx, snapshot, filter, collect); err != nil {
		return RequestPage{}, err
	}
	if filter.Outcome == "" || filter.Outcome == "active" {
		for _, summary := range snapshot.activeRequests {
			if err := collect(summary); err != nil {
				return RequestPage{}, err
			}
		}
	}
	page := RequestPage{HasMore: len(items) > limit}
	if page.HasMore {
		items = items[:limit]
		last := items[len(items)-1]
		page.NextCursor = EncodePageCursor(PageCursor{StartedAt: last.StartedAt, RequestID: last.RequestID})
	}
	page.Requests = items
	return page, nil
}

type startCandidate struct {
	summary         RequestSummary
	ended           bool
	started         bool
	transmissions   int
	providerAttempt int
	providerElapsed int64
}

func startSummary(record Record) RequestSummary {
	summary := RequestSummary{RequestID: record.RequestID, StartedAt: record.StartedAt, DurationNS: record.ElapsedNS, Outcome: "interrupted", Interrupted: true}
	if record.ServiceAccount != nil {
		summary.ServiceAccount = *record.ServiceAccount
	}
	if record.Model != nil {
		summary.Model = *record.Model
	}
	return summary
}

func scanSummaries(ctx context.Context, snapshot *QuerySnapshot, filter QueryFilter, visit func(RequestSummary) error) (returnErr error) {
	needsInterrupted := filter.Outcome == "" || filter.Outcome == "interrupted"
	var completedIDs *completedIDIndex
	if needsInterrupted {
		completedIDs = newCompletedIDIndex()
		defer func() {
			if err := completedIDs.close(); err != nil {
				returnErr = errors.Join(returnErr, queryIndexFailure("close or remove query index", err))
			}
		}()
	}
	legacy := make(map[string]RequestSummary, MaxQueryBatch)
	flushLegacy := func() error {
		if len(legacy) == 0 {
			return nil
		}
		pending := legacy
		legacy = make(map[string]RequestSummary, MaxQueryBatch)
		return resolveLegacyEnds(ctx, snapshot, pending, visit)
	}
	if err := snapshot.scan(ctx, func(record Record) error {
		if record.Type != "request_end" {
			return nil
		}
		if record.SchemaVersion >= 2 {
			if completedIDs != nil {
				if err := completedIDs.add(ctx, record.RequestID); err != nil {
					return queryIndexFailure("collect completed request IDs", err)
				}
			}
			return visit(summaryFromEnd(record))
		}
		legacy[record.RequestID] = summaryFromEnd(record)
		if len(legacy) >= MaxQueryBatch {
			return flushLegacy()
		}
		return nil
	}); err != nil {
		return err
	}
	if err := flushLegacy(); err != nil {
		return err
	}

	if !needsInterrupted {
		return nil
	}
	if err := completedIDs.finalize(ctx); err != nil {
		return queryIndexFailure("finalize completed request ID index", err)
	}
	batch := make(map[string]*startCandidate, MaxQueryBatch)
	flushStarts := func() error {
		if len(batch) == 0 {
			return nil
		}
		pending := batch
		batch = make(map[string]*startCandidate, MaxQueryBatch)
		if err := resolveStartBatch(ctx, snapshot, pending); err != nil {
			return err
		}
		for _, candidate := range pending {
			if candidate.ended || !candidate.started {
				continue
			}
			candidate.summary.Attempts = candidate.transmissions
			if candidate.summary.Attempts == 0 && candidate.summary.Provider != nil {
				candidate.summary.Attempts = 1
			}
			if err := visit(candidate.summary); err != nil {
				return err
			}
		}
		return nil
	}
	if err := snapshot.scan(ctx, func(record Record) error {
		if record.Type != "request_start" {
			return nil
		}
		if _, active := snapshot.activeRequests[record.RequestID]; active {
			return nil
		}
		summary := startSummary(record)
		if filter.From != nil && summary.StartedAt.Before(*filter.From) || filter.To != nil && !summary.StartedAt.Before(*filter.To) ||
			filter.AccountID != "" && summary.ServiceAccount.ID != filter.AccountID || filter.ModelID != "" && summary.Model.ID != filter.ModelID {
			return nil
		}
		ended, err := completedIDs.contains(ctx, record.RequestID)
		if err != nil {
			return queryIndexFailure("probe completed request ID index", err)
		}
		if ended {
			return nil
		}
		if _, exists := batch[record.RequestID]; !exists {
			batch[record.RequestID] = &startCandidate{summary: summary}
		}
		if len(batch) >= MaxQueryBatch {
			return flushStarts()
		}
		return nil
	}); err != nil {
		return err
	}
	return flushStarts()
}

func queryIndexFailure(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrQueryIndexUnavailable, action, err)
}

func resolveLegacyEnds(ctx context.Context, snapshot *QuerySnapshot, pending map[string]RequestSummary, visit func(RequestSummary) error) error {
	info := make(map[string]*startCandidate, len(pending))
	for id, summary := range pending {
		copy := summary
		info[id] = &startCandidate{summary: copy}
	}
	err := snapshot.scan(ctx, func(record Record) error {
		candidate := info[record.RequestID]
		if candidate == nil {
			return nil
		}
		switch record.Type {
		case "request_start":
			candidate.started = true
			if record.ElapsedNS > candidate.summary.DurationNS {
				candidate.summary.DurationNS = record.ElapsedNS
			}
			if record.ServiceAccount != nil {
				candidate.summary.ServiceAccount = *record.ServiceAccount
			}
			if record.Model != nil {
				candidate.summary.Model = *record.Model
			}
		case "upstream_transmission":
			candidate.transmissions++
			if record.Attempt > candidate.providerAttempt || record.Attempt == candidate.providerAttempt && record.ElapsedNS >= candidate.providerElapsed {
				candidate.providerAttempt, candidate.providerElapsed = record.Attempt, record.ElapsedNS
				candidate.summary.Provider = &ProviderSnapshot{ID: record.ProviderID, Name: record.ProviderName, UpstreamModel: record.UpstreamModel}
			}
			if record.ElapsedNS > candidate.summary.DurationNS {
				candidate.summary.DurationNS = record.ElapsedNS
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for id, candidate := range info {
		if !candidate.started {
			return fmt.Errorf("schema 1 request_end %s has no request_start", id)
		}
		candidate.summary.Attempts = candidate.transmissions
		if candidate.summary.Attempts == 0 {
			candidate.summary.Attempts = pending[id].Attempts
		}
		if err := visit(candidate.summary); err != nil {
			return err
		}
	}
	return nil
}

func resolveStartBatch(ctx context.Context, snapshot *QuerySnapshot, pending map[string]*startCandidate) error {
	return snapshot.scan(ctx, func(record Record) error {
		candidate := pending[record.RequestID]
		if candidate == nil {
			return nil
		}
		if record.ElapsedNS > candidate.summary.DurationNS {
			candidate.summary.DurationNS = record.ElapsedNS
		}
		switch record.Type {
		case "request_start":
			candidate.started = true
		case "upstream_transmission":
			candidate.transmissions++
			if record.Attempt > candidate.providerAttempt || record.Attempt == candidate.providerAttempt && record.ElapsedNS >= candidate.providerElapsed {
				candidate.providerAttempt, candidate.providerElapsed = record.Attempt, record.ElapsedNS
				candidate.summary.Provider = &ProviderSnapshot{ID: record.ProviderID, Name: record.ProviderName, UpstreamModel: record.UpstreamModel}
			}
		case "request_end":
			candidate.ended = true
		}
		return nil
	})
}

// RequestDetail holds bounded metadata. Request/effective/HTTP/event bytes are
// retrieved through Content in chunks so large inputs never inflate this response.
type RequestDetail struct {
	Request            RequestSummary   `json:"request"`
	IncomingBytes      int64            `json:"incomingRequestBytes,omitempty"`
	IncomingURL        string           `json:"incomingRequestUrl,omitempty"`
	Attempts           []AttemptDetail  `json:"attempts"`
	AttemptCount       int              `json:"attemptCount"`
	AttemptsTruncated  bool             `json:"attemptsTruncated"`
	Responses          []ResponseDetail `json:"upstreamResponses"`
	ResponseCount      int              `json:"upstreamResponseCount"`
	ResponsesTruncated bool             `json:"responsesTruncated"`
	EventCount         uint64           `json:"eventCount"`
	EventBytes         uint64           `json:"eventBytes"`
	EventsURL          string           `json:"eventsUrl"`
}

type AttemptDetail struct {
	Number         int              `json:"number"`
	Provider       ProviderSnapshot `json:"provider"`
	EffectiveBytes int64            `json:"effectiveRequestBytes"`
	EffectiveURL   string           `json:"effectiveRequestUrl"`
}

type ResponseDetail struct {
	Attempt           int    `json:"attempt"`
	HTTPStatus        int    `json:"httpStatus"`
	UpstreamRequestID string `json:"upstreamRequestId,omitempty"`
	BodyBytes         int64  `json:"bodyBytes"`
	BodyTruncated     bool   `json:"bodyTruncated"`
	BodyReadFailed    bool   `json:"bodyReadFailed"`
	BodyURL           string `json:"bodyUrl"`
}

func (q *QueryService) Detail(ctx context.Context, requestID string) (RequestDetail, error) {
	if !validRequestID(requestID) {
		return RequestDetail{}, ErrInvalidQuery
	}
	ctx, cancel := context.WithTimeout(ctx, MaxQueryDuration)
	defer cancel()
	snapshot, err := q.recorder.querySnapshot(ctx)
	if err != nil {
		return RequestDetail{}, err
	}
	defer snapshot.Close()
	detail := RequestDetail{Attempts: make([]AttemptDetail, 0, 8), Responses: make([]ResponseDetail, 0, 4), EventsURL: "/api/requests/" + requestID + "/events"}
	found := false
	var start *RequestSummary
	var final *RequestSummary
	var finalSchema int
	var finalElapsed int64
	var lastProvider *ProviderSnapshot
	var lastProviderAttempt int
	var lastProviderElapsed int64
	var lastObservedElapsed int64
	attempts := make(map[int]AttemptDetail, 8)
	err = snapshot.scan(ctx, func(record Record) error {
		if record.RequestID != requestID {
			return nil
		}
		found = true
		switch record.Type {
		case "request_start":
			if start == nil {
				value := startSummary(record)
				start = &value
				detail.IncomingBytes = int64(len(record.IncomingRequest))
				detail.IncomingURL = "/api/requests/" + requestID + "/body?kind=incoming"
			}
		case "upstream_transmission":
			detail.AttemptCount++
			if _, exists := attempts[record.Attempt]; !exists && len(attempts) < MaxDetailAttempts {
				attempts[record.Attempt] = AttemptDetail{Number: record.Attempt,
					Provider:       ProviderSnapshot{ID: record.ProviderID, Name: record.ProviderName, UpstreamModel: record.UpstreamModel},
					EffectiveBytes: int64(len(record.EffectiveRequest)),
					EffectiveURL:   fmt.Sprintf("/api/requests/%s/body?kind=effective&attempt=%d", requestID, record.Attempt)}
			} else if len(attempts) >= MaxDetailAttempts {
				detail.AttemptsTruncated = true
			}
			if record.Attempt > lastProviderAttempt || record.Attempt == lastProviderAttempt && record.ElapsedNS >= lastProviderElapsed {
				lastProviderAttempt, lastProviderElapsed = record.Attempt, record.ElapsedNS
				lastProvider = &ProviderSnapshot{ID: record.ProviderID, Name: record.ProviderName, UpstreamModel: record.UpstreamModel}
			}
		case "upstream_response":
			detail.ResponseCount++
			if len(detail.Responses) < MaxDetailResponses {
				detail.Responses = append(detail.Responses, ResponseDetail{Attempt: record.Attempt, HTTPStatus: record.HTTPStatus,
					UpstreamRequestID: record.UpstreamRequestID, BodyBytes: int64(len(record.UpstreamResponseBody)),
					BodyTruncated: record.UpstreamResponseBodyTruncated, BodyReadFailed: record.UpstreamResponseBodyReadFailed,
					BodyURL: fmt.Sprintf("/api/requests/%s/body?kind=http_response&attempt=%d", requestID, record.Attempt)})
			} else {
				detail.ResponsesTruncated = true
			}
		case "response_event":
			detail.EventCount++
			detail.EventBytes += uint64(len(record.WireText))
		case "request_end":
			if final == nil || record.ElapsedNS >= finalElapsed {
				end := summaryFromEnd(record)
				final = &end
				finalSchema, finalElapsed = record.SchemaVersion, record.ElapsedNS
			}
		}
		if record.ElapsedNS > lastObservedElapsed {
			lastObservedElapsed = record.ElapsedNS
		}
		return nil
	})
	if err != nil {
		return RequestDetail{}, err
	}
	if !found {
		return RequestDetail{}, ErrQueryNotFound
	}
	if final != nil {
		detail.Request = *final
		if finalSchema == 1 && start != nil {
			detail.Request.ServiceAccount = start.ServiceAccount
			detail.Request.Model = start.Model
			detail.Request.Provider = lastProvider
			detail.Request.Attempts = detail.AttemptCount
		}
	} else if start != nil {
		detail.Request = *start
		detail.Request.Outcome, detail.Request.Interrupted = "interrupted", true
		detail.Request.Provider = lastProvider
		detail.Request.Attempts = detail.AttemptCount
	}
	if active, ok := snapshot.activeRequests[requestID]; ok {
		detail.Request = active
	}
	if lastObservedElapsed > detail.Request.DurationNS {
		detail.Request.DurationNS = lastObservedElapsed
	}
	for _, attempt := range attempts {
		detail.Attempts = append(detail.Attempts, attempt)
	}
	sort.Slice(detail.Attempts, func(i, j int) bool { return detail.Attempts[i].Number < detail.Attempts[j].Number })
	sort.Slice(detail.Responses, func(i, j int) bool { return detail.Responses[i].Attempt < detail.Responses[j].Attempt })
	return detail, nil
}

// EventPage is count and raw-byte bounded. Oversized or byte-budget-omitted frames
// remain available from the single-event content endpoint.
type EventPage struct {
	Events     []EventItem `json:"events"`
	NextCursor uint64      `json:"nextCursor,omitempty"`
	HasMore    bool        `json:"hasMore"`
}

type EventItem struct {
	Sequence          uint64 `json:"sequence"`
	ElapsedNS         int64  `json:"elapsedNs"`
	Attempt           int    `json:"attempt"`
	EventName         string `json:"eventName,omitempty"`
	EventNameBytes    int    `json:"eventNameBytes"`
	EventNameTruncated bool  `json:"eventNameTruncated"`
	RawBytes          int    `json:"rawBytes"`
	RawBase64         string `json:"rawBase64,omitempty"`
	RawOmitted        string `json:"rawOmitted,omitempty"`
	RawURL            string `json:"rawUrl"`
}

func eventLess(a, b EventItem) bool {
	if a.Sequence != b.Sequence {
		return a.Sequence < b.Sequence
	}
	if a.ElapsedNS != b.ElapsedNS {
		return a.ElapsedNS < b.ElapsedNS
	}
	return a.Attempt < b.Attempt
}

func (q *QueryService) Events(ctx context.Context, requestID string, limit int, cursor uint64) (EventPage, error) {
	if !validRequestID(requestID) || limit < 1 || limit > MaxPageSize {
		return EventPage{}, ErrInvalidQuery
	}
	ctx, cancel := context.WithTimeout(ctx, MaxQueryDuration)
	defer cancel()
	snapshot, err := q.recorder.querySnapshot(ctx)
	if err != nil {
		return EventPage{}, err
	}
	defer snapshot.Close()
	items := make([]EventItem, 0, limit+1)
	requestFound := false
	if err := snapshot.scan(ctx, func(record Record) error {
		if record.RequestID != requestID {
			return nil
		}
		requestFound = true
		if record.Type != "response_event" || record.Sequence <= cursor {
			return nil
		}
		namePreview, nameTruncated := eventNamePreview(record.EventName)
		item := EventItem{Sequence: record.Sequence, ElapsedNS: record.ElapsedNS, Attempt: record.Attempt,
			EventName: namePreview, EventNameBytes: len(record.EventName), EventNameTruncated: nameTruncated,
			RawBytes: len(record.WireText), RawURL: fmt.Sprintf("/api/requests/%s/events/%d", requestID, record.Sequence)}
		position := sort.Search(len(items), func(i int) bool { return !eventLess(items[i], item) })
		if position >= limit+1 {
			return nil
		}
		items = append(items, EventItem{})
		copy(items[position+1:], items[position:])
		items[position] = item
		if len(items) > limit+1 {
			items = items[:limit+1]
		}
		return nil
	}); err != nil {
		return EventPage{}, err
	}
	if !requestFound {
		return EventPage{}, ErrQueryNotFound
	}
	page := EventPage{HasMore: len(items) > limit}
	if page.HasMore {
		items = items[:limit]
	}
	selected := make(map[uint64]int, len(items))
	remaining := MaxEventPageRawBytes
	for i := range items {
		if items[i].RawBytes > remaining {
			if items[i].RawBytes > MaxEventPageRawBytes {
				items[i].RawOmitted = "event_exceeds_page_byte_limit"
			} else {
				items[i].RawOmitted = "page_byte_limit"
			}
			continue
		}
		remaining -= items[i].RawBytes
		selected[items[i].Sequence] = i
	}
	if len(selected) > 0 {
		if err := snapshot.scan(ctx, func(record Record) error {
			if record.RequestID != requestID || record.Type != "response_event" {
				return nil
			}
			index, ok := selected[record.Sequence]
			if !ok {
				return nil
			}
			items[index].RawBase64 = base64.StdEncoding.EncodeToString([]byte(record.WireText))
			return nil
		}); err != nil {
			return EventPage{}, err
		}
	}
	if len(items) > 0 {
		page.NextCursor = items[len(items)-1].Sequence
	}
	page.Events = items
	return page, nil
}

func eventNamePreview(value string) (string, bool) {
	const previewLimit = 1024
	if len(value) <= previewLimit {
		return value, false
	}
	prefix := value[:previewLimit]
	for len(prefix) > 0 && !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix, true
}

type ContentRequest struct {
	Kind     string
	Attempt  int
	Sequence uint64
	Offset   int64
	Limit    int
}

type ContentChunk struct {
	Data       []byte
	Offset     int64
	TotalBytes int64
	Complete   bool
	Truncated  bool
	ReadFailed bool
}

func (q *QueryService) Content(ctx context.Context, requestID string, requested ContentRequest) (ContentChunk, error) {
	if !validRequestID(requestID) || requested.Offset < 0 || requested.Limit < 1 || requested.Limit > MaxContentChunkBytes || requested.Attempt < 0 ||
		(requested.Kind != "incoming" && requested.Kind != "effective" && requested.Kind != "http_response" && requested.Kind != "event") ||
		(requested.Kind == "effective" || requested.Kind == "http_response") && requested.Attempt < 1 || requested.Kind == "event" && requested.Sequence < 1 {
		return ContentChunk{}, ErrInvalidQuery
	}
	ctx, cancel := context.WithTimeout(ctx, MaxQueryDuration)
	defer cancel()
	snapshot, err := q.recorder.querySnapshot(ctx)
	if err != nil {
		return ContentChunk{}, err
	}
	defer snapshot.Close()
	var chunk ContentChunk
	found := false
	err = snapshot.scan(ctx, func(record Record) error {
		if record.RequestID != requestID {
			return nil
		}
		var data []byte
		var truncated, readFailed bool
		match := false
		switch requested.Kind {
		case "incoming":
			match = record.Type == "request_start"
			data = record.IncomingRequest
		case "effective":
			match = record.Type == "upstream_transmission" && record.Attempt == requested.Attempt
			data = record.EffectiveRequest
		case "http_response":
			match = record.Type == "upstream_response" && record.Attempt == requested.Attempt
			data = record.UpstreamResponseBody
			truncated, readFailed = record.UpstreamResponseBodyTruncated, record.UpstreamResponseBodyReadFailed
		case "event":
			match = record.Type == "response_event" && record.Sequence == requested.Sequence
			data = []byte(record.WireText)
		}
		if !match {
			return nil
		}
		if found {
			return errors.New("history content key is duplicated")
		}
		found = true
		if requested.Offset > int64(len(data)) {
			return ErrInvalidQuery
		}
		end := requested.Offset + int64(requested.Limit)
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		chunk = ContentChunk{Data: append([]byte(nil), data[requested.Offset:end]...), Offset: requested.Offset,
			TotalBytes: int64(len(data)), Complete: end == int64(len(data)), Truncated: truncated, ReadFailed: readFailed}
		return nil
	})
	if err != nil {
		return ContentChunk{}, err
	}
	if !found {
		return ContentChunk{}, ErrQueryNotFound
	}
	return chunk, nil
}

// UsageCount keeps known totals and unknown/known request counts distinct. A
// pointer to zero is known zero; nil is unknown.
type UsageCount struct {
	Total           int64 `json:"total"`
	KnownRequests   int64 `json:"knownRequests"`
	UnknownRequests int64 `json:"unknownRequests"`
}

type UsageGroup struct {
	AccountID         string           `json:"accountId,omitempty"`
	AccountName       string           `json:"accountName,omitempty"`
	ModelID           string           `json:"modelId,omitempty"`
	ModelName         string           `json:"modelName,omitempty"`
	ProviderID        string           `json:"providerId,omitempty"`
	ProviderName      string           `json:"providerName,omitempty"`
	UpstreamModel     string           `json:"upstreamModel,omitempty"`
	ClientRequests    int64            `json:"clientRequests"`
	UpstreamAttempts  int64            `json:"upstreamAttempts"`
	Outcomes          map[string]int64 `json:"outcomes"`
	InputTokens       UsageCount       `json:"inputTokens"`
	OutputTokens      UsageCount       `json:"outputTokens"`
	CachedInputTokens UsageCount       `json:"cachedInputTokens"`
	ReasoningTokens   UsageCount       `json:"reasoningOutputTokens"`
	CacheRatio        *float64         `json:"cacheRatio"`
	CacheRatioUnavailableReason string `json:"cacheRatioUnavailableReason,omitempty"`
	cacheEligible     int64
	cacheUnavailable  int64
	cacheSubsetInvalid int64
}

type UsageReport struct {
	GroupBy []string     `json:"groupBy"`
	Groups  []UsageGroup `json:"groups"`
}

func (q *QueryService) Usage(ctx context.Context, filter QueryFilter, groupBy []string) (UsageReport, error) {
	validDimensions := map[string]bool{"account": true, "model": true, "provider": true, "upstream_model": true}
	seen := make(map[string]bool, len(groupBy))
	for _, dimension := range groupBy {
		if !validDimensions[dimension] || seen[dimension] {
			return UsageReport{}, ErrInvalidQuery
		}
		seen[dimension] = true
	}
	ctx, cancel := context.WithTimeout(ctx, MaxQueryDuration)
	defer cancel()
	snapshot, err := q.recorder.querySnapshot(ctx)
	if err != nil {
		return UsageReport{}, err
	}
	defer snapshot.Close()
	groups := make(map[string]*UsageGroup)
	add := func(summary RequestSummary) error {
		if !filter.matches(summary) {
			return nil
		}
		keyValue := struct {
			AccountID     string `json:"accountId,omitempty"`
			ModelID       string `json:"modelId,omitempty"`
			ProviderID    string `json:"providerId,omitempty"`
			UpstreamModel string `json:"upstreamModel,omitempty"`
		}{}
		for _, dimension := range groupBy {
			switch dimension {
			case "account":
				keyValue.AccountID = summary.ServiceAccount.ID
			case "model":
				keyValue.ModelID = summary.Model.ID
			case "provider":
				if summary.Provider != nil {
					keyValue.ProviderID = summary.Provider.ID
				}
			case "upstream_model":
				if summary.Provider != nil {
					keyValue.UpstreamModel = summary.Provider.UpstreamModel
				}
			}
		}
		encoded, _ := json.Marshal(keyValue)
		key := string(encoded)
		group := groups[key]
		if group == nil {
			if len(groups) >= MaxAggregateGroups {
				return ErrTooManyGroups
			}
			group = &UsageGroup{Outcomes: make(map[string]int64)}
			if contains(groupBy, "account") {
				group.AccountID, group.AccountName = summary.ServiceAccount.ID, summary.ServiceAccount.Name
			}
			if contains(groupBy, "model") {
				group.ModelID, group.ModelName = summary.Model.ID, summary.Model.Name
			}
			if contains(groupBy, "provider") && summary.Provider != nil {
				group.ProviderID, group.ProviderName = summary.Provider.ID, summary.Provider.Name
			}
			if contains(groupBy, "upstream_model") && summary.Provider != nil {
				group.UpstreamModel = summary.Provider.UpstreamModel
			}
			groups[key] = group
		}
		if group.ClientRequests == math.MaxInt64 || int64(summary.Attempts) > math.MaxInt64-group.UpstreamAttempts {
			return ErrAggregateOverflow
		}
		group.ClientRequests++
		group.UpstreamAttempts += int64(summary.Attempts)
		if group.Outcomes[summary.Outcome] == math.MaxInt64 {
			return ErrAggregateOverflow
		}
		group.Outcomes[summary.Outcome]++
		addUsageCount := func(target *UsageCount, value *int64) error {
			if value == nil {
				if target.UnknownRequests == math.MaxInt64 {
					return ErrAggregateOverflow
				}
				target.UnknownRequests++
				return nil
			}
			if *value > math.MaxInt64-target.Total || target.KnownRequests == math.MaxInt64 {
				return ErrAggregateOverflow
			}
			target.Total += *value
			target.KnownRequests++
			return nil
		}
		var usage *Usage
		if summary.Usage != nil {
			usage = summary.Usage
		}
		var input, output, cached, reasoning *int64
		if usage != nil {
			input, output, cached, reasoning = usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.ReasoningOutputTokens
		}
		if err := addUsageCount(&group.InputTokens, input); err != nil {
			return err
		}
		if err := addUsageCount(&group.OutputTokens, output); err != nil {
			return err
		}
		if err := addUsageCount(&group.CachedInputTokens, cached); err != nil {
			return err
		}
		if err := addUsageCount(&group.ReasoningTokens, reasoning); err != nil {
			return err
		}
		if input != nil && cached != nil {
			group.cacheEligible++
			if *cached > *input {
				group.cacheSubsetInvalid++
			}
		} else {
			group.cacheUnavailable++
		}
		return nil
	}
	if err := scanSummaries(ctx, snapshot, filter, add); err != nil {
		return UsageReport{}, err
	}
	if filter.Outcome == "" || filter.Outcome == "active" {
		for _, summary := range snapshot.activeRequests {
			if err := add(summary); err != nil {
				return UsageReport{}, err
			}
		}
	}
	result := UsageReport{GroupBy: append([]string(nil), groupBy...), Groups: make([]UsageGroup, 0, len(groups))}
	for _, group := range groups {
		if group.cacheUnavailable > 0 {
			group.CacheRatioUnavailableReason = "input_or_cached_count_unknown"
		} else if group.InputTokens.Total == 0 {
			group.CacheRatioUnavailableReason = "input_tokens_zero"
		} else if group.cacheSubsetInvalid > 0 {
			group.CacheRatioUnavailableReason = "cached_tokens_exceed_input"
		} else if group.cacheEligible > 0 {
			ratio := float64(group.CachedInputTokens.Total) / float64(group.InputTokens.Total)
			group.CacheRatio = &ratio
		}
		result.Groups = append(result.Groups, *group)
	}
	sort.Slice(result.Groups, func(i, j int) bool {
		a, _ := json.Marshal(result.Groups[i])
		b, _ := json.Marshal(result.Groups[j])
		return strings.Compare(string(a), string(b)) < 0
	})
	return result, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
