package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/gyemoim/internal/history"
)

func (api *managementAPI) requestList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	filter, ok := parseHistoryFilter(w, r)
	if !ok {
		return
	}
	limit := history.DefaultPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > history.MaxPageSize {
			writeManagementError(w, http.StatusBadRequest, "limit must be between 1 and 100", "invalid_history_query")
			return
		}
		limit = value
	}
	cursor, err := history.DecodePageCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeManagementError(w, http.StatusBadRequest, "cursor is invalid", "invalid_history_query")
		return
	}
	page, err := api.history.List(r.Context(), filter, limit, cursor)
	if err != nil {
		writeHistoryFailure(w, err)
		return
	}
	writeBoundedHistoryJSON(w, http.StatusOK, page)
}

func (api *managementAPI) requestDetail(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	detail, err := api.history.Detail(r.Context(), id)
	if err != nil {
		writeHistoryFailure(w, err)
		return
	}
	writeBoundedHistoryJSON(w, http.StatusOK, detail)
}

func (api *managementAPI) requestEvents(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	limit := history.DefaultPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > history.MaxPageSize {
			writeManagementError(w, http.StatusBadRequest, "limit must be between 1 and 100", "invalid_history_query")
			return
		}
		limit = value
	}
	var cursor uint64
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeManagementError(w, http.StatusBadRequest, "event cursor must be a non-negative sequence number", "invalid_history_query")
			return
		}
		cursor = value
	}
	page, err := api.history.Events(r.Context(), id, limit, cursor)
	if err != nil {
		writeHistoryFailure(w, err)
		return
	}
	writeBoundedHistoryJSON(w, http.StatusOK, page)
}

func (api *managementAPI) requestEventContent(w http.ResponseWriter, r *http.Request, id, sequenceText string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil || sequence == 0 {
		writeManagementError(w, http.StatusBadRequest, "event sequence is invalid", "invalid_history_query")
		return
	}
	content := history.ContentRequest{Kind: "event", Sequence: sequence, Limit: 256 << 10}
	if !parseContentPaging(w, r, &content) {
		return
	}
	api.writeHistoryContent(w, r, id, content)
}

func (api *managementAPI) requestContent(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	query := r.URL.Query()
	content := history.ContentRequest{Kind: query.Get("kind"), Limit: 256 << 10}
	if raw := query.Get("attempt"); raw != "" {
		attempt, err := strconv.Atoi(raw)
		if err != nil || attempt < 1 {
			writeManagementError(w, http.StatusBadRequest, "attempt is invalid", "invalid_history_query")
			return
		}
		content.Attempt = attempt
	}
	if raw := query.Get("sequence"); raw != "" {
		sequence, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || sequence == 0 {
			writeManagementError(w, http.StatusBadRequest, "sequence is invalid", "invalid_history_query")
			return
		}
		content.Sequence = sequence
	}
	if !parseContentPaging(w, r, &content) {
		return
	}
	api.writeHistoryContent(w, r, id, content)
}

func parseContentPaging(w http.ResponseWriter, r *http.Request, content *history.ContentRequest) bool {
	query := r.URL.Query()
	if raw := query.Get("offset"); raw != "" {
		offset, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 {
			writeManagementError(w, http.StatusBadRequest, "offset must be a non-negative byte position", "invalid_history_query")
			return false
		}
		content.Offset = offset
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > history.MaxContentChunkBytes {
			writeManagementError(w, http.StatusBadRequest, "limit must be between 1 and 1048576 bytes", "invalid_history_query")
			return false
		}
		content.Limit = limit
	}
	return true
}

func (api *managementAPI) writeHistoryContent(w http.ResponseWriter, r *http.Request, id string, content history.ContentRequest) {
	if content.Limit == 0 {
		content.Limit = 256 << 10
	}
	chunk, err := api.history.Content(r.Context(), id, content)
	if err != nil {
		writeHistoryFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Offset", strconv.FormatInt(chunk.Offset, 10))
	w.Header().Set("X-Content-Total-Bytes", strconv.FormatInt(chunk.TotalBytes, 10))
	w.Header().Set("X-Content-Complete", strconv.FormatBool(chunk.Complete))
	w.Header().Set("X-Content-Truncated", strconv.FormatBool(chunk.Truncated))
	w.Header().Set("X-Content-Read-Failed", strconv.FormatBool(chunk.ReadFailed))
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk.Data)))
	w.Header().Set("Cache-Control", "no-store")
	_ = writeBoundedResponse(http.NewResponseController(w), w, http.StatusOK, "application/octet-stream", chunk.Data)
}

func (api *managementAPI) usage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	filter, ok := parseHistoryFilter(w, r)
	if !ok {
		return
	}
	var groupBy []string
	if raw := r.URL.Query().Get("group_by"); raw != "" {
		groupBy = strings.Split(raw, ",")
		for i := range groupBy {
			groupBy[i] = strings.TrimSpace(groupBy[i])
			if groupBy[i] == "" {
				writeManagementError(w, http.StatusBadRequest, "group_by must contain account, model, provider, or upstream_model", "invalid_history_query")
				return
			}
		}
	}
	report, err := api.history.Usage(r.Context(), filter, groupBy)
	if err != nil {
		writeHistoryFailure(w, err)
		return
	}
	writeBoundedHistoryJSON(w, http.StatusOK, report)
}

func parseHistoryFilter(w http.ResponseWriter, r *http.Request) (history.QueryFilter, bool) {
	query := r.URL.Query()
	filter := history.QueryFilter{
		AccountID: query.Get("account_id"), ModelID: query.Get("model_id"),
		ProviderID: query.Get("provider_id"), UpstreamModel: query.Get("upstream_model"), Outcome: query.Get("outcome"),
	}
	for _, field := range []struct{ name, value string }{
		{name: "account_id", value: filter.AccountID}, {name: "model_id", value: filter.ModelID},
		{name: "provider_id", value: filter.ProviderID}, {name: "upstream_model", value: filter.UpstreamModel},
	} {
		if strings.TrimSpace(field.value) != field.value || len(field.value) > 256 {
			writeManagementError(w, http.StatusBadRequest, field.name+" is invalid", "invalid_history_query")
			return history.QueryFilter{}, false
		}
	}
	switch filter.Outcome {
	case "", "completed", "failed", "cancelled", "incomplete", "interrupted", "active":
	default:
		writeManagementError(w, http.StatusBadRequest, "outcome is invalid", "invalid_history_query")
		return history.QueryFilter{}, false
	}
	var ok bool
	if filter.From, ok = parseHistoryTime(w, query.Get("from"), "from"); !ok {
		return history.QueryFilter{}, false
	}
	if filter.To, ok = parseHistoryTime(w, query.Get("to"), "to"); !ok {
		return history.QueryFilter{}, false
	}
	if filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To) {
		writeManagementError(w, http.StatusBadRequest, "from must be earlier than to", "invalid_history_query")
		return history.QueryFilter{}, false
	}
	return filter, true
}

func parseHistoryTime(w http.ResponseWriter, value, name string) (*time.Time, bool) {
	if value == "" {
		return nil, true
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		writeManagementError(w, http.StatusBadRequest, name+" must be a UTC RFC3339 timestamp", "invalid_history_query")
		return nil, false
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		writeManagementError(w, http.StatusBadRequest, name+" must be a UTC RFC3339 timestamp", "invalid_history_query")
		return nil, false
	}
	parsed = parsed.UTC()
	return &parsed, true
}

func writeHistoryFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, history.ErrMaintenance):
		writeManagementError(w, http.StatusServiceUnavailable, "history files are undergoing maintenance; retry shortly", "history_maintenance")
	case errors.Is(err, history.ErrQueryBusy):
		writeManagementError(w, http.StatusServiceUnavailable, "too many history queries are active; retry shortly", "history_query_busy")
	case errors.Is(err, history.ErrInvalidQuery):
		writeManagementError(w, http.StatusBadRequest, "history query parameters are invalid", "invalid_history_query")
	case errors.Is(err, history.ErrQueryNotFound):
		writeManagementError(w, http.StatusNotFound, "history request content was not found", "history_not_found")
	case errors.Is(err, history.ErrTooManyGroups):
		writeManagementError(w, http.StatusUnprocessableEntity, "history aggregate exceeds the 1000 group limit; narrow the date range or filters", "history_group_limit")
	case errors.Is(err, history.ErrAggregateOverflow):
		writeManagementError(w, http.StatusUnprocessableEntity, "history aggregate totals exceed supported limits", "history_aggregate_overflow")
	case errors.Is(err, context.DeadlineExceeded):
		writeManagementError(w, http.StatusGatewayTimeout, "history query exceeded its 30 second time limit; narrow the date range or filters", "history_query_timeout")
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, history.ErrQueryUnavailable):
		writeManagementError(w, http.StatusServiceUnavailable, "history storage is unavailable", "history_unavailable")
	default:
		message := "history records could not be read"
		if strings.Contains(err.Error(), "history file") || strings.Contains(err.Error(), "history line") || strings.Contains(err.Error(), "history record") {
			message = "history records are malformed or unreadable: " + err.Error()
		}
		writeManagementError(w, http.StatusInternalServerError, message, "history_read_failed")
	}
}

func writeBoundedHistoryJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, "history response could not be encoded", "history_response_failed")
		return
	}
	body = append(body, '\n')
	w.Header().Set("Cache-Control", "no-store")
	_ = writeBoundedResponse(http.NewResponseController(w), w, status, "application/json; charset=utf-8", body)
}
