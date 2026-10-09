package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Chat Completions translation. SIWC access tokens work only on the Responses
// endpoint, so the chat endpoint is served by translating the Chat Completions
// request into the Responses request format (the approach OpenAI's own Codex
// responses-api-proxy uses) and translating the collected terminal response
// back into one chat.completion object. Translation is request-local: chat
// clients resend conversation history by design, so no response state is
// consulted or stored.

// managedChatRequestFields are Chat Completions top-level fields the translator
// consumes (mapped or validated) or that have no Responses equivalent and are
// silently dropped. Everything else — including benign fields the Responses
// adapter classifies as drops (temperature, top_p, user, metadata, ...) and
// unknown future fields — passes through to Prepare, which owns the classified
// drop policy and upstream validation.
var managedChatRequestFields = map[string]struct{}{
	"messages": {}, "tools": {}, "tool_choice": {}, "response_format": {},
	"reasoning_effort": {}, "verbosity": {}, "n": {}, "stream_options": {},
	"audio": {},
	// Chat-only sampling and output-shaping fields with no Responses equivalent.
	"max_tokens": {}, "max_completion_tokens": {}, "stop": {}, "seed": {},
	"frequency_penalty": {}, "presence_penalty": {}, "logit_bias": {}, "logprobs": {},
}

// TranslateChatCompletions converts a Chat Completions request body into the
// Responses request format. The result carries the translated body, whether the
// client asked for a final usage-only chunk via stream_options.include_usage
// (never forwarded upstream), and the list of chat-only fields that were
// dropped so the executor can record them like the classified Responses drops.
// Client errors are returned as *CapabilityError suitable for a standard
// OpenAI error envelope.
type ChatTranslation struct {
	Responses     json.RawMessage
	IncludeUsage  bool
	DroppedFields []string
}

// chatDroppedByTranslation are Chat Completions request fields the translator
// removes with no Responses equivalent, recorded as drops when present.
// Fields that are mapped (messages, tools, tool_choice, response_format,
// reasoning_effort, verbosity) and fields the Responses adapter classifies
// (temperature, top_p, user, metadata, top_logprobs, ...) are not listed.
var chatDroppedByTranslation = []string{
	"max_tokens", "max_completion_tokens", "stop", "seed",
	"frequency_penalty", "presence_penalty", "logit_bias", "logprobs",
	"n", "stream_options",
}

// jsonNull reports whether a raw value is a JSON null, which optional chat
// fields use the same way as absence.
func jsonNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

func TranslateChatCompletions(chat json.RawMessage) (*ChatTranslation, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(chat, &fields); err != nil || fields == nil {
		return nil, capability("", "invalid_json", "The request body must be a JSON object.")
	}

	messagesRaw, exists := fields["messages"]
	if !exists {
		return nil, capability("messages", "missing_required_parameter", "The request must include messages.")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil || messages == nil {
		return nil, capability("messages", "invalid_type", "The messages field must be an array of message objects.")
	}
	items := make([]json.RawMessage, 0, len(messages))
	for index, message := range messages {
		translated, err := translateChatMessage(index, message)
		if err != nil {
			return nil, err
		}
		items = append(items, translated...)
	}

	includeUsage := false
	if raw, exists := fields["stream_options"]; exists && !jsonNull(raw) {
		var options map[string]json.RawMessage
		if json.Unmarshal(raw, &options) != nil || options == nil {
			return nil, capability("stream_options", "invalid_type", "The stream_options field must be an object.")
		}
		if raw, exists := options["include_usage"]; exists && !jsonNull(raw) {
			value, err := strictBool(raw)
			if err != nil {
				return nil, capability("stream_options.include_usage", "invalid_type", "The stream_options.include_usage field must be a boolean.")
			}
			includeUsage = value
		}
	}
	if raw, exists := fields["n"]; exists && !jsonNull(raw) {
		var count float64
		if json.Unmarshal(raw, &count) != nil {
			return nil, capability("n", "invalid_type", "The n field must be a number.")
		}
		if count != 1 {
			return nil, capability("n", "invalid_value", "Only a single choice is supported; use n: 1.")
		}
	}
	if raw, exists := fields["audio"]; exists && !jsonNull(raw) {
		return nil, capability("audio", "unsupported_value", "Audio output is not supported by this provider adapter.")
	}

	out := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	for key := range managedChatRequestFields {
		delete(out, key)
	}
	inputJSON, err := json.Marshal(items)
	if err != nil {
		return nil, capability("", "invalid_json", "The request body could not be translated.")
	}
	out["input"] = inputJSON

	if raw, exists := fields["tools"]; exists && !jsonNull(raw) {
		translated, err := translateChatTools(raw)
		if err != nil {
			return nil, err
		}
		out["tools"] = translated
	}
	if raw, exists := fields["tool_choice"]; exists && !jsonNull(raw) {
		translated, err := translateChatToolChoice(raw)
		if err != nil {
			return nil, err
		}
		if translated != nil {
			out["tool_choice"] = translated
		}
	}
	text, reasoning, err := translateChatOutputShape(fields)
	if err != nil {
		return nil, err
	}
	if text != nil {
		out["text"] = text
	}
	if reasoning != nil {
		out["reasoning"] = reasoning
	}

	payload, err := json.Marshal(out)
	if err != nil {
		return nil, capability("", "invalid_json", "The request body could not be translated.")
	}
	var dropped []string
	for _, field := range chatDroppedByTranslation {
		if _, exists := fields[field]; exists {
			dropped = append(dropped, field)
		}
	}
	return &ChatTranslation{Responses: payload, IncludeUsage: includeUsage, DroppedFields: dropped}, nil
}

// translateChatMessage converts one chat message into one or more Responses
// input items: assistant tool_calls each become a function_call item, and a
// tool role message becomes a function_call_output item.
func translateChatMessage(index int, raw json.RawMessage) ([]json.RawMessage, error) {
	path := "messages[" + strconv.Itoa(index) + "]"
	var message map[string]json.RawMessage
	if err := json.Unmarshal(raw, &message); err != nil || message == nil {
		return nil, capability(path, "invalid_type", "Each message must be an object.")
	}
	roleRaw, exists := message["role"]
	if !exists {
		return nil, capability(path+".role", "missing_required_parameter", "Each message must include a role.")
	}
	var role string
	if json.Unmarshal(roleRaw, &role) != nil || role == "" {
		return nil, capability(path+".role", "invalid_type", "The message role must be a non-empty string.")
	}
	switch role {
	case "system", "developer":
		return chatMessageItem(message, "developer", "input_text", path)
	case "user":
		return chatMessageItem(message, "user", "input_text", path)
	case "assistant":
		return chatAssistantItems(message, path)
	case "tool":
		return chatToolOutputItem(message, path)
	default:
		return nil, capability(path+".role", "invalid_value", "The message role is not supported; use system, developer, user, assistant, or tool.")
	}
}

func chatMessageItem(message map[string]json.RawMessage, role, textPartType, path string) ([]json.RawMessage, error) {
	content, err := translateChatContent(message["content"], textPartType, path+".content")
	if err != nil {
		return nil, err
	}
	item := map[string]any{"type": "message", "role": role}
	if len(content) > 0 {
		item["content"] = content
	}
	return []json.RawMessage{mustMarshal(item)}, nil
}

// translateChatContent converts chat message content (string, part array, or
// null) into Responses content parts. Unknown part shapes pass through
// unchanged so the upstream API remains the validator for future part types.
func translateChatContent(raw json.RawMessage, textPartType, path string) ([]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []any{map[string]any{"type": textPartType, "text": text}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || parts == nil {
		return nil, capability(path, "invalid_type", "The message content must be a string or an array of content parts.")
	}
	out := make([]any, 0, len(parts))
	for index, part := range parts {
		partPath := path + "[" + strconv.Itoa(index) + "]"
		var entry map[string]json.RawMessage
		if json.Unmarshal(part, &entry) != nil || entry == nil {
			out = append(out, part)
			continue
		}
		var partType string
		_ = json.Unmarshal(entry["type"], &partType)
		switch partType {
		case "text":
			if textPart, hasText := entry["text"]; hasText {
				out = append(out, map[string]any{"type": textPartType, "text": textPart})
				continue
			}
			out = append(out, part)
		case "image_url":
			var image struct {
				URL    *string         `json:"url"`
				Detail json.RawMessage `json:"detail"`
			}
			if json.Unmarshal(entry["image_url"], &image) == nil && image.URL != nil {
				part := map[string]any{"type": "input_image", "image_url": *image.URL}
				if len(image.Detail) != 0 && !jsonNull(image.Detail) {
					// detail affects image quality only; Responses supports the
					// same field on input_image, so it is carried across.
					part["detail"] = image.Detail
				}
				out = append(out, part)
				continue
			}
			out = append(out, part)
		case "input_audio", "audio":
			return nil, capability(partPath, "unsupported_value", "Audio input is not supported by this provider adapter.")
		default:
			out = append(out, part)
		}
	}
	return out, nil
}

func chatAssistantItems(message map[string]json.RawMessage, path string) ([]json.RawMessage, error) {
	if raw, exists := message["function_call"]; exists && !jsonNull(raw) {
		// The deprecated chat function_call shape has no reliable call id to
		// link the following tool message to, so it is an explicit error rather
		// than a silent loss of the tool call.
		return nil, capability(path+".function_call", "unsupported_value", "The deprecated function_call message field is not supported; use tool_calls.")
	}
	content, err := translateChatContent(message["content"], "output_text", path+".content")
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if len(content) > 0 {
		items = append(items, mustMarshal(map[string]any{"type": "message", "role": "assistant", "content": content}))
	}
	if raw, exists := message["tool_calls"]; exists {
		var toolCalls []json.RawMessage
		if err := json.Unmarshal(raw, &toolCalls); err != nil || toolCalls == nil {
			return nil, capability(path+".tool_calls", "invalid_type", "The tool_calls field must be an array.")
		}
		for index, call := range toolCalls {
			callPath := path + ".tool_calls[" + strconv.Itoa(index) + "]"
			var entry map[string]json.RawMessage
			if json.Unmarshal(call, &entry) != nil || entry == nil {
				return nil, capability(callPath, "invalid_type", "Each tool call must be an object.")
			}
			var callType string
			_ = json.Unmarshal(entry["type"], &callType)
			if callType != "function" {
				return nil, capability(callPath+".type", "invalid_value", "Only function tool calls are supported.")
			}
			var id string
			if json.Unmarshal(entry["id"], &id) != nil || id == "" {
				return nil, capability(callPath+".id", "invalid_value", "Each tool call must include a non-empty id.")
			}
			var function map[string]json.RawMessage
			if json.Unmarshal(entry["function"], &function) != nil || function == nil {
				return nil, capability(callPath+".function", "invalid_type", "Each tool call must include a function object.")
			}
			item := map[string]any{"type": "function_call", "call_id": id}
			if name, ok := function["name"]; ok {
				item["name"] = name
			}
			if arguments, ok := function["arguments"]; ok {
				item["arguments"] = arguments
			}
			items = append(items, mustMarshal(item))
		}
	}
	return items, nil
}

func chatToolOutputItem(message map[string]json.RawMessage, path string) ([]json.RawMessage, error) {
	var callID string
	if raw, exists := message["tool_call_id"]; !exists || json.Unmarshal(raw, &callID) != nil || callID == "" {
		return nil, capability(path+".tool_call_id", "invalid_value", "Tool messages must include a non-empty tool_call_id.")
	}
	output := ""
	if raw, exists := message["content"]; exists && len(raw) != 0 && string(raw) != "null" {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			output = text
		} else {
			var parts []json.RawMessage
			if err := json.Unmarshal(raw, &parts); err != nil || parts == nil {
				// Preserve structured tool output as its exact JSON text.
				output = string(raw)
			} else {
				var texts []string
				for _, part := range parts {
					var entry struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(part, &entry) == nil && entry.Text != "" {
						texts = append(texts, entry.Text)
					}
				}
				if len(texts) > 0 {
					output = strings.Join(texts, "\n")
				} else {
					// No text parts to extract (images, empty parts, or
					// non-object entries): preserve the exact JSON instead of
					// silently emptying the tool result.
					output = string(raw)
				}
			}
		}
	}
	return []json.RawMessage{mustMarshal(map[string]any{"type": "function_call_output", "call_id": callID, "output": output})}, nil
}

// translateChatTools de-nests chat function tools into the flat Responses tool
// shape with an explicit strict flag (Responses defaults toward strict mode
// when omitted). Non-function tool entries pass through unchanged; the
// Responses adapter strips classified-unsupported tool types.
func translateChatTools(raw json.RawMessage) (json.RawMessage, error) {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil || tools == nil {
		return nil, capability("tools", "invalid_type", "The tools field must be an array.")
	}
	out := make([]json.RawMessage, 0, len(tools))
	for index, tool := range tools {
		var entry map[string]json.RawMessage
		if json.Unmarshal(tool, &entry) != nil || entry == nil {
			out = append(out, tool)
			continue
		}
		var toolType string
		_ = json.Unmarshal(entry["type"], &toolType)
		if toolType != "function" {
			out = append(out, tool)
			continue
		}
		var function map[string]json.RawMessage
		if json.Unmarshal(entry["function"], &function) != nil || function == nil {
			return nil, capability("tools["+strconv.Itoa(index)+"].function", "invalid_type", "Function tools must include a function object.")
		}
		flat := map[string]json.RawMessage{"type": json.RawMessage(`"function"`)}
		for _, key := range []string{"name", "description", "parameters"} {
			if value, ok := function[key]; ok {
				flat[key] = value
			}
		}
		if strict, ok := function["strict"]; ok {
			flat["strict"] = strict
		} else {
			flat["strict"] = json.RawMessage("false")
		}
		out = append(out, mustMarshal(flat))
	}
	return json.Marshal(out)
}

// translateChatToolChoice flattens the chat object form; the string forms are
// identical in both APIs. nil means the field should be dropped.
func translateChatToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	var choice string
	if json.Unmarshal(raw, &choice) == nil {
		switch choice {
		case "none", "auto", "required":
			return raw, nil
		default:
			return nil, capability("tool_choice", "invalid_value", "The tool_choice value is not supported; use none, auto, required, or a function object.")
		}
	}
	var entry struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Type != "function" || entry.Function == nil || entry.Function.Name == "" {
		return nil, capability("tool_choice", "invalid_value", "Only function tool_choice objects with a function name are supported.")
	}
	return json.Marshal(map[string]any{"type": "function", "name": entry.Function.Name})
}

// translateChatOutputShape maps reasoning_effort, verbosity, and
// response_format into the Responses reasoning and text objects. A JSON null
// value is treated the same as absence.
func translateChatOutputShape(fields map[string]json.RawMessage) (text, reasoning json.RawMessage, err error) {
	if raw, exists := fields["reasoning_effort"]; exists && !jsonNull(raw) {
		var effort string
		if json.Unmarshal(raw, &effort) != nil || effort == "" {
			return nil, nil, capability("reasoning_effort", "invalid_type", "The reasoning_effort field must be a non-empty string.")
		}
		reasoning = mustMarshal(map[string]any{"effort": effort})
	}
	if raw, exists := fields["verbosity"]; exists && !jsonNull(raw) {
		var verbosity string
		if json.Unmarshal(raw, &verbosity) != nil || verbosity == "" {
			return nil, nil, capability("verbosity", "invalid_type", "The verbosity field must be a non-empty string.")
		}
		var shape map[string]any
		if text != nil {
			_ = json.Unmarshal(text, &shape)
		}
		if shape == nil {
			shape = map[string]any{}
		}
		shape["verbosity"] = verbosity
		text = mustMarshal(shape)
	}
	if raw, exists := fields["response_format"]; exists && !jsonNull(raw) {
		format, err := translateChatResponseFormat(raw)
		if err != nil {
			return nil, nil, err
		}
		if format != nil {
			var shape map[string]any
			if text != nil {
				_ = json.Unmarshal(text, &shape)
			}
			if shape == nil {
				shape = map[string]any{}
			}
			shape["format"] = format
			text = mustMarshal(shape)
		}
	}
	return text, reasoning, nil
}

// translateChatResponseFormat maps the chat response_format object into the
// Responses text.format object. nil means the field is dropped (text is the
// default and needs no format).
func translateChatResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil || entry == nil {
		return nil, capability("response_format", "invalid_type", "The response_format field must be an object.")
	}
	var formatType string
	if json.Unmarshal(entry["type"], &formatType) != nil {
		return nil, capability("response_format.type", "invalid_type", "The response_format.type field must be a string.")
	}
	switch formatType {
	case "text":
		return nil, nil
	case "json_object":
		return json.RawMessage(`{"type":"json_object"}`), nil
	case "json_schema":
		var schema struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		}
		if json.Unmarshal(entry["json_schema"], &schema) != nil || schema.Name == "" || len(schema.Schema) == 0 {
			return nil, capability("response_format.json_schema", "invalid_value", "JSON schema response formats require a name and a schema.")
		}
		format := map[string]any{"type": "json_schema", "name": schema.Name, "schema": schema.Schema}
		if schema.Description != "" {
			format["description"] = schema.Description
		}
		if schema.Strict != nil {
			format["strict"] = *schema.Strict
		} else {
			format["strict"] = false
		}
		return mustMarshal(format), nil
	default:
		return nil, capability("response_format.type", "invalid_value", "The response_format type is not supported; use text, json_object, or json_schema.")
	}
}

// TranslateChatCompletionResponse converts the collected terminal Responses
// object into one chat.completion JSON body. content is concatenated from
// output_text parts; function_call items become message.tool_calls in order.
// The chat format cannot represent reasoning items, so they are omitted
// (documented loss; usage still reports reasoning tokens).
func TranslateChatCompletionResponse(gatewayRequestID, modelAlias string, createdUnix int64, responseJSON json.RawMessage) (json.RawMessage, error) {
	var response struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []json.RawMessage   `json:"output"`
		Usage  *responsesUsageJSON `json:"usage"`
	}
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		return nil, fmt.Errorf("provider response is not a Responses object")
	}
	var content strings.Builder
	var toolCalls []map[string]any
	for _, item := range response.Output {
		var typed struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if json.Unmarshal(item, &typed) != nil {
			continue
		}
		switch typed.Type {
		case "message":
			for _, part := range typed.Content {
				if part.Type == "output_text" {
					content.WriteString(part.Text)
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id":   typed.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      typed.Name,
					"arguments": typed.Arguments,
				},
			})
		}
	}
	contentValue := any(content.String())
	if contentValue == "" && len(toolCalls) > 0 {
		contentValue = nil
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	if response.Status == "incomplete" && response.IncompleteDetails != nil && response.IncompleteDetails.Reason == "max_output_tokens" {
		finishReason = "length"
	}
	message := map[string]any{"role": "assistant", "content": contentValue}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	completion := map[string]any{
		"id":      "chatcmpl-" + gatewayRequestID,
		"object":  "chat.completion",
		"created": createdUnix,
		"model":   modelAlias,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finishReason, "logprobs": nil,
		}},
		"usage": chatUsageJSON(response.Usage),
	}
	return json.Marshal(completion)
}

// responsesUsageJSON mirrors the provider-reported Responses usage object.
type responsesUsageJSON struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// chatUsageShape builds the chat usage object from possibly-missing counts.
// Missing counts stay null rather than zero.
func chatUsageShape(prompt, completion, cached, reasoning *int64) map[string]any {
	chatUsage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
	}
	if prompt != nil && completion != nil {
		total := *prompt + *completion
		chatUsage["total_tokens"] = &total
	} else {
		chatUsage["total_tokens"] = nil
	}
	if cached != nil {
		chatUsage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoning != nil {
		chatUsage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return chatUsage
}

// chatUsageJSON maps the provider-reported Responses usage object into the
// chat usage shape.
func chatUsageJSON(usage *responsesUsageJSON) any {
	if usage == nil {
		return nil
	}
	var cached, reasoning *int64
	if usage.InputTokensDetails != nil {
		cached = usage.InputTokensDetails.CachedTokens
	}
	if usage.OutputTokensDetails != nil {
		reasoning = usage.OutputTokensDetails.ReasoningTokens
	}
	return chatUsageShape(usage.InputTokens, usage.OutputTokens, cached, reasoning)
}

// chatUsageFromProviderUsage maps the adapter's parsed terminal usage into the
// chat usage shape used by streaming usage chunks. A missing usage report is
// still an object, with null counts.
func chatUsageFromProviderUsage(usage *Usage) any {
	if usage == nil {
		usage = &Usage{}
	}
	return chatUsageShape(usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.ReasoningOutputTokens)
}

// ChatStreamSynthesizer converts parsed Responses SSE events into Chat
// Completions completion chunks. Reasoning deltas have no chat representation
// and are not emitted (documented loss; usage still reports reasoning tokens).
// Tool calls are accumulated from output_item.added — which carries the call
// id and name and fixes the stable tool index — plus
// function_call_arguments.delta; later chunks omit id and name, which is the
// shape chat clients accumulate. Building tool calls only from the terminal
// response would drop streamed tool calls entirely.
type ChatStreamSynthesizer struct {
	gatewayRequestID string
	modelAlias       string
	createdUnix      int64
	includeUsage     bool

	roleSent     bool
	toolIndexes  map[string]int
	toolCount    int
	sawToolCalls bool
}

// NewChatStreamSynthesizer creates the per-request synthesizer. gatewayRequestID
// and modelAlias back the chunk id/model fields; includeUsage controls whether
// a final usage-only chunk is emitted for stream_options.include_usage.
func NewChatStreamSynthesizer(gatewayRequestID, modelAlias string, createdUnix int64, includeUsage bool) *ChatStreamSynthesizer {
	return &ChatStreamSynthesizer{
		gatewayRequestID: gatewayRequestID,
		modelAlias:       modelAlias,
		createdUnix:      createdUnix,
		includeUsage:     includeUsage,
		toolIndexes:      map[string]int{},
	}
}

// Feed consumes one parsed Responses event and returns the chat completion
// chunk bodies to write downstream, in order. The caller owns SSE framing, the
// data: [DONE] sentinel, and error chunks for failed terminal events.
func (s *ChatStreamSynthesizer) Feed(event SSEEvent) []json.RawMessage {
	switch event.Type {
	case "response.created":
		s.roleSent = true
		return []json.RawMessage{s.chunk(map[string]any{"role": "assistant"}, nil)}
	case "response.output_text.delta":
		var payload struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(event.DataJSON, &payload) != nil || payload.Delta == "" {
			return nil
		}
		return s.withRole([]json.RawMessage{s.chunk(map[string]any{"content": payload.Delta}, nil)})
	case "response.output_item.added":
		var payload struct {
			Item struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if json.Unmarshal(event.DataJSON, &payload) != nil || payload.Item.Type != "function_call" {
			return nil
		}
		index := s.toolCount
		s.toolCount++
		s.sawToolCalls = true
		// Register the index under both identifiers so arguments deltas resolve
		// whichever one the upstream uses for item_id.
		if payload.Item.ID != "" {
			s.toolIndexes[payload.Item.ID] = index
		}
		if payload.Item.CallID != "" && payload.Item.CallID != payload.Item.ID {
			s.toolIndexes[payload.Item.CallID] = index
		}
		return s.withRole([]json.RawMessage{s.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index,
			"id":    payload.Item.CallID,
			"type":  "function",
			"function": map[string]any{
				"name":      payload.Item.Name,
				"arguments": "",
			},
		}}}, nil)})
	case "response.function_call_arguments.delta":
		var payload struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(event.DataJSON, &payload) != nil || payload.Delta == "" {
			return nil
		}
		index, known := s.toolIndexes[payload.ItemID]
		if !known {
			return nil
		}
		return []json.RawMessage{s.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    index,
			"function": map[string]any{"arguments": payload.Delta},
		}}}, nil)}
	case "response.completed", "response.incomplete":
		if !event.Terminal {
			return nil
		}
		return s.finalChunks(event)
	default:
		// content_part bookkeeping, done events, and reasoning deltas are not
		// represented in the chat streaming shape.
		return nil
	}
}

func (s *ChatStreamSynthesizer) withRole(chunks []json.RawMessage) []json.RawMessage {
	if s.roleSent {
		return chunks
	}
	s.roleSent = true
	return append([]json.RawMessage{s.chunk(map[string]any{"role": "assistant"}, nil)}, chunks...)
}

// finalChunks emits the terminal content chunk — empty delta with
// finish_reason — followed by the optional usage-only chunk. finish_reason is
// tool_calls when function-call items were streamed, length when the response
// stopped on the output-token cap, and stop otherwise.
func (s *ChatStreamSynthesizer) finalChunks(event SSEEvent) []json.RawMessage {
	finishReason := "stop"
	if s.sawToolCalls {
		finishReason = "tool_calls"
	}
	if event.Outcome == "incomplete" {
		source := event.ResponseJSON
		if len(source) == 0 {
			// The terminal parser normally extracts the response object; fall
			// back to its envelope when it did not.
			var envelope struct {
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(event.DataJSON, &envelope) == nil && len(envelope.Response) > 0 {
				source = envelope.Response
			} else {
				source = event.DataJSON
			}
		}
		if incompleteReason(source) == "max_output_tokens" {
			finishReason = "length"
		}
	}
	chunks := s.withRole([]json.RawMessage{s.chunk(map[string]any{}, finishReason)})
	if s.includeUsage {
		chunks = append(chunks, s.usageChunk(event.Usage))
	}
	return chunks
}

func (s *ChatStreamSynthesizer) chunk(delta map[string]any, finishReason any) json.RawMessage {
	return mustMarshal(map[string]any{
		"id":      "chatcmpl-" + s.gatewayRequestID,
		"object":  "chat.completion.chunk",
		"created": s.createdUnix,
		"model":   s.modelAlias,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	})
}

func (s *ChatStreamSynthesizer) usageChunk(usage *Usage) json.RawMessage {
	return mustMarshal(map[string]any{
		"id":      "chatcmpl-" + s.gatewayRequestID,
		"object":  "chat.completion.chunk",
		"created": s.createdUnix,
		"model":   s.modelAlias,
		"choices": []any{},
		"usage":   chatUsageFromProviderUsage(usage),
	})
}

func incompleteReason(raw json.RawMessage) string {
	var response struct {
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if !jsonObject(raw) || json.Unmarshal(raw, &response) != nil || response.IncompleteDetails == nil {
		return ""
	}
	return response.IncompleteDetails.Reason
}

func mustMarshal(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		// All values here are maps, slices, and JSON raw messages that cannot
		// fail to marshal; fall back to an empty object rather than panicking.
		return json.RawMessage("{}")
	}
	return encoded
}
