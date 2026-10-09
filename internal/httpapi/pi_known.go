package httpapi

import "strings"

// knownModelSpec holds verified Pi metadata for one upstream model, taken from
// the provider's published model documentation. Values are exact published
// specifications, never guesses; the source and verification date are recorded
// per entry so stale values can be rechecked.
type knownModelSpec struct {
	ContextWindow             int
	MaxTokens                 int
	Input                     []string
	SupportedReasoningEfforts []string
}

// knownModelSpecs maps an upstream model ID to its published specification.
// Verified 2026-10-09 against developers.openai.com model pages:
//
//	https://developers.openai.com/api/docs/models/gpt-6-astra
//	https://developers.openai.com/api/docs/models/gpt-6-sol
//	https://developers.openai.com/api/docs/models/gpt-6-luna
//	https://developers.openai.com/api/docs/models/gpt-6.1-sol
//
// All four models: 1,050,000-token context window, 128,000 max output tokens,
// text and image input, text output. The effort lists differ per model:
// gpt-6-astra and gpt-6.1-sol do not support none (or minimal); gpt-6-sol and
// gpt-6-luna support none but not minimal.
var knownModelSpecs = map[string]knownModelSpec{
	"gpt-6-astra": {
		ContextWindow: 1050000, MaxTokens: 128000,
		Input:                     []string{"text", "image"},
		SupportedReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"},
	},
	"gpt-6-sol": {
		ContextWindow: 1050000, MaxTokens: 128000,
		Input:                     []string{"text", "image"},
		SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"},
	},
	"gpt-6-luna": {
		ContextWindow: 1050000, MaxTokens: 128000,
		Input:                     []string{"text", "image"},
		SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"},
	},
	"gpt-6.1-sol": {
		ContextWindow: 1050000, MaxTokens: 128000,
		Input:                     []string{"text", "image"},
		SupportedReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"},
	},
}

// knownModelSpec returns the published specification for an upstream model ID.
// Dated snapshot IDs such as "gpt-6-sol-2026-09-22" fall back to their base
// slug. The lookup only ever returns values published for that exact model;
// unknown upstream models have no spec.
func lookupKnownModelSpec(upstreamModel string) (knownModelSpec, bool) {
	slug := strings.TrimSpace(upstreamModel)
	if spec, ok := knownModelSpecs[slug]; ok {
		return spec, true
	}
	if base, trimmed := trimSnapshotDate(slug); trimmed {
		if spec, ok := knownModelSpecs[base]; ok {
			return spec, true
		}
	}
	return knownModelSpec{}, false
}

// trimSnapshotDate removes a trailing "-YYYY-MM-DD" snapshot suffix, for
// example "gpt-6-sol-2026-09-22" becomes "gpt-6-sol".
func trimSnapshotDate(slug string) (string, bool) {
	parts := strings.Split(slug, "-")
	if len(parts) <= 4 {
		return slug, false
	}
	date := parts[len(parts)-3:]
	if !isDigits(date[0], 4) || !isDigits(date[1], 2) || !isDigits(date[2], 2) {
		return slug, false
	}
	return strings.Join(parts[:len(parts)-3], "-"), true
}

func isDigits(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
