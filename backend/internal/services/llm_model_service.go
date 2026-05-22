package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"clawreef/internal/models"
	"clawreef/internal/repository"
)

// LLMModelService defines admin model catalog operations.
type LLMModelService interface {
	ListModels() ([]models.LLMModel, error)
	ListActiveModels() ([]models.LLMModel, error)
	SaveModel(req SaveLLMModelRequest) (*models.LLMModel, error)
	DeleteModel(id int) error
	DiscoverProviderModels(req DiscoverLLMModelsRequest) ([]DiscoveredLLMModel, error)
}

// SaveLLMModelRequest contains editable model catalog fields.
type SaveLLMModelRequest struct {
	ID                int
	DisplayName       string
	Description       *string
	ProviderType      string
	ProtocolType      string
	BaseURL           string
	ProviderModelName string
	APIKey            *string
	APIKeySecretRef   *string
	CustomHeaders     *[]models.LLMModelCustomHeader
	IsSecure          bool
	IsActive          bool
	InputPrice        float64
	OutputPrice       float64
	Currency          string
}

// DiscoverLLMModelsRequest contains fields needed to query a provider for available models.
type DiscoverLLMModelsRequest struct {
	ProviderType    string
	ProtocolType    string
	BaseURL         string
	APIKey          *string
	APIKeySecretRef *string
}

// DiscoveredLLMModel is a normalized provider model entry returned by discovery.
type DiscoveredLLMModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type llmModelService struct {
	repo             repository.LLMModelRepository
	httpClient       *http.Client
	secretRefService SecretRefService
}

var versionSegmentPattern = regexp.MustCompile(`(?i)^v\d+(?:[a-z0-9._-]*)?$`)
var httpHeaderNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

var disallowedCustomHeaderNames = map[string]struct{}{
	"connection":          {},
	"content-length":      {},
	"host":                {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

// NewLLMModelService creates a new LLM model service.
func NewLLMModelService(repo repository.LLMModelRepository) LLMModelService {
	return &llmModelService{
		repo:             repo,
		secretRefService: NewSecretRefService(),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (s *llmModelService) ListModels() ([]models.LLMModel, error) {
	items, err := s.repo.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list llm models: %w", err)
	}
	return items, nil
}

func (s *llmModelService) ListActiveModels() ([]models.LLMModel, error) {
	items, err := s.repo.ListActive()
	if err != nil {
		return nil, fmt.Errorf("failed to list active llm models: %w", err)
	}
	return items, nil
}

func (s *llmModelService) SaveModel(req SaveLLMModelRequest) (*models.LLMModel, error) {
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		return nil, errors.New("display name is required")
	}

	providerType := strings.TrimSpace(strings.ToLower(req.ProviderType))
	if providerType == "" {
		return nil, errors.New("provider type is required")
	}
	protocolType, err := models.ResolveLLMProtocolType(providerType, req.ProtocolType)
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		return nil, errors.New("base URL is required")
	}

	providerModelName := strings.TrimSpace(req.ProviderModelName)
	if providerModelName == "" {
		return nil, errors.New("provider model name is required")
	}

	if req.InputPrice < 0 {
		return nil, errors.New("input price must be non-negative")
	}

	if req.OutputPrice < 0 {
		return nil, errors.New("output price must be non-negative")
	}

	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if currency == "" {
		currency = "USD"
	}

	existingByName, err := s.repo.GetByDisplayName(displayName)
	if err != nil {
		return nil, fmt.Errorf("failed to validate model display name: %w", err)
	}
	if existingByName != nil && existingByName.ID != req.ID {
		return nil, errors.New("display name already exists")
	}

	var current *models.LLMModel
	if req.ID != 0 {
		current, err = s.repo.GetByID(req.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to get llm model: %w", err)
		}
		if current == nil {
			return nil, errors.New("model not found")
		}
	}

	var description *string
	if req.Description != nil {
		trimmed := strings.TrimSpace(*req.Description)
		if trimmed != "" {
			description = &trimmed
		}
	}

	var apiKey *string
	if req.APIKey != nil {
		trimmed := strings.TrimSpace(*req.APIKey)
		if trimmed != "" {
			apiKey = &trimmed
		}
	}

	var apiKeySecretRef *string
	if req.APIKeySecretRef != nil {
		trimmed := strings.TrimSpace(*req.APIKeySecretRef)
		if trimmed != "" {
			apiKeySecretRef = &trimmed
		}
	}

	customHeadersJSON := (*string)(nil)
	if req.CustomHeaders != nil {
		customHeadersJSON, err = normalizeLLMModelCustomHeaders(*req.CustomHeaders)
		if err != nil {
			return nil, err
		}
	} else if current != nil {
		customHeadersJSON = current.CustomHeadersJSON
	}

	model := &models.LLMModel{
		ID:                req.ID,
		DisplayName:       displayName,
		Description:       description,
		ProviderType:      providerType,
		ProtocolType:      protocolType,
		BaseURL:           baseURL,
		ProviderModelName: providerModelName,
		APIKey:            apiKey,
		APIKeySecretRef:   apiKeySecretRef,
		CustomHeadersJSON: customHeadersJSON,
		IsSecure:          req.IsSecure,
		IsActive:          req.IsActive,
		InputPrice:        req.InputPrice,
		OutputPrice:       req.OutputPrice,
		Currency:          currency,
	}

	if current != nil {
		model.CreatedAt = current.CreatedAt
	}

	if err := s.repo.Save(model); err != nil {
		return nil, fmt.Errorf("failed to save llm model: %w", err)
	}

	return model, nil
}

func normalizeLLMModelCustomHeaders(items []models.LLMModelCustomHeader) (*string, error) {
	if len(items) == 0 {
		return nil, nil
	}

	normalized := make([]models.LLMModelCustomHeader, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		key := strings.TrimSpace(item.Key)
		value := strings.TrimSpace(item.Value)
		if key == "" && value == "" {
			continue
		}
		if key == "" {
			return nil, errors.New("custom header key is required")
		}
		if value == "" {
			return nil, errors.New("custom header value is required")
		}
		if strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("custom header key and value cannot contain newlines")
		}
		if !httpHeaderNamePattern.MatchString(key) {
			return nil, fmt.Errorf("custom header key is invalid: %s", key)
		}

		normalizedKey := http.CanonicalHeaderKey(key)
		lowerKey := strings.ToLower(normalizedKey)
		if _, blocked := disallowedCustomHeaderNames[lowerKey]; blocked {
			return nil, fmt.Errorf("custom header key is not allowed: %s", normalizedKey)
		}
		if _, exists := seen[lowerKey]; exists {
			return nil, fmt.Errorf("custom header key is duplicated: %s", normalizedKey)
		}
		seen[lowerKey] = struct{}{}
		normalized = append(normalized, models.LLMModelCustomHeader{
			Key:   normalizedKey,
			Value: value,
		})
	}

	if len(normalized) == 0 {
		return nil, nil
	}

	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("failed to encode custom headers: %w", err)
	}
	encoded := string(raw)
	return &encoded, nil
}

func (s *llmModelService) DeleteModel(id int) error {
	existing, err := s.repo.GetByID(id)
	if err != nil {
		return fmt.Errorf("failed to get llm model: %w", err)
	}
	if existing == nil {
		return errors.New("model not found")
	}

	if err := s.repo.Delete(id); err != nil {
		return fmt.Errorf("failed to delete llm model: %w", err)
	}
	return nil
}

func (s *llmModelService) DiscoverProviderModels(req DiscoverLLMModelsRequest) ([]DiscoveredLLMModel, error) {
	providerType := strings.TrimSpace(strings.ToLower(req.ProviderType))
	if providerType == "" {
		return nil, errors.New("provider type is required")
	}
	protocolType, err := models.ResolveLLMProtocolType(providerType, req.ProtocolType)
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		return nil, errors.New("base URL is required")
	}

	resolvedAPIKey, err := s.secretRefService.ResolveString(context.Background(), req.APIKey, req.APIKeySecretRef)
	if err != nil {
		return nil, err
	}

	var apiKey string
	if resolvedAPIKey != nil {
		apiKey = strings.TrimSpace(*resolvedAPIKey)
	}

	switch protocolType {
	case models.ProtocolTypeOpenAI, models.ProtocolTypeOpenAICompatible:
		return s.discoverOpenAICompatibleModels(baseURL, apiKey)
	case models.ProtocolTypeAnthropic:
		return s.discoverAnthropicModels(baseURL, apiKey)
	case models.ProtocolTypeGoogle:
		return s.discoverGoogleModels(baseURL, apiKey)
	case models.ProtocolTypeAzureOpenAI:
		return nil, errors.New("automatic model discovery for azure-openai is not supported yet")
	default:
		return nil, errors.New("provider discovery is not supported")
	}
}

func (s *llmModelService) discoverOpenAICompatibleModels(baseURL, apiKey string) ([]DiscoveredLLMModel, error) {
	endpoint, err := buildProviderEndpoint(baseURL, "v1", "models")
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build discovery request: %w", err)
	}

	if apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}

	type responsePayload struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}

	var payload responsePayload
	if err := s.doJSON(request, &payload); err != nil {
		return nil, err
	}

	models := make([]DiscoveredLLMModel, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		displayName := id
		if owner := strings.TrimSpace(item.OwnedBy); owner != "" {
			displayName = fmt.Sprintf("%s (%s)", id, owner)
		}
		models = append(models, DiscoveredLLMModel{
			ID:          id,
			DisplayName: displayName,
		})
	}
	return models, nil
}

func (s *llmModelService) discoverAnthropicModels(baseURL, apiKey string) ([]DiscoveredLLMModel, error) {
	endpoint, err := buildProviderEndpoint(baseURL, "v1", "models")
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build discovery request: %w", err)
	}
	request.Header.Set("anthropic-version", "2023-06-01")
	if apiKey != "" {
		request.Header.Set("x-api-key", apiKey)
	}

	type responsePayload struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}

	var payload responsePayload
	if err := s.doJSON(request, &payload); err != nil {
		return nil, err
	}

	models := make([]DiscoveredLLMModel, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = id
		}
		models = append(models, DiscoveredLLMModel{
			ID:          id,
			DisplayName: displayName,
		})
	}
	return models, nil
}

func (s *llmModelService) discoverGoogleModels(baseURL, apiKey string) ([]DiscoveredLLMModel, error) {
	endpoint, err := buildProviderEndpoint(baseURL, "v1beta", "models")
	if err != nil {
		return nil, err
	}

	if apiKey != "" {
		separator := "?"
		if strings.Contains(endpoint, "?") {
			separator = "&"
		}
		endpoint = endpoint + separator + "key=" + url.QueryEscape(apiKey)
	}

	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build discovery request: %w", err)
	}

	type responsePayload struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}

	var payload responsePayload
	if err := s.doJSON(request, &payload); err != nil {
		return nil, err
	}

	models := make([]DiscoveredLLMModel, 0, len(payload.Models))
	for _, item := range payload.Models {
		id := strings.TrimSpace(strings.TrimPrefix(item.Name, "models/"))
		if id == "" {
			continue
		}
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = id
		}
		models = append(models, DiscoveredLLMModel{
			ID:          id,
			DisplayName: displayName,
		})
	}
	return models, nil
}

func (s *llmModelService) doJSON(request *http.Request, target any) error {
	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("failed to call provider discovery endpoint: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("provider discovery failed: %s", message)
	}

	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("failed to decode provider discovery response: %w", err)
	}
	return nil
}

func buildProviderEndpoint(baseURL, versionPrefix, resource string) (string, error) {
	trimmed := strings.TrimSpace(strings.TrimRight(baseURL, "/"))
	if trimmed == "" {
		return "", errors.New("base URL is required")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("base URL is invalid")
	}

	versionPath := "/" + strings.Trim(versionPrefix, "/")
	resourcePath := strings.Trim(resource, "/")
	if strings.HasSuffix(strings.ToLower(parsed.Path), strings.ToLower(versionPath)) {
		return trimmed + "/" + resourcePath, nil
	}

	pathSegments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	lastSegment := ""
	if len(pathSegments) > 0 {
		lastSegment = pathSegments[len(pathSegments)-1]
	}
	if versionSegmentPattern.MatchString(lastSegment) {
		return trimmed + "/" + resourcePath, nil
	}

	return trimmed + versionPath + "/" + resourcePath, nil
}
