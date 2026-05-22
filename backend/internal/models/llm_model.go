package models

import (
	"encoding/json"
	"strings"
	"time"
)

// LLMModelCustomHeader stores a model-specific provider header template.
type LLMModelCustomHeader struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// LLMModel stores an admin-managed AI model configuration.
type LLMModel struct {
	ID                int       `db:"id,primarykey,autoincrement" json:"id"`
	DisplayName       string    `db:"display_name" json:"display_name"`
	Description       *string   `db:"description" json:"description,omitempty"`
	ProviderType      string    `db:"provider_type" json:"provider_type"`
	ProtocolType      string    `db:"protocol_type" json:"protocol_type,omitempty"`
	BaseURL           string    `db:"base_url" json:"base_url"`
	ProviderModelName string    `db:"provider_model_name" json:"provider_model_name"`
	APIKey            *string   `db:"api_key" json:"api_key,omitempty"`
	APIKeySecretRef   *string   `db:"api_key_secret_ref" json:"api_key_secret_ref,omitempty"`
	CustomHeadersJSON *string   `db:"custom_headers_json" json:"-"`
	IsSecure          bool      `db:"is_secure" json:"is_secure"`
	IsActive          bool      `db:"is_active" json:"is_active"`
	InputPrice        float64   `db:"input_price" json:"input_price"`
	OutputPrice       float64   `db:"output_price" json:"output_price"`
	Currency          string    `db:"currency" json:"currency"`
	CreatedAt         time.Time `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time `db:"updated_at" json:"updated_at"`
}

// TableName returns the table name for the LLM model.
func (m LLMModel) TableName() string {
	return "llm_models"
}

// CustomHeaders decodes the stored custom provider headers.
func (m LLMModel) CustomHeaders() []LLMModelCustomHeader {
	headers, err := ParseLLMModelCustomHeaders(m.CustomHeadersJSON)
	if err != nil {
		return []LLMModelCustomHeader{}
	}
	return headers
}

// ParseLLMModelCustomHeaders decodes a custom header JSON payload.
func ParseLLMModelCustomHeaders(raw *string) ([]LLMModelCustomHeader, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return []LLMModelCustomHeader{}, nil
	}

	var headers []LLMModelCustomHeader
	if err := json.Unmarshal([]byte(strings.TrimSpace(*raw)), &headers); err != nil {
		return nil, err
	}
	if headers == nil {
		return []LLMModelCustomHeader{}, nil
	}
	return headers, nil
}

// MarshalJSON exposes decoded custom headers while keeping the raw JSON column private.
func (m LLMModel) MarshalJSON() ([]byte, error) {
	type alias LLMModel
	return json.Marshal(struct {
		alias
		CustomHeaders []LLMModelCustomHeader `json:"custom_headers,omitempty"`
	}{
		alias:         alias(m),
		CustomHeaders: m.CustomHeaders(),
	})
}
