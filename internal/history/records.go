package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxRecordBytes leaves room for JSON escaping of a 64 MiB UTF-8 SSE frame:
	// JSON can expand control characters to six bytes each (about 384 MiB).
	maxRecordBytes       = 512 << 20
	recordReadBufferSize = 64 << 10
)

// Record is the decoded union of supported schema-version-1 and version-2 history lines.
// Fields that do not apply to Type remain at their zero value. Raw request bodies
// and response frames belong only to this callback and should not be retained by
// readers that process an entire history.
type Record struct {
	SchemaVersion                  int
	Type                           string
	RequestID                      string
	StartedAt                      time.Time
	TimestampUTC                   time.Time
	ElapsedNS                      int64
	ServiceAccount                 *ServiceAccountSnapshot
	Model                          *ModelSnapshot
	IncomingRequest                json.RawMessage
	Attempt                        int
	ProviderID                     string
	ProviderName                   string
	UpstreamModel                  string
	EffectiveRequest               json.RawMessage
	UpstreamRequestID              string
	UpstreamContentType            string
	UpstreamResponseBody           []byte
	UpstreamResponseBodyTruncated  bool
	UpstreamResponseBodyReadFailed bool
	EventName                      string
	Sequence                       uint64
	WireText                       string
	Outcome                        string
	HTTPStatus                     int
	SafeError                      string
	Usage                          *Usage
	Timings                        Timings
}

// ReadRecords visits complete, supported schema-version-1 or version-2 lines in an NDJSON
// stream. It retains at most one record line (capped at 512 MiB) at a time and
// stops immediately if the context or callback reports an error. A non-newline
// terminated tail is an error; startup recovery handles that only for active.ndjson.
func ReadRecords(ctx context.Context, source io.Reader, visit func(Record) error) error {
	if source == nil {
		return errors.New("history reader is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if visit == nil {
		return errors.New("history record callback is nil")
	}
	_, err := walkRecords(ctx, source, false, visit)
	return err
}

type scanResult struct {
	FirstStartedAt time.Time
	Records        uint64
	TailBytes      int64
}

func walkRecords(ctx context.Context, source io.Reader, allowIncompleteTail bool, visit func(Record) error) (scanResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var result scanResult
	reader := bufio.NewReaderSize(source, recordReadBufferSize)
	var lineNumber uint64
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		line, terminated, err := readBoundedLine(ctx, reader)
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return result, fmt.Errorf("read history line %d: %w", lineNumber+1, err)
		}
		lineNumber++
		if !terminated {
			if allowIncompleteTail {
				result.TailBytes = int64(len(line))
				return result, nil
			}
			return result, fmt.Errorf("history line %d is missing its final newline", lineNumber)
		}
		record, err := decodeRecord(line)
		if err != nil {
			return result, fmt.Errorf("decode history line %d: %w", lineNumber, err)
		}
		if result.Records == 0 {
			result.FirstStartedAt = record.StartedAt
		}
		result.Records++
		if err := visit(record); err != nil {
			return result, fmt.Errorf("visit history line %d: %w", lineNumber, err)
		}
	}
}

func readBoundedLine(ctx context.Context, reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		fragment, err := reader.ReadSlice('\n')
		contentBytes := len(fragment)
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			contentBytes--
		}
		if contentBytes > maxRecordBytes-len(line) {
			return nil, false, fmt.Errorf("history record exceeds %d MiB", maxRecordBytes>>20)
		}
		line = append(line, fragment...)
		switch err {
		case nil:
			return line[:len(line)-1], true, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 {
				return nil, false, io.EOF
			}
			return line, false, nil
		default:
			return nil, false, err
		}
	}
}

func decodeRecord(line []byte) (Record, error) {
	if !utf8.Valid(line) {
		return Record{}, errors.New("history line is not UTF-8")
	}
	var envelope struct {
		SchemaVersion int       `json:"schema_version"`
		Type          string    `json:"type"`
		RequestID     string    `json:"request_id"`
		StartedAt     time.Time `json:"started_at"`
		TimestampUTC  time.Time `json:"timestamp_utc"`
		ElapsedNS     int64     `json:"elapsed_ns"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return Record{}, err
	}
	if envelope.SchemaVersion < minimumSchemaVersion || envelope.SchemaVersion > schemaVersion {
		return Record{}, fmt.Errorf("unsupported schema version %d", envelope.SchemaVersion)
	}
	if !validRequestID(envelope.RequestID) || envelope.StartedAt.IsZero() || envelope.TimestampUTC.IsZero() || envelope.ElapsedNS < 0 {
		return Record{}, errors.New("invalid common record fields")
	}
	if _, offset := envelope.StartedAt.Zone(); offset != 0 {
		return Record{}, errors.New("started_at is not UTC")
	}
	if _, offset := envelope.TimestampUTC.Zone(); offset != 0 {
		return Record{}, errors.New("timestamp_utc is not UTC")
	}
	record := Record{
		SchemaVersion: envelope.SchemaVersion,
		Type:          envelope.Type,
		RequestID:     envelope.RequestID,
		StartedAt:     envelope.StartedAt,
		TimestampUTC:  envelope.TimestampUTC,
		ElapsedNS:     envelope.ElapsedNS,
	}
	switch envelope.Type {
	case "request_start":
		var lineValue startLine
		if err := decodeStrict(line, &lineValue); err != nil {
			return Record{}, err
		}
		if !validJSONObjectShape(lineValue.IncomingRequest) || strings.TrimSpace(lineValue.ServiceAccount.ID) == "" ||
			strings.TrimSpace(lineValue.ServiceAccount.Name) == "" || strings.TrimSpace(lineValue.Model.ID) == "" ||
			strings.TrimSpace(lineValue.Model.Name) == "" || lineValue.Model.Version < 1 || strings.TrimSpace(lineValue.Model.Strategy) == "" {
			return Record{}, errors.New("invalid request_start fields")
		}
		serviceAccount := lineValue.ServiceAccount
		model := lineValue.Model
		record.ServiceAccount = &serviceAccount
		record.Model = &model
		record.IncomingRequest = lineValue.IncomingRequest
	case "upstream_transmission":
		var lineValue transmitLine
		if err := decodeStrict(line, &lineValue); err != nil {
			return Record{}, err
		}
		if lineValue.Attempt < 1 || strings.TrimSpace(lineValue.ProviderID) == "" || strings.TrimSpace(lineValue.ProviderName) == "" ||
			strings.TrimSpace(lineValue.UpstreamModel) == "" || !validJSONObjectShape(lineValue.EffectiveRequest) {
			return Record{}, errors.New("invalid upstream_transmission fields")
		}
		record.Attempt = lineValue.Attempt
		record.ProviderID = lineValue.ProviderID
		record.ProviderName = lineValue.ProviderName
		record.UpstreamModel = lineValue.UpstreamModel
		record.EffectiveRequest = lineValue.EffectiveRequest
	case "upstream_response":
		if envelope.SchemaVersion < 2 {
			return Record{}, errors.New("upstream_response requires schema version 2")
		}
		var lineValue upstreamResponseLine
		if err := decodeStrict(line, &lineValue); err != nil {
			return Record{}, err
		}
		if lineValue.Attempt < 1 || lineValue.HTTPStatus < 100 || lineValue.HTTPStatus > 599 ||
			!validContentTypeHeader(lineValue.ContentType) || !validUpstreamRequestID(lineValue.UpstreamRequestID) || len(lineValue.Body) > maxEventBytes {
			return Record{}, errors.New("invalid upstream_response fields")
		}
		record.Attempt = lineValue.Attempt
		record.HTTPStatus = lineValue.HTTPStatus
		record.UpstreamContentType = lineValue.ContentType
		record.UpstreamRequestID = lineValue.UpstreamRequestID
		record.UpstreamResponseBody = append([]byte(nil), lineValue.Body...)
		record.UpstreamResponseBodyTruncated = lineValue.BodyTruncated
		record.UpstreamResponseBodyReadFailed = lineValue.BodyReadFailed
	case "response_event":
		var lineValue eventLine
		if err := decodeStrict(line, &lineValue); err != nil {
			return Record{}, err
		}
		if lineValue.Attempt < 1 || lineValue.Sequence < 1 || !utf8.ValidString(lineValue.WireText) {
			return Record{}, errors.New("invalid response_event fields")
		}
		record.Attempt = lineValue.Attempt
		record.EventName = lineValue.EventName
		record.Sequence = lineValue.Sequence
		record.WireText = lineValue.WireText
	case "request_end":
		var lineValue endLine
		if err := decodeStrict(line, &lineValue); err != nil {
			return Record{}, err
		}
		if lineValue.Attempt < 0 || !validOutcome(lineValue.Outcome) || lineValue.HTTPStatus < 0 || lineValue.HTTPStatus > 599 ||
			len(lineValue.SafeError) > maxSafeError || !utf8.ValidString(lineValue.SafeError) || !validTimings(lineValue.Timings) || !validUsage(lineValue.Usage) ||
			!validUpstreamRequestID(lineValue.UpstreamRequestID) {
			return Record{}, errors.New("invalid request_end fields")
		}
		if envelope.SchemaVersion >= 2 && (strings.TrimSpace(lineValue.ServiceAccount.ID) == "" || strings.TrimSpace(lineValue.ServiceAccount.Name) == "" ||
			strings.TrimSpace(lineValue.Model.ID) == "" || strings.TrimSpace(lineValue.Model.Name) == "" || lineValue.Model.Version < 1 || strings.TrimSpace(lineValue.Model.Strategy) == "" ||
			(lineValue.Attempt == 0 && lineValue.Provider != nil) || (lineValue.Attempt > 0 && (lineValue.Provider == nil || strings.TrimSpace(lineValue.Provider.ID) == "" || strings.TrimSpace(lineValue.Provider.Name) == "" || strings.TrimSpace(lineValue.Provider.UpstreamModel) == ""))) {
			return Record{}, errors.New("invalid request_end attribution")
		}
		record.Attempt = lineValue.Attempt
		record.Outcome = lineValue.Outcome
		record.HTTPStatus = lineValue.HTTPStatus
		record.SafeError = lineValue.SafeError
		record.UpstreamRequestID = lineValue.UpstreamRequestID
		if envelope.SchemaVersion >= 2 {
			account := lineValue.ServiceAccount
			model := lineValue.Model
			record.ServiceAccount = &account
			record.Model = &model
			if lineValue.Provider != nil {
				provider := *lineValue.Provider
				record.ProviderID, record.ProviderName, record.UpstreamModel = provider.ID, provider.Name, provider.UpstreamModel
			}
		}
		if lineValue.Usage != nil {
			record.Usage = cloneUsage(lineValue.Usage)
		}
		record.Timings = cloneTimings(lineValue.Timings)
	default:
		return Record{}, fmt.Errorf("unsupported record type %q", envelope.Type)
	}
	return record, nil
}

func decodeStrict(line []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values in one history line")
		}
		return err
	}
	return nil
}

func validJSONObjectShape(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}
