package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gosuda/gyemoim/internal/config"
)

var piReasoningEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}
var acceptedReasoningEfforts = map[string]bool{
	"none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

type piSetupModel struct {
	ModelID       string   `json:"modelId"`
	Name          string   `json:"name"`
	ProviderID    string   `json:"providerId"`
	UpstreamModel string   `json:"upstreamModel"`
	Ready         bool     `json:"ready"`
	Reasons       []string `json:"reasons"`
}

type piSetupResponse struct {
	AccountID                      string           `json:"accountId"`
	AccountEnabled                 bool             `json:"accountEnabled"`
	ProviderID                     string           `json:"providerId"`
	APIKeyEnvironmentVariable      string           `json:"apiKeyEnvironmentVariable"`
	Models                         []piSetupModel   `json:"models"`
	Configuration                  *piConfiguration `json:"configuration,omitempty"`
	ConfigurationUnavailableReason string           `json:"configurationUnavailableReason,omitempty"`
}

type piConfiguration struct {
	Providers map[string]piProvider `json:"providers"`
}

// piProvider deliberately carries no base URL: the reachable address depends on
// how the browser reaches this UI (decision 4), so the UI JavaScript fills
// `baseUrl` from window.location.origin before saving the fragment.
type piProvider struct {
	API    string    `json:"api"`
	APIKey string    `json:"apiKey"`
	Models []piModel `json:"models"`
}

type piModel struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Input            []string           `json:"input"`
	Reasoning        bool               `json:"reasoning"`
	ContextWindow    int                `json:"contextWindow"`
	MaxTokens        int                `json:"maxTokens"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap"`
	Compat           piModelCompat      `json:"compat"`
}

type piModelCompat struct {
	SupportsMaxOutputTokens    bool `json:"supportsMaxOutputTokens"`
	SupportsLongCacheRetention bool `json:"supportsLongCacheRetention"`
}

func (api *managementAPI) piAccountConfig(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	account, err := api.store.GetServiceAccount(r.Context(), accountID)
	if err != nil {
		writeManagementFailure(w, err)
		return
	}
	models, err := api.store.ListGrantedModels(r.Context(), accountID)
	if err != nil {
		writeManagementFailure(w, err)
		return
	}

	providerID := piProviderID(account.ID)
	environmentName := piAPIKeyEnvironmentVariable(account.ID)
	response := piSetupResponse{
		AccountID: account.ID, AccountEnabled: account.Enabled, ProviderID: providerID,
		APIKeyEnvironmentVariable: environmentName,
		Models:                    make([]piSetupModel, 0, len(models)),
	}
	piModels := make([]piModel, 0, len(models))
	for _, model := range models {
		piModel, reasons := piModelForConfig(model)
		response.Models = append(response.Models, piSetupModel{
			ModelID: model.ID, Name: model.Name, ProviderID: model.ProviderID,
			UpstreamModel: model.UpstreamModel, Ready: len(reasons) == 0, Reasons: reasons,
		})
		if len(reasons) == 0 {
			piModels = append(piModels, piModel)
		}
	}

	switch {
	case !account.Enabled:
		response.ConfigurationUnavailableReason = "Enable this service account before exporting its Pi configuration."
	case len(piModels) == 0:
		if len(models) == 0 {
			response.ConfigurationUnavailableReason = "Grant at least one Model to this service account before exporting a Pi configuration."
		} else {
			response.ConfigurationUnavailableReason = "No granted Model has complete Pi metadata yet. Complete the required metadata on a granted reasoning Model."
		}
	default:
		response.Configuration = &piConfiguration{Providers: map[string]piProvider{providerID: {
			API: "openai-responses", APIKey: "${" + environmentName + "}", Models: piModels,
		}}}
	}
	writeJSON(w, http.StatusOK, response)
}

func piModelForConfig(model config.Model) (piModel, []string) {
	metadata := map[string]json.RawMessage{}
	if len(model.MetadataJSON) != 0 {
		if err := json.Unmarshal(model.MetadataJSON, &metadata); err != nil || metadata == nil {
			return piModel{}, []string{"metadata is not a valid JSON object"}
		}
	}

	var reasons []string
	contextWindow, contextWindowOK := positiveInteger(metadata["contextWindow"])
	if !contextWindowOK {
		reasons = append(reasons, "contextWindow must be a positive integer")
	}
	maxTokens, maxTokensOK := positiveInteger(metadata["maxTokens"])
	if !maxTokensOK {
		reasons = append(reasons, "maxTokens must be a positive integer")
	}

	var input []string
	inputOK := false
	if raw, exists := metadata["input"]; exists {
		inputOK = json.Unmarshal(raw, &input) == nil && len(input) > 0
		if inputOK {
			for _, modality := range input {
				if modality != "text" && modality != "image" {
					inputOK = false
					break
				}
			}
		}
	}
	if !inputOK {
		reasons = append(reasons, "input must explicitly list supported text and/or image modalities")
	} else if !containsString(input, "text") {
		reasons = append(reasons, "input must include text")
	}

	var reasoning bool
	reasoningOK := false
	if raw, exists := metadata["reasoning"]; exists {
		var decoded *bool
		if json.Unmarshal(raw, &decoded) == nil && decoded != nil {
			reasoning, reasoningOK = *decoded, true
		}
	}
	if !reasoningOK || !reasoning {
		reasons = append(reasons, "reasoning must be explicitly set to true")
	}

	var efforts []string
	effortsOK := false
	if raw, exists := metadata["supportedReasoningEfforts"]; exists {
		effortsOK = json.Unmarshal(raw, &efforts) == nil && len(efforts) > 0
		if effortsOK {
			for _, effort := range efforts {
				if !acceptedReasoningEfforts[effort] {
					effortsOK = false
					break
				}
			}
		}
	}
	if !effortsOK {
		reasons = append(reasons, "supportedReasoningEfforts must explicitly list at least one supported effort")
	}
	if len(reasons) > 0 {
		return piModel{}, reasons
	}

	return piModel{
		ID: model.Name, Name: model.Name, Input: input, Reasoning: true,
		ContextWindow: contextWindow, MaxTokens: maxTokens,
		ThinkingLevelMap: piThinkingLevelMap(efforts),
		Compat:           piModelCompat{SupportsMaxOutputTokens: false, SupportsLongCacheRetention: false},
	}, nil
}

func piThinkingLevelMap(efforts []string) map[string]*string {
	supported := make(map[string]bool, len(efforts))
	for _, effort := range efforts {
		supported[effort] = true
	}
	mapping := make(map[string]*string, len(piReasoningEfforts)+1)
	if supported["none"] {
		value := "none"
		mapping["off"] = &value
	} else {
		mapping["off"] = nil
	}
	for _, level := range piReasoningEfforts {
		if supported[level] {
			value := level
			mapping[level] = &value
		} else {
			mapping[level] = nil
		}
	}
	return mapping
}

func positiveInteger(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validatePiMetadata(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil || metadata == nil {
		return "metadata must be a JSON object"
	}
	if value, exists := metadata["contextWindow"]; exists {
		if _, ok := positiveInteger(value); !ok {
			return "metadata.contextWindow must be a positive integer"
		}
	}
	if value, exists := metadata["maxTokens"]; exists {
		if _, ok := positiveInteger(value); !ok {
			return "metadata.maxTokens must be a positive integer"
		}
	}
	if value, exists := metadata["input"]; exists {
		var input []string
		if err := json.Unmarshal(value, &input); err != nil || len(input) == 0 {
			return "metadata.input must be a nonempty array containing text and/or image"
		}
		for _, modality := range input {
			if modality != "text" && modality != "image" {
				return "metadata.input may contain only text and image"
			}
		}
	}
	if value, exists := metadata["reasoning"]; exists {
		var reasoning *bool
		if err := json.Unmarshal(value, &reasoning); err != nil || reasoning == nil {
			return "metadata.reasoning must be a boolean"
		}
	}
	if value, exists := metadata["supportedReasoningEfforts"]; exists {
		var efforts []string
		if err := json.Unmarshal(value, &efforts); err != nil || len(efforts) == 0 {
			return "metadata.supportedReasoningEfforts must be a nonempty array"
		}
		for _, effort := range efforts {
			if !acceptedReasoningEfforts[effort] {
				return "metadata.supportedReasoningEfforts contains an unsupported value"
			}
		}
	}
	return ""
}

func piProviderID(accountID string) string {
	var result strings.Builder
	result.WriteString("gyemoim-")
	for _, character := range strings.ToLower(accountID) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	return result.String()
}

func piAPIKeyEnvironmentVariable(accountID string) string {
	var result strings.Builder
	result.WriteString("GYEMOIM_SERVICE_ACCOUNT_")
	for _, character := range strings.ToUpper(accountID) {
		if character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' {
			result.WriteRune(character)
		} else {
			result.WriteByte('_')
		}
	}
	result.WriteString("_API_KEY")
	return result.String()
}
