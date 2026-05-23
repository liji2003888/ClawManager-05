package aigateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"clawreef/internal/models"
	"clawreef/internal/repository"
	"clawreef/internal/services"
)

// ToolCallFunction represents a tool/function call payload.
type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall represents a tool call emitted by an assistant response.
type ToolCall struct {
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function *ToolCallFunction `json:"function,omitempty"`
	Index    *int              `json:"index,omitempty"`
}

// ChatMessage represents an OpenAI-compatible chat message.
type ChatMessage struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`
	Name       string      `json:"name,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Refusal    interface{} `json:"refusal,omitempty"`
	Audio      interface{} `json:"audio,omitempty"`
}

// PassthroughRequest captures the minimal context needed to proxy an
// OpenAI-compatible request to an upstream provider without parsing the body.
type PassthroughRequest struct {
	Model      string
	RawBody    []byte
	InstanceID *int
	SubPath    string // e.g. "/responses" — appended to model.BaseURL
	Stream     bool
	TraceID    string
	RequestID  string
}

// ChatCompletionRequest is the platform gateway request shape.
type ChatCompletionRequest struct {
	RawBody           []byte          `json:"-"`
	Model             string          `json:"model"`
	Messages          []ChatMessage   `json:"messages"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Stream            bool            `json:"stream"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"`
	Stop              json.RawMessage `json:"stop,omitempty"`
	N                 *int            `json:"n,omitempty"`
	FrequencyPenalty  *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64        `json:"presence_penalty,omitempty"`
	ReasoningEffort   *string         `json:"reasoning_effort,omitempty"`
	StreamOptions     json.RawMessage `json:"stream_options,omitempty"`
	User              *string         `json:"user,omitempty"`
	SessionID         *string         `json:"session_id,omitempty"`
	InstanceID        *int            `json:"instance_id,omitempty"`
	TraceID           *string         `json:"trace_id,omitempty"`
	RequestID         *string         `json:"request_id,omitempty"`
}

// ChatCompletionResponse is used for audit parsing only.
type ChatCompletionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	Created int64  `json:"created,omitempty"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string      `json:"role"`
			Content   interface{} `json:"content"`
			ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
			Refusal   interface{} `json:"refusal,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ProxyResponse is the raw provider response returned to the client.
type ProxyResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// AvailableModel represents a user-selectable active model.
type AvailableModel struct {
	ID          int     `json:"id"`
	DisplayName string  `json:"display_name"`
	Description *string `json:"description,omitempty"`
	IsSecure    bool    `json:"is_secure"`
	Provider    string  `json:"provider_type"`
}

const autoModelID = "auto"
const maxStoredIdentifierLength = 100
const anthropicVersionHeader = "2023-06-01"
const defaultAnthropicMaxTokens = 4096

var providerVersionSegmentPattern = regexp.MustCompile(`(?i)^v\d+(?:[a-z0-9._-]*)?$`)
var customHeaderTemplatePattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}|\$\{\s*([A-Za-z0-9_.-]+)\s*\}`)
var customHeaderNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

type preparedChatRequest struct {
	traceID       string
	sessionID     string
	sessionIDPtr  *string
	requestID     string
	requestIDPtr  *string
	userID        int
	userIDPtr     *int
	instance      *models.Instance
	selectedModel *models.LLMModel
	resolvedModel *models.LLMModel
	req           ChatCompletionRequest
}

type openAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	Created int64  `json:"created,omitempty"`
	Model   string `json:"model,omitempty"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string      `json:"role,omitempty"`
			Content   interface{} `json:"content,omitempty"`
			ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
			Refusal   interface{} `json:"refusal,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

type openAIStreamDelta struct {
	Role      string      `json:"role,omitempty"`
	Content   interface{} `json:"content,omitempty"`
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
}

type openAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason,omitempty"`
}

type openAIStreamResponseChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object,omitempty"`
	Created int64                `json:"created,omitempty"`
	Model   string               `json:"model,omitempty"`
	Choices []openAIStreamChoice `json:"choices"`
	Usage   *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

type anthropicRequestMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicRequestPayload struct {
	Model       string                    `json:"model"`
	Messages    []anthropicRequestMessage `json:"messages"`
	System      string                    `json:"system,omitempty"`
	MaxTokens   int                       `json:"max_tokens"`
	Stream      bool                      `json:"stream,omitempty"`
	Temperature *float64                  `json:"temperature,omitempty"`
	TopP        *float64                  `json:"top_p,omitempty"`
	StopSeqs    []string                  `json:"stop_sequences,omitempty"`
	Tools       []anthropicToolDefinition `json:"tools,omitempty"`
	ToolChoice  map[string]interface{}    `json:"tool_choice,omitempty"`
}

type anthropicToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"input_schema"`
}

type anthropicContentBlock struct {
	Type      string      `json:"type"`
	Text      string      `json:"text,omitempty"`
	ID        string      `json:"id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Input     interface{} `json:"input,omitempty"`
	ToolUseID string      `json:"tool_use_id,omitempty"`
	Content   interface{} `json:"content,omitempty"`
}

type anthropicMessageResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type,omitempty"`
	Role       string                  `json:"role,omitempty"`
	Model      string                  `json:"model,omitempty"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason,omitempty"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicStreamEvent struct {
	Type         string                    `json:"type"`
	Message      *anthropicMessageResponse `json:"message,omitempty"`
	Index        *int                      `json:"index,omitempty"`
	ContentBlock *anthropicContentBlock    `json:"content_block,omitempty"`
	Delta        *struct {
		Type         string `json:"type,omitempty"`
		Text         string `json:"text,omitempty"`
		PartialJSON  string `json:"partial_json,omitempty"`
		StopReason   string `json:"stop_reason,omitempty"`
		StopSequence string `json:"stop_sequence,omitempty"`
	} `json:"delta,omitempty"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Type    string `json:"type,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

type anthropicStreamState struct {
	ResponseID       string
	Model            string
	Created          int64
	PromptTokens     int
	CompletionTokens int
	AssistantText    strings.Builder
	ToolCalls        map[int]*anthropicToolCallState
	SentRole         bool
}

type anthropicToolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// Service defines gateway operations.
type Service interface {
	ListAvailableModels() ([]AvailableModel, error)
	ChatCompletions(ctx context.Context, userID int, req ChatCompletionRequest) (*ProxyResponse, string, error)
	StreamChatCompletions(ctx context.Context, userID int, req ChatCompletionRequest, w http.ResponseWriter) (string, error)
	// Passthrough proxies an unrecognized OpenAI-compatible POST (e.g. /responses)
	// directly to the upstream provider. Risk detection, audit logging, and cost
	// accounting are intentionally skipped — use only for protocols ClawManager
	// does not natively understand.
	Passthrough(ctx context.Context, userID int, req PassthroughRequest) (*http.Response, *models.LLMModel, error)
}

type service struct {
	modelRepo          repository.LLMModelRepository
	instanceRepo       repository.InstanceRepository
	invocationService  services.ModelInvocationService
	auditEventService  services.AuditEventService
	costRecordService  services.CostRecordService
	riskDetector       services.RiskDetectionService
	riskHitService     services.RiskHitService
	chatSessionService services.ChatSessionService
	chatMessageService services.ChatMessageService
	secretRefService   services.SecretRefService
	httpClient         *http.Client
	passthroughClient  *http.Client
}

// NewService creates a new AI gateway service.
func NewService(
	modelRepo repository.LLMModelRepository,
	instanceRepo repository.InstanceRepository,
	invocationService services.ModelInvocationService,
	auditEventService services.AuditEventService,
	costRecordService services.CostRecordService,
	riskDetector services.RiskDetectionService,
	riskHitService services.RiskHitService,
	chatSessionService services.ChatSessionService,
	chatMessageService services.ChatMessageService,
) Service {
	return &service{
		modelRepo:          modelRepo,
		instanceRepo:       instanceRepo,
		invocationService:  invocationService,
		auditEventService:  auditEventService,
		costRecordService:  costRecordService,
		riskDetector:       riskDetector,
		riskHitService:     riskHitService,
		chatSessionService: chatSessionService,
		chatMessageService: chatMessageService,
		secretRefService:   services.NewSecretRefService(),
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
		passthroughClient: &http.Client{
			// Reasoning models / Responses API streams can run for minutes.
			// Rely on the request context for cancellation instead of a hard
			// client-side timeout.
			Timeout: 0,
		},
	}
}

func (s *service) ListAvailableModels() ([]AvailableModel, error) {
	items, err := s.modelRepo.ListActive()
	if err != nil {
		return nil, fmt.Errorf("failed to list available models: %w", err)
	}
	if len(items) == 0 {
		return []AvailableModel{}, nil
	}

	return []AvailableModel{
		{
			ID:          0,
			DisplayName: "Auto",
			Description: stringPtr("Automatically route requests to the best available model under current governance policy."),
			IsSecure:    false,
			Provider:    "gateway",
		},
	}, nil
}

func (s *service) ChatCompletions(ctx context.Context, userID int, req ChatCompletionRequest) (*ProxyResponse, string, error) {
	prepared, err := s.prepareChatRequest(userID, req)
	if err != nil {
		traceID := ""
		if prepared != nil {
			traceID = prepared.traceID
		}
		return nil, traceID, err
	}

	switch models.ResolveLLMProtocolTypeOrDefault(prepared.resolvedModel.ProviderType, prepared.resolvedModel.ProtocolType) {
	case models.ProtocolTypeOpenAI, models.ProtocolTypeOpenAICompatible:
		return s.callOpenAICompatible(ctx, prepared)
	case models.ProtocolTypeAnthropic:
		return s.callAnthropic(ctx, prepared)
	default:
		_ = s.auditEventService.RecordEvent(&models.AuditEvent{
			TraceID:      prepared.traceID,
			SessionID:    prepared.req.SessionID,
			RequestID:    prepared.requestIDPtr,
			UserID:       prepared.userIDPtr,
			InstanceID:   prepared.req.InstanceID,
			EventType:    "gateway.request.blocked",
			TrafficClass: models.TrafficClassLLM,
			Severity:     models.AuditSeverityWarn,
			Message:      fmt.Sprintf("Provider type %s is not supported yet", prepared.resolvedModel.ProviderType),
		})
		return nil, prepared.traceID, errors.New("provider type is not supported yet")
	}
}

func (s *service) StreamChatCompletions(ctx context.Context, userID int, req ChatCompletionRequest, w http.ResponseWriter) (string, error) {
	prepared, err := s.prepareChatRequest(userID, req)
	if err != nil {
		traceID := ""
		if prepared != nil {
			traceID = prepared.traceID
		}
		return traceID, err
	}

	switch models.ResolveLLMProtocolTypeOrDefault(prepared.resolvedModel.ProviderType, prepared.resolvedModel.ProtocolType) {
	case models.ProtocolTypeOpenAI, models.ProtocolTypeOpenAICompatible:
		return prepared.traceID, s.streamOpenAICompatible(ctx, prepared, w)
	case models.ProtocolTypeAnthropic:
		return prepared.traceID, s.streamAnthropic(ctx, prepared, w)
	default:
		_ = s.auditEventService.RecordEvent(&models.AuditEvent{
			TraceID:      prepared.traceID,
			SessionID:    prepared.req.SessionID,
			RequestID:    prepared.requestIDPtr,
			UserID:       prepared.userIDPtr,
			InstanceID:   prepared.req.InstanceID,
			EventType:    "gateway.request.blocked",
			TrafficClass: models.TrafficClassLLM,
			Severity:     models.AuditSeverityWarn,
			Message:      fmt.Sprintf("Provider type %s is not supported yet", prepared.resolvedModel.ProviderType),
		})
		return prepared.traceID, errors.New("provider type is not supported yet")
	}
}

func (s *service) prepareChatRequest(userID int, req ChatCompletionRequest) (*preparedChatRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("messages are required")
	}

	requestedModel := strings.TrimSpace(req.Model)
	selectedModel, err := s.resolveRequestedModel(requestedModel)
	if err != nil {
		return nil, err
	}

	prepared := &preparedChatRequest{
		userID:        userID,
		userIDPtr:     intPtr(userID),
		selectedModel: selectedModel,
		req:           req,
	}
	prepared.sessionID = resolveSessionID(req)
	prepared.traceID = s.resolveTraceID(userID, req, prepared.sessionID)
	if prepared.sessionID == "" {
		prepared.sessionID = normalizeSessionID(nil, prepared.traceID)
	}
	prepared.sessionIDPtr = stringPtr(prepared.sessionID)
	prepared.req.SessionID = prepared.sessionIDPtr
	prepared.requestID = normalizeOrCreateID(req.RequestID, "req")
	prepared.requestIDPtr = stringPtr(prepared.requestID)
	if prepared.req.InstanceID != nil && s.instanceRepo != nil {
		instance, err := s.instanceRepo.GetByID(*prepared.req.InstanceID)
		if err != nil {
			return prepared, fmt.Errorf("failed to get instance: %w", err)
		}
		if instance == nil {
			return prepared, errors.New("instance not found")
		}
		if instance.UserID != userID {
			return prepared, errors.New("access denied")
		}
		prepared.instance = instance
	}

	sessionTitle := deriveSessionTitle(prepared.req.Messages)
	if _, err := s.chatSessionService.EnsureSession(prepared.sessionID, prepared.userIDPtr, prepared.req.InstanceID, stringPtr(prepared.traceID), sessionTitle); err != nil {
		logPersistenceError("ensure chat session", prepared.traceID, err)
	}
	if err := s.chatMessageService.RecordMessages(
		prepared.traceID,
		prepared.sessionID,
		prepared.requestIDPtr,
		prepared.userIDPtr,
		prepared.req.InstanceID,
		nil,
		buildPersistedMessages(prepared.req.Messages),
	); err != nil {
		logPersistenceError("record request messages", prepared.traceID, err)
	}

	if err := s.auditEventService.RecordEvent(&models.AuditEvent{
		TraceID:      prepared.traceID,
		SessionID:    prepared.req.SessionID,
		RequestID:    prepared.requestIDPtr,
		UserID:       prepared.userIDPtr,
		InstanceID:   prepared.req.InstanceID,
		EventType:    "gateway.request.received",
		TrafficClass: models.TrafficClassLLM,
		Severity:     models.AuditSeverityInfo,
		Message:      fmt.Sprintf("Received LLM request for model %s", prepared.req.Model),
	}); err != nil {
		logPersistenceError("record gateway.request.received", prepared.traceID, err)
	}

	riskAnalysis := s.riskDetector.AnalyzeText(flattenMessages(prepared.req.Messages))
	if riskAnalysis.IsSensitive {
		if err := s.auditEventService.RecordEvent(&models.AuditEvent{
			TraceID:      prepared.traceID,
			SessionID:    prepared.req.SessionID,
			RequestID:    prepared.requestIDPtr,
			UserID:       prepared.userIDPtr,
			InstanceID:   prepared.req.InstanceID,
			EventType:    "gateway.risk.detected",
			TrafficClass: models.TrafficClassLLM,
			Severity:     models.AuditSeverityWarn,
			Message:      fmt.Sprintf("Sensitive content detected with %d hit(s)", len(riskAnalysis.Hits)),
		}); err != nil {
			logPersistenceError("record gateway.risk.detected", prepared.traceID, err)
		}
	}

	resolvedModel, riskAction, resolveErr := s.resolveTargetModel(prepared.selectedModel, riskAnalysis)
	if resolveErr != nil {
		blockedInvocationID := s.recordBlockedInvocation(prepared.traceID, prepared.requestID, prepared.req, prepared.userID, prepared.selectedModel, resolveErr.Error())
		if err := s.riskHitService.RecordHits(prepared.traceID, prepared.req.SessionID, prepared.requestIDPtr, prepared.userIDPtr, prepared.req.InstanceID, blockedInvocationID, riskAction, riskAnalysis.Hits); err != nil {
			logPersistenceError("record blocked risk hits", prepared.traceID, err)
		}
		if err := s.auditEventService.RecordEvent(&models.AuditEvent{
			TraceID:      prepared.traceID,
			SessionID:    prepared.req.SessionID,
			RequestID:    prepared.requestIDPtr,
			UserID:       prepared.userIDPtr,
			InstanceID:   prepared.req.InstanceID,
			InvocationID: blockedInvocationID,
			EventType:    "gateway.request.blocked",
			TrafficClass: models.TrafficClassLLM,
			Severity:     models.AuditSeverityWarn,
			Message:      resolveErr.Error(),
		}); err != nil {
			logPersistenceError("record gateway.request.blocked", prepared.traceID, err)
		}
		return prepared, resolveErr
	}

	if riskAnalysis.IsSensitive {
		if err := s.riskHitService.RecordHits(prepared.traceID, prepared.req.SessionID, prepared.requestIDPtr, prepared.userIDPtr, prepared.req.InstanceID, nil, riskAction, riskAnalysis.Hits); err != nil {
			logPersistenceError("record risk hits", prepared.traceID, err)
		}
		if riskAction == models.RiskActionRouteSecureModel && resolvedModel != nil && resolvedModel.ID != prepared.selectedModel.ID {
			if err := s.auditEventService.RecordEvent(&models.AuditEvent{
				TraceID:      prepared.traceID,
				SessionID:    prepared.req.SessionID,
				RequestID:    prepared.requestIDPtr,
				UserID:       prepared.userIDPtr,
				InstanceID:   prepared.req.InstanceID,
				EventType:    "gateway.request.rerouted",
				TrafficClass: models.TrafficClassLLM,
				Severity:     models.AuditSeverityWarn,
				Message:      fmt.Sprintf("Sensitive content rerouted from model %s to secure model %s", prepared.selectedModel.DisplayName, resolvedModel.DisplayName),
			}); err != nil {
				logPersistenceError("record gateway.request.rerouted", prepared.traceID, err)
			}
		}
	}

	prepared.resolvedModel = resolvedModel
	return prepared, nil
}

func (s *service) callOpenAICompatible(ctx context.Context, prepared *preparedChatRequest) (*ProxyResponse, string, error) {
	resolvedAPIKey, err := s.secretRefService.ResolveString(ctx, prepared.resolvedModel.APIKey, prepared.resolvedModel.APIKeySecretRef)
	if err != nil {
		return nil, prepared.traceID, err
	}

	providerRequestBody, err := buildProviderRequestBody(prepared.req, prepared.resolvedModel)
	if err != nil {
		return nil, prepared.traceID, err
	}

	httpRequest, err := buildProviderHTTPRequest(ctx, prepared.traceID, prepared.requestID, prepared.resolvedModel, providerRequestBody, resolvedAPIKey, false, prepared.customHeaderVariables())
	if err != nil {
		return nil, prepared.traceID, err
	}

	startedAt := time.Now()
	response, err := s.httpClient.Do(httpRequest)
	if err != nil {
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("provider call failed: %v", err), providerRequestBody)
		return nil, prepared.traceID, fmt.Errorf("failed to call provider: %w", err)
	}
	defer response.Body.Close()

	responseBody, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("failed to read provider response: %v", readErr), providerRequestBody)
		return nil, prepared.traceID, fmt.Errorf("failed to read provider response: %w", readErr)
	}

	proxyResponse := &ProxyResponse{
		StatusCode: response.StatusCode,
		Headers:    cloneProxyHeaders(response.Header),
		Body:       responseBody,
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(responseBody))
		if message == "" {
			message = response.Status
		}
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, "provider returned non-success status: "+message, providerRequestBody)
		return proxyResponse, prepared.traceID, nil
	}

	var providerResponse ChatCompletionResponse
	if err := json.Unmarshal(responseBody, &providerResponse); err != nil {
		s.recordSuccess(prepared, providerRequestBody, string(responseBody), "", 0, 0, 0, int(time.Since(startedAt).Milliseconds()), false)
		return proxyResponse, prepared.traceID, nil
	}

	assistantContent := extractAssistantContent(providerResponse)
	s.recordSuccess(prepared, providerRequestBody, string(responseBody), assistantContent, providerResponse.Usage.PromptTokens, providerResponse.Usage.CompletionTokens, providerResponse.Usage.TotalTokens, int(time.Since(startedAt).Milliseconds()), false)
	return proxyResponse, prepared.traceID, nil
}

func (s *service) callAnthropic(ctx context.Context, prepared *preparedChatRequest) (*ProxyResponse, string, error) {
	resolvedAPIKey, err := s.secretRefService.ResolveString(ctx, prepared.resolvedModel.APIKey, prepared.resolvedModel.APIKeySecretRef)
	if err != nil {
		return nil, prepared.traceID, err
	}

	providerRequestBody, err := buildProviderRequestBody(prepared.req, prepared.resolvedModel)
	if err != nil {
		return nil, prepared.traceID, err
	}

	httpRequest, err := buildProviderHTTPRequest(ctx, prepared.traceID, prepared.requestID, prepared.resolvedModel, providerRequestBody, resolvedAPIKey, false, prepared.customHeaderVariables())
	if err != nil {
		return nil, prepared.traceID, err
	}

	startedAt := time.Now()
	response, err := s.httpClient.Do(httpRequest)
	if err != nil {
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("provider call failed: %v", err), providerRequestBody)
		return nil, prepared.traceID, fmt.Errorf("failed to call provider: %w", err)
	}
	defer response.Body.Close()

	responseBody, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("failed to read provider response: %v", readErr), providerRequestBody)
		return nil, prepared.traceID, fmt.Errorf("failed to read provider response: %w", readErr)
	}

	proxyResponse := &ProxyResponse{
		StatusCode: response.StatusCode,
		Headers:    cloneProxyHeaders(response.Header),
		Body:       responseBody,
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(responseBody))
		if message == "" {
			message = response.Status
		}
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, "provider returned non-success status: "+message, providerRequestBody)
		return proxyResponse, prepared.traceID, nil
	}

	var providerResponse anthropicMessageResponse
	if err := json.Unmarshal(responseBody, &providerResponse); err != nil {
		s.recordSuccess(prepared, providerRequestBody, string(responseBody), "", 0, 0, 0, int(time.Since(startedAt).Milliseconds()), false)
		return proxyResponse, prepared.traceID, nil
	}

	normalizedBody, assistantContent, promptTokens, completionTokens, totalTokens, normalizeErr := normalizeAnthropicResponse(providerResponse)
	if normalizeErr != nil {
		s.recordSuccess(prepared, providerRequestBody, string(responseBody), "", providerResponse.Usage.InputTokens, providerResponse.Usage.OutputTokens, providerResponse.Usage.InputTokens+providerResponse.Usage.OutputTokens, int(time.Since(startedAt).Milliseconds()), false)
		return proxyResponse, prepared.traceID, nil
	}

	proxyResponse.Headers.Del("Content-Length")
	proxyResponse.Body = normalizedBody
	s.recordSuccess(prepared, providerRequestBody, string(normalizedBody), assistantContent, promptTokens, completionTokens, totalTokens, int(time.Since(startedAt).Milliseconds()), false)
	return proxyResponse, prepared.traceID, nil
}

func (s *service) recordFailure(traceID, requestID string, req ChatCompletionRequest, userID int, model *models.LLMModel, startedAt time.Time, failure string, providerRequestBody []byte) {
	completedAt := time.Now()
	latencyMs := int(time.Since(startedAt).Milliseconds())
	requestPayload := string(providerRequestBody)
	if strings.TrimSpace(requestPayload) == "" {
		requestPayload = rawOrJSONRequestPayload(req)
	}
	responsePayload := failure
	invocation := &models.ModelInvocation{
		TraceID:             traceID,
		SessionID:           req.SessionID,
		RequestID:           requestID,
		UserID:              intPtr(userID),
		InstanceID:          req.InstanceID,
		ModelID:             intPtr(model.ID),
		ProviderType:        model.ProviderType,
		RequestedModel:      req.Model,
		ActualProviderModel: model.ProviderModelName,
		TrafficClass:        models.TrafficClassLLM,
		RequestPayload:      &requestPayload,
		ResponsePayload:     &responsePayload,
		LatencyMs:           &latencyMs,
		IsStreaming:         req.Stream,
		Status:              models.ModelInvocationStatusFailed,
		ErrorMessage:        stringPtr(failure),
		CompletedAt:         &completedAt,
	}
	if err := s.invocationService.RecordInvocation(invocation); err != nil {
		logPersistenceError("record failed invocation", traceID, err)
	}

	providerRequestPayload := requestPayload
	if err := s.auditEventService.RecordEvent(&models.AuditEvent{
		TraceID:      traceID,
		SessionID:    req.SessionID,
		RequestID:    stringPtr(requestID),
		UserID:       intPtr(userID),
		InstanceID:   req.InstanceID,
		InvocationID: intPtr(invocation.ID),
		EventType:    "gateway.request.failed",
		TrafficClass: models.TrafficClassLLM,
		Severity:     models.AuditSeverityError,
		Message:      failure,
		Details:      &providerRequestPayload,
	}); err != nil {
		logPersistenceError("record gateway.request.failed", traceID, err)
	}
}

func (s *service) recordSuccess(prepared *preparedChatRequest, providerRequestBody []byte, responsePayload, assistantContent string, promptTokens, completionTokens, totalTokens, latencyMs int, isStreaming bool) {
	completedAt := time.Now()
	requestPayload := string(providerRequestBody)
	if strings.TrimSpace(requestPayload) == "" {
		requestPayload = rawOrJSONRequestPayload(prepared.req)
	}
	promptTokens, completionTokens, totalTokens, usageEstimated := resolveUsage(prepared.req.Messages, assistantContent, promptTokens, completionTokens, totalTokens)
	invocation := &models.ModelInvocation{
		TraceID:             prepared.traceID,
		SessionID:           prepared.sessionIDPtr,
		RequestID:           prepared.requestID,
		UserID:              prepared.userIDPtr,
		InstanceID:          prepared.req.InstanceID,
		ModelID:             intPtr(prepared.resolvedModel.ID),
		ProviderType:        prepared.resolvedModel.ProviderType,
		RequestedModel:      prepared.req.Model,
		ActualProviderModel: prepared.resolvedModel.ProviderModelName,
		TrafficClass:        models.TrafficClassLLM,
		RequestPayload:      &requestPayload,
		ResponsePayload:     &responsePayload,
		PromptTokens:        promptTokens,
		CompletionTokens:    completionTokens,
		TotalTokens:         totalTokens,
		LatencyMs:           &latencyMs,
		IsStreaming:         isStreaming,
		Status:              models.ModelInvocationStatusCompleted,
		CompletedAt:         &completedAt,
	}
	if err := s.invocationService.RecordInvocation(invocation); err != nil {
		logPersistenceError("record completed invocation", prepared.traceID, err)
	}
	if err := s.chatMessageService.RecordMessages(
		prepared.traceID,
		prepared.sessionID,
		prepared.requestIDPtr,
		prepared.userIDPtr,
		prepared.req.InstanceID,
		intPtr(invocation.ID),
		[]services.PersistedChatMessage{
			{
				Role:       "assistant",
				Content:    assistantContent,
				SequenceNo: len(prepared.req.Messages) + 1,
			},
		},
	); err != nil {
		logPersistenceError("record assistant messages", prepared.traceID, err)
	}

	estimatedCost := calculateEstimatedCost(prepared.resolvedModel, promptTokens, completionTokens)
	internalCost := 0.0
	if prepared.resolvedModel.IsSecure {
		internalCost = estimatedCost
	}
	if err := s.costRecordService.RecordCost(&models.CostRecord{
		TraceID:          prepared.traceID,
		SessionID:        prepared.req.SessionID,
		RequestID:        prepared.requestIDPtr,
		UserID:           prepared.userIDPtr,
		InstanceID:       prepared.req.InstanceID,
		InvocationID:     intPtr(invocation.ID),
		ModelID:          intPtr(prepared.resolvedModel.ID),
		ProviderType:     prepared.resolvedModel.ProviderType,
		ModelName:        prepared.resolvedModel.DisplayName,
		Currency:         fallbackCurrency(prepared.resolvedModel.Currency),
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		InputUnitPrice:   prepared.resolvedModel.InputPrice,
		OutputUnitPrice:  prepared.resolvedModel.OutputPrice,
		EstimatedCost:    estimatedCost,
		InternalCost:     internalCost,
	}); err != nil {
		logPersistenceError("record cost", prepared.traceID, err)
	}

	if err := s.auditEventService.RecordEvent(&models.AuditEvent{
		TraceID:      prepared.traceID,
		SessionID:    prepared.sessionIDPtr,
		RequestID:    prepared.requestIDPtr,
		UserID:       prepared.userIDPtr,
		InstanceID:   prepared.req.InstanceID,
		InvocationID: intPtr(invocation.ID),
		EventType:    "gateway.request.completed",
		TrafficClass: models.TrafficClassLLM,
		Severity:     models.AuditSeverityInfo,
		Message:      fmt.Sprintf("Completed LLM request for model %s", prepared.resolvedModel.DisplayName),
	}); err != nil {
		logPersistenceError("record gateway.request.completed", prepared.traceID, err)
	}
	if usageEstimated {
		if err := s.auditEventService.RecordEvent(&models.AuditEvent{
			TraceID:      prepared.traceID,
			SessionID:    prepared.sessionIDPtr,
			RequestID:    prepared.requestIDPtr,
			UserID:       prepared.userIDPtr,
			InstanceID:   prepared.req.InstanceID,
			InvocationID: intPtr(invocation.ID),
			EventType:    "gateway.usage.estimated",
			TrafficClass: models.TrafficClassLLM,
			Severity:     models.AuditSeverityWarn,
			Message:      "Provider usage was missing; token usage was estimated locally.",
		}); err != nil {
			logPersistenceError("record gateway.usage.estimated", prepared.traceID, err)
		}
	}
}

func (s *service) streamOpenAICompatible(ctx context.Context, prepared *preparedChatRequest, w http.ResponseWriter) error {
	resolvedAPIKey, err := s.secretRefService.ResolveString(ctx, prepared.resolvedModel.APIKey, prepared.resolvedModel.APIKeySecretRef)
	if err != nil {
		return err
	}

	providerRequestBody, err := buildProviderRequestBody(prepared.req, prepared.resolvedModel)
	if err != nil {
		return err
	}

	httpRequest, err := buildProviderHTTPRequest(ctx, prepared.traceID, prepared.requestID, prepared.resolvedModel, providerRequestBody, resolvedAPIKey, true, prepared.customHeaderVariables())
	if err != nil {
		return err
	}

	startedAt := time.Now()
	response, err := s.httpClient.Do(httpRequest)
	if err != nil {
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("provider call failed: %v", err), providerRequestBody)
		return fmt.Errorf("failed to call provider: %w", err)
	}
	defer response.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming response writer is not supported")
	}

	copyProxyHeaders(w.Header(), response.Header)
	w.Header().Set("X-Trace-ID", prepared.traceID)
	w.WriteHeader(response.StatusCode)
	flusher.Flush()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(response.Body)
		if len(responseBody) > 0 {
			_, _ = w.Write(responseBody)
			flusher.Flush()
		}
		message := strings.TrimSpace(string(responseBody))
		if message == "" {
			message = response.Status
		}
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, "provider returned non-success status: "+message, providerRequestBody)
		return nil
	}

	reader := bufio.NewReader(response.Body)
	var rawStream strings.Builder
	var assistantText strings.Builder
	promptTokens := 0
	completionTokens := 0
	totalTokens := 0
	streamFailed := false

	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			done := inspectStreamLine(line, &assistantText, &promptTokens, &completionTokens, &totalTokens)
			rawStream.WriteString(line)
			if _, err := io.WriteString(w, line); err == nil {
				flusher.Flush()
			}
			if done {
				break
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			streamFailed = true
			s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("failed while reading provider stream: %v", readErr), providerRequestBody)
			break
		}
	}

	if !streamFailed {
		assistantContent := assistantText.String()
		if strings.TrimSpace(assistantContent) == "" {
			assistantContent = rawStream.String()
		}
		s.recordSuccess(prepared, providerRequestBody, rawStream.String(), assistantContent, promptTokens, completionTokens, totalTokens, int(time.Since(startedAt).Milliseconds()), true)
	}
	return nil
}

func (s *service) streamAnthropic(ctx context.Context, prepared *preparedChatRequest, w http.ResponseWriter) error {
	resolvedAPIKey, err := s.secretRefService.ResolveString(ctx, prepared.resolvedModel.APIKey, prepared.resolvedModel.APIKeySecretRef)
	if err != nil {
		return err
	}

	providerRequestBody, err := buildProviderRequestBody(prepared.req, prepared.resolvedModel)
	if err != nil {
		return err
	}

	httpRequest, err := buildProviderHTTPRequest(ctx, prepared.traceID, prepared.requestID, prepared.resolvedModel, providerRequestBody, resolvedAPIKey, true, prepared.customHeaderVariables())
	if err != nil {
		return err
	}

	startedAt := time.Now()
	response, err := s.httpClient.Do(httpRequest)
	if err != nil {
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("provider call failed: %v", err), providerRequestBody)
		return fmt.Errorf("failed to call provider: %w", err)
	}
	defer response.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming response writer is not supported")
	}

	copyProxyHeaders(w.Header(), response.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Trace-ID", prepared.traceID)
	w.WriteHeader(response.StatusCode)
	flusher.Flush()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(response.Body)
		if len(responseBody) > 0 {
			_, _ = w.Write(responseBody)
			flusher.Flush()
		}
		message := strings.TrimSpace(string(responseBody))
		if message == "" {
			message = response.Status
		}
		s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, "provider returned non-success status: "+message, providerRequestBody)
		return nil
	}

	reader := bufio.NewReader(response.Body)
	var rawStream strings.Builder
	state := &anthropicStreamState{
		Created:   time.Now().Unix(),
		ToolCalls: map[int]*anthropicToolCallState{},
	}
	eventType := ""
	dataLines := make([]string, 0, 2)
	streamFailed := false
	done := false

	flushEvent := func() error {
		if len(dataLines) == 0 {
			eventType = ""
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		finished, err := processAnthropicStreamEvent(payload, eventType, state, w, flusher)
		eventType = ""
		if err != nil {
			return err
		}
		if finished {
			done = true
		}
		return nil
	}

	for !done {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			rawStream.WriteString(line)
			trimmedLine := strings.TrimRight(line, "\r\n")
			switch {
			case trimmedLine == "":
				if err := flushEvent(); err != nil {
					streamFailed = true
					s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("failed while processing provider stream: %v", err), providerRequestBody)
					break
				}
			case strings.HasPrefix(trimmedLine, "event:"):
				eventType = strings.TrimSpace(strings.TrimPrefix(trimmedLine, "event:"))
			case strings.HasPrefix(trimmedLine, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmedLine, "data:")))
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if err := flushEvent(); err != nil {
					streamFailed = true
					s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("failed while processing provider stream: %v", err), providerRequestBody)
				}
				break
			}
			streamFailed = true
			s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("failed while reading provider stream: %v", readErr), providerRequestBody)
			break
		}
	}

	if !streamFailed {
		finalChunk := openAIStreamResponseChunk{
			ID:      defaultIfBlank(state.ResponseID, "chatcmpl-anthropic"),
			Object:  "chat.completion.chunk",
			Created: state.Created,
			Model:   state.Model,
			Choices: []openAIStreamChoice{
				{
					Index: 0,
					Delta: openAIStreamDelta{},
				},
			},
			Usage: &struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}{
				PromptTokens:     state.PromptTokens,
				CompletionTokens: state.CompletionTokens,
				TotalTokens:      state.PromptTokens + state.CompletionTokens,
			},
		}
		if err := emitOpenAIStreamPayload(w, flusher, finalChunk); err != nil {
			s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("failed while writing normalized stream: %v", err), providerRequestBody)
			return nil
		}
		if err := emitOpenAIStreamDone(w, flusher); err != nil {
			s.recordFailure(prepared.traceID, prepared.requestID, prepared.req, userIDOrZero(prepared.userIDPtr), prepared.resolvedModel, startedAt, fmt.Sprintf("failed while closing normalized stream: %v", err), providerRequestBody)
			return nil
		}

		assistantContent := strings.TrimSpace(state.AssistantText.String())
		if assistantContent == "" {
			assistantContent = renderAnthropicToolCalls(state)
		}
		if assistantContent == "" {
			assistantContent = rawStream.String()
		}
		normalizedStream := rawStream.String()
		s.recordSuccess(prepared, providerRequestBody, normalizedStream, assistantContent, state.PromptTokens, state.CompletionTokens, state.PromptTokens+state.CompletionTokens, int(time.Since(startedAt).Milliseconds()), true)
	}
	return nil
}

func inspectStreamLine(line string, assistantText *strings.Builder, promptTokens, completionTokens, totalTokens *int) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return false
	}

	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" {
		return false
	}
	if payload == "[DONE]" {
		return true
	}

	var chunk openAIStreamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return false
	}

	if chunk.Usage != nil {
		*promptTokens = chunk.Usage.PromptTokens
		*completionTokens = chunk.Usage.CompletionTokens
		*totalTokens = chunk.Usage.TotalTokens
	}

	for _, choice := range chunk.Choices {
		content := flattenMessageContent(choice.Delta.Content)
		if content != "" {
			assistantText.WriteString(content)
		}
	}
	return false
}

func buildProviderRequestBody(req ChatCompletionRequest, model *models.LLMModel) ([]byte, error) {
	if model == nil {
		return nil, errors.New("model is not active or does not exist")
	}

	switch models.ResolveLLMProtocolTypeOrDefault(model.ProviderType, model.ProtocolType) {
	case models.ProtocolTypeAnthropic:
		return buildAnthropicRequestBody(req, model)
	default:
		return buildOpenAICompatibleRequestBody(req, model)
	}
}

func processAnthropicStreamEvent(payload, eventType string, state *anthropicStreamState, w io.Writer, flusher http.Flusher) (bool, error) {
	if strings.TrimSpace(payload) == "" {
		return false, nil
	}

	var event anthropicStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return false, nil
	}
	if strings.TrimSpace(eventType) == "" {
		eventType = event.Type
	}

	switch eventType {
	case "message_start":
		if event.Message != nil {
			state.ResponseID = event.Message.ID
			state.Model = event.Message.Model
			state.PromptTokens = event.Message.Usage.InputTokens
		}
	case "content_block_start":
		if event.ContentBlock == nil || event.Index == nil {
			return false, nil
		}
		index := *event.Index
		switch event.ContentBlock.Type {
		case "text":
			chunk := openAIStreamResponseChunk{
				ID:      defaultIfBlank(state.ResponseID, "chatcmpl-anthropic"),
				Object:  "chat.completion.chunk",
				Created: state.Created,
				Model:   state.Model,
				Choices: []openAIStreamChoice{
					{
						Index: 0,
						Delta: openAIStreamDelta{
							Role: conditionalAssistantRole(state),
						},
					},
				},
			}
			if err := emitOpenAIStreamPayload(w, flusher, chunk); err != nil {
				return false, err
			}
		case "tool_use":
			toolState := &anthropicToolCallState{
				ID:   defaultIfBlank(event.ContentBlock.ID, fmt.Sprintf("call_%d", index)),
				Name: strings.TrimSpace(event.ContentBlock.Name),
			}
			state.ToolCalls[index] = toolState
			toolCallIndex := index
			chunk := openAIStreamResponseChunk{
				ID:      defaultIfBlank(state.ResponseID, "chatcmpl-anthropic"),
				Object:  "chat.completion.chunk",
				Created: state.Created,
				Model:   state.Model,
				Choices: []openAIStreamChoice{
					{
						Index: 0,
						Delta: openAIStreamDelta{
							Role: conditionalAssistantRole(state),
							ToolCalls: []ToolCall{
								{
									ID:    toolState.ID,
									Type:  "function",
									Index: &toolCallIndex,
									Function: &ToolCallFunction{
										Name:      toolState.Name,
										Arguments: "",
									},
								},
							},
						},
					},
				},
			}
			if err := emitOpenAIStreamPayload(w, flusher, chunk); err != nil {
				return false, err
			}
		}
	case "content_block_delta":
		if event.Index == nil || event.Delta == nil {
			return false, nil
		}
		index := *event.Index
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text == "" {
				return false, nil
			}
			state.AssistantText.WriteString(event.Delta.Text)
			chunk := openAIStreamResponseChunk{
				ID:      defaultIfBlank(state.ResponseID, "chatcmpl-anthropic"),
				Object:  "chat.completion.chunk",
				Created: state.Created,
				Model:   state.Model,
				Choices: []openAIStreamChoice{
					{
						Index: 0,
						Delta: openAIStreamDelta{
							Content: event.Delta.Text,
						},
					},
				},
			}
			if err := emitOpenAIStreamPayload(w, flusher, chunk); err != nil {
				return false, err
			}
		case "input_json_delta":
			toolState := state.ToolCalls[index]
			if toolState == nil {
				return false, nil
			}
			toolState.Arguments.WriteString(event.Delta.PartialJSON)
			toolCallIndex := index
			chunk := openAIStreamResponseChunk{
				ID:      defaultIfBlank(state.ResponseID, "chatcmpl-anthropic"),
				Object:  "chat.completion.chunk",
				Created: state.Created,
				Model:   state.Model,
				Choices: []openAIStreamChoice{
					{
						Index: 0,
						Delta: openAIStreamDelta{
							ToolCalls: []ToolCall{
								{
									ID:    toolState.ID,
									Type:  "function",
									Index: &toolCallIndex,
									Function: &ToolCallFunction{
										Arguments: event.Delta.PartialJSON,
									},
								},
							},
						},
					},
				},
			}
			if err := emitOpenAIStreamPayload(w, flusher, chunk); err != nil {
				return false, err
			}
		}
	case "message_delta":
		if event.Usage != nil {
			state.CompletionTokens = event.Usage.OutputTokens
		}
		if event.Delta != nil {
			finishReason := normalizeAnthropicStopReason(event.Delta.StopReason)
			if finishReason != "" {
				chunk := openAIStreamResponseChunk{
					ID:      defaultIfBlank(state.ResponseID, "chatcmpl-anthropic"),
					Object:  "chat.completion.chunk",
					Created: state.Created,
					Model:   state.Model,
					Choices: []openAIStreamChoice{
						{
							Index:        0,
							Delta:        openAIStreamDelta{},
							FinishReason: &finishReason,
						},
					},
				}
				if err := emitOpenAIStreamPayload(w, flusher, chunk); err != nil {
					return false, err
				}
			}
		}
	case "message_stop":
		return true, nil
	case "error":
		if event.Error != nil && strings.TrimSpace(event.Error.Message) != "" {
			return false, errors.New(event.Error.Message)
		}
	}

	return false, nil
}

func emitOpenAIStreamPayload(w io.Writer, flusher http.Flusher, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func emitOpenAIStreamDone(w io.Writer, flusher http.Flusher) error {
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func conditionalAssistantRole(state *anthropicStreamState) string {
	if state.SentRole {
		return ""
	}
	state.SentRole = true
	return "assistant"
}

func buildOpenAICompatibleRequestBody(req ChatCompletionRequest, model *models.LLMModel) ([]byte, error) {
	if model == nil {
		return nil, errors.New("model is not active or does not exist")
	}
	payload := map[string]json.RawMessage{}
	if len(req.RawBody) > 0 {
		if err := json.Unmarshal(req.RawBody, &payload); err != nil {
			return nil, fmt.Errorf("failed to decode provider request: %w", err)
		}
	}
	if payload == nil {
		payload = map[string]json.RawMessage{}
	}
	delete(payload, "session_id")
	delete(payload, "instance_id")
	delete(payload, "trace_id")
	delete(payload, "request_id")
	modelPayload, err := json.Marshal(model.ProviderModelName)
	if err != nil {
		return nil, fmt.Errorf("failed to encode provider model name: %w", err)
	}
	payload["model"] = json.RawMessage(modelPayload)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to encode provider request: %w", err)
	}
	return body, nil
}

func buildAnthropicRequestBody(req ChatCompletionRequest, model *models.LLMModel) ([]byte, error) {
	systemPrompt, messages := convertChatMessagesToAnthropic(req.Messages)
	tools, err := convertToolsToAnthropic(req.Tools)
	if err != nil {
		return nil, err
	}
	toolChoice, err := convertToolChoiceToAnthropic(req.ToolChoice)
	if err != nil {
		return nil, err
	}

	stopSequences, err := convertStopSequences(req.Stop)
	if err != nil {
		return nil, err
	}

	maxTokens := defaultAnthropicMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}

	payload := anthropicRequestPayload{
		Model:       model.ProviderModelName,
		Messages:    messages,
		System:      systemPrompt,
		MaxTokens:   maxTokens,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		StopSeqs:    stopSequences,
	}
	if len(tools) > 0 {
		payload.Tools = tools
		if toolChoice != nil {
			payload.ToolChoice = toolChoice
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to encode anthropic request: %w", err)
	}
	return body, nil
}

func buildProviderHTTPRequest(ctx context.Context, traceID, requestID string, model *models.LLMModel, providerRequestBody []byte, resolvedAPIKey *string, acceptStream bool, variables map[string]string) (*http.Request, error) {
	if model == nil {
		return nil, errors.New("model is not active or does not exist")
	}

	var httpRequest *http.Request
	var err error
	switch models.ResolveLLMProtocolTypeOrDefault(model.ProviderType, model.ProtocolType) {
	case models.ProtocolTypeAnthropic:
		httpRequest, err = buildAnthropicProviderHTTPRequest(ctx, traceID, requestID, model, providerRequestBody, resolvedAPIKey, acceptStream)
	default:
		httpRequest, err = buildOpenAICompatibleProviderHTTPRequest(ctx, traceID, requestID, model, providerRequestBody, resolvedAPIKey, acceptStream)
	}
	if err != nil {
		return nil, err
	}
	if err := applyCustomProviderHeaders(httpRequest, model, variables); err != nil {
		return nil, err
	}
	return httpRequest, nil
}

func applyCustomProviderHeaders(httpRequest *http.Request, model *models.LLMModel, variables map[string]string) error {
	if httpRequest == nil || model == nil {
		return nil
	}

	headers, err := models.ParseLLMModelCustomHeaders(model.CustomHeadersJSON)
	if err != nil {
		return fmt.Errorf("custom header config is invalid: %w", err)
	}
	for _, header := range headers {
		key := strings.TrimSpace(header.Key)
		if key == "" {
			continue
		}
		if strings.ContainsAny(key, "\r\n") || !customHeaderNamePattern.MatchString(key) {
			return fmt.Errorf("custom header key is invalid: %s", key)
		}
		value := renderCustomHeaderValue(header.Value, variables)
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("custom header value for %s cannot contain newlines", key)
		}
		httpRequest.Header.Set(key, value)
	}
	return nil
}

func renderCustomHeaderValue(template string, variables map[string]string) string {
	if strings.TrimSpace(template) == "" || len(variables) == 0 {
		return template
	}

	return customHeaderTemplatePattern.ReplaceAllStringFunc(template, func(match string) string {
		parts := customHeaderTemplatePattern.FindStringSubmatch(match)
		if len(parts) < 3 {
			return ""
		}
		key := strings.TrimSpace(parts[1])
		if key == "" {
			key = strings.TrimSpace(parts[2])
		}
		if key == "" {
			return ""
		}
		return variables[strings.ToLower(key)]
	})
}

func (p *preparedChatRequest) customHeaderVariables() map[string]string {
	variables := map[string]string{}
	if p == nil {
		return variables
	}

	addHeaderVariable(variables, "user.id", strconv.Itoa(p.userID))
	addHeaderVariable(variables, "user_id", strconv.Itoa(p.userID))
	addHeaderVariable(variables, "clawmanager.user_id", strconv.Itoa(p.userID))
	addHeaderVariable(variables, "CLAWMANAGER_USER_ID", strconv.Itoa(p.userID))
	addHeaderVariable(variables, "request.trace_id", p.traceID)
	addHeaderVariable(variables, "trace_id", p.traceID)
	addHeaderVariable(variables, "clawmanager.trace_id", p.traceID)
	addHeaderVariable(variables, "CLAWMANAGER_TRACE_ID", p.traceID)
	addHeaderVariable(variables, "request.request_id", p.requestID)
	addHeaderVariable(variables, "request_id", p.requestID)
	addHeaderVariable(variables, "clawmanager.request_id", p.requestID)
	addHeaderVariable(variables, "CLAWMANAGER_REQUEST_ID", p.requestID)
	addHeaderVariable(variables, "request.session_id", p.sessionID)
	addHeaderVariable(variables, "session_id", p.sessionID)
	addHeaderVariable(variables, "clawmanager.session_id", p.sessionID)
	addHeaderVariable(variables, "CLAWMANAGER_SESSION_ID", p.sessionID)
	addHeaderVariable(variables, "request.model", p.req.Model)
	addHeaderVariable(variables, "requested_model", p.req.Model)

	if p.resolvedModel != nil {
		addHeaderVariable(variables, "model.id", strconv.Itoa(p.resolvedModel.ID))
		addHeaderVariable(variables, "model.display_name", p.resolvedModel.DisplayName)
		addHeaderVariable(variables, "model.provider_type", p.resolvedModel.ProviderType)
		addHeaderVariable(variables, "model.protocol_type", p.resolvedModel.ProtocolType)
		addHeaderVariable(variables, "model.provider_model_name", p.resolvedModel.ProviderModelName)
		addHeaderVariable(variables, "provider_model_name", p.resolvedModel.ProviderModelName)
		addHeaderVariable(variables, "CLAWMANAGER_MODEL", p.resolvedModel.DisplayName)
		addHeaderVariable(variables, "CLAWMANAGER_PROVIDER_MODEL", p.resolvedModel.ProviderModelName)
	}

	instanceID := 0
	if p.req.InstanceID != nil {
		instanceID = *p.req.InstanceID
	}
	if p.instance != nil {
		instanceID = p.instance.ID
	}
	if instanceID > 0 {
		instanceIDText := strconv.Itoa(instanceID)
		addHeaderVariable(variables, "instance.id", instanceIDText)
		addHeaderVariable(variables, "instance_id", instanceIDText)
		addHeaderVariable(variables, "openclaw.instance_id", instanceIDText)
		addHeaderVariable(variables, "clawmanager.instance_id", instanceIDText)
		addHeaderVariable(variables, "OPENCLAW_INSTANCE_ID", instanceIDText)
		addHeaderVariable(variables, "CLAWMANAGER_INSTANCE_ID", instanceIDText)
	}
	if p.instance != nil {
		addInstanceHeaderVariables(variables, p.instance)
	}

	return variables
}

func addInstanceHeaderVariables(variables map[string]string, instance *models.Instance) {
	if instance == nil {
		return
	}

	addHeaderVariable(variables, "instance.user_id", strconv.Itoa(instance.UserID))
	addHeaderVariable(variables, "instance.name", instance.Name)
	addHeaderVariable(variables, "instance.type", instance.Type)
	addHeaderVariable(variables, "instance.status", instance.Status)
	addHeaderVariable(variables, "instance.cpu_cores", strconv.FormatFloat(instance.CPUCores, 'f', -1, 64))
	addHeaderVariable(variables, "instance.memory_gb", strconv.Itoa(instance.MemoryGB))
	addHeaderVariable(variables, "instance.disk_gb", strconv.Itoa(instance.DiskGB))
	addHeaderVariable(variables, "instance.gpu_enabled", strconv.FormatBool(instance.GPUEnabled))
	addHeaderVariable(variables, "instance.gpu_count", strconv.Itoa(instance.GPUCount))
	addHeaderVariable(variables, "instance.os_type", instance.OSType)
	addHeaderVariable(variables, "instance.os_version", instance.OSVersion)
	addHeaderVariable(variables, "instance.storage_class", instance.StorageClass)
	addHeaderVariable(variables, "instance.mount_path", instance.MountPath)
	addOptionalHeaderVariable(variables, "instance.image_registry", instance.ImageRegistry)
	addOptionalHeaderVariable(variables, "instance.image_tag", instance.ImageTag)
	addOptionalHeaderVariable(variables, "instance.pod_name", instance.PodName)
	addOptionalHeaderVariable(variables, "instance.pod_namespace", instance.PodNamespace)
	addOptionalHeaderVariable(variables, "instance.pod_ip", instance.PodIP)
	addHeaderVariable(variables, "openclaw.instance_name", instance.Name)
	addHeaderVariable(variables, "clawmanager.instance_name", instance.Name)
	addHeaderVariable(variables, "OPENCLAW_INSTANCE_NAME", instance.Name)
	addHeaderVariable(variables, "CLAWMANAGER_INSTANCE_NAME", instance.Name)
	addInstanceEnvHeaderVariables(variables, instance.EnvironmentOverridesJSON)
}

func addInstanceEnvHeaderVariables(variables map[string]string, raw *string) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return
	}

	var env map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(*raw)), &env); err != nil {
		return
	}
	for key, value := range env {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		addHeaderVariable(variables, "env."+key, value)
		addHeaderVariable(variables, "instance.env."+key, value)
		addHeaderVariable(variables, "clawmanager.env."+key, value)
	}
}

func addOptionalHeaderVariable(variables map[string]string, key string, value *string) {
	if value == nil {
		return
	}
	addHeaderVariable(variables, key, *value)
}

func addHeaderVariable(variables map[string]string, key, value string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	variables[strings.ToLower(key)] = value
}

func buildOpenAICompatibleProviderHTTPRequest(ctx context.Context, traceID, requestID string, model *models.LLMModel, providerRequestBody []byte, resolvedAPIKey *string, acceptStream bool) (*http.Request, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(model.BaseURL), "/") + "/chat/completions"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(providerRequestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to build provider request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if acceptStream {
		httpRequest.Header.Set("Accept", "text/event-stream")
	} else {
		httpRequest.Header.Set("Accept", "application/json")
	}
	httpRequest.Header.Set("X-Trace-ID", traceID)
	httpRequest.Header.Set("X-Request-ID", requestID)
	if resolvedAPIKey != nil && strings.TrimSpace(*resolvedAPIKey) != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*resolvedAPIKey))
	}
	return httpRequest, nil
}

func buildAnthropicProviderHTTPRequest(ctx context.Context, traceID, requestID string, model *models.LLMModel, providerRequestBody []byte, resolvedAPIKey *string, acceptStream bool) (*http.Request, error) {
	endpoint, err := buildProviderAPIEndpoint(model.BaseURL, "v1", "messages")
	if err != nil {
		return nil, err
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(providerRequestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to build provider request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("anthropic-version", anthropicVersionHeader)
	if acceptStream {
		httpRequest.Header.Set("Accept", "text/event-stream")
	} else {
		httpRequest.Header.Set("Accept", "application/json")
	}
	httpRequest.Header.Set("X-Trace-ID", traceID)
	httpRequest.Header.Set("X-Request-ID", requestID)
	if resolvedAPIKey != nil && strings.TrimSpace(*resolvedAPIKey) != "" {
		httpRequest.Header.Set("x-api-key", strings.TrimSpace(*resolvedAPIKey))
	}
	return httpRequest, nil
}

func buildProviderAPIEndpoint(baseURL, versionPrefix, resource string) (string, error) {
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
	if providerVersionSegmentPattern.MatchString(lastSegment) {
		return trimmed + "/" + resourcePath, nil
	}

	return trimmed + versionPath + "/" + resourcePath, nil
}

func convertChatMessagesToAnthropic(messages []ChatMessage) (string, []anthropicRequestMessage) {
	systemParts := make([]string, 0)
	converted := make([]anthropicRequestMessage, 0, len(messages))

	appendMessage := func(role string, blocks []anthropicContentBlock) {
		if len(blocks) == 0 {
			return
		}
		if len(converted) > 0 && converted[len(converted)-1].Role == role {
			converted[len(converted)-1].Content = append(converted[len(converted)-1].Content, blocks...)
			return
		}
		converted = append(converted, anthropicRequestMessage{
			Role:    role,
			Content: blocks,
		})
	}

	for _, message := range messages {
		role := strings.TrimSpace(strings.ToLower(message.Role))
		switch role {
		case "system":
			if text := strings.TrimSpace(flattenChatMessage(message)); text != "" {
				systemParts = append(systemParts, text)
			}
		case "assistant":
			appendMessage("assistant", convertAssistantMessageToAnthropicBlocks(message))
		case "tool":
			appendMessage("user", convertToolMessageToAnthropicBlocks(message))
		default:
			appendMessage("user", convertUserMessageToAnthropicBlocks(message))
		}
	}

	return strings.Join(systemParts, "\n\n"), converted
}

func convertUserMessageToAnthropicBlocks(message ChatMessage) []anthropicContentBlock {
	blocks := convertContentToAnthropicTextBlocks(message.Content)
	if len(blocks) > 0 {
		return blocks
	}
	fallback := strings.TrimSpace(flattenChatMessage(message))
	if fallback == "" {
		return nil
	}
	return []anthropicContentBlock{{Type: "text", Text: fallback}}
}

func convertAssistantMessageToAnthropicBlocks(message ChatMessage) []anthropicContentBlock {
	blocks := convertContentToAnthropicTextBlocks(message.Content)
	for _, toolCall := range message.ToolCalls {
		if toolCall.Function == nil {
			continue
		}
		blocks = append(blocks, anthropicContentBlock{
			Type:  "tool_use",
			ID:    defaultIfBlank(toolCall.ID, "tool_call"),
			Name:  strings.TrimSpace(toolCall.Function.Name),
			Input: parseToolCallArguments(toolCall.Function.Arguments),
		})
	}
	return blocks
}

func convertToolMessageToAnthropicBlocks(message ChatMessage) []anthropicContentBlock {
	toolUseID := strings.TrimSpace(message.ToolCallID)
	content := strings.TrimSpace(flattenMessageContent(message.Content))
	if content == "" {
		content = mustJSONString(message.Content)
	}
	if toolUseID == "" && content == "" {
		return nil
	}
	return []anthropicContentBlock{
		{
			Type:      "tool_result",
			ToolUseID: toolUseID,
			Content:   defaultIfBlank(content, "{}"),
		},
	}
}

func convertContentToAnthropicTextBlocks(content interface{}) []anthropicContentBlock {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []anthropicContentBlock{{Type: "text", Text: value}}
	case []interface{}:
		blocks := make([]anthropicContentBlock, 0, len(value))
		for _, item := range value {
			text := strings.TrimSpace(flattenStructuredContentPart(item))
			if text == "" {
				continue
			}
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
		}
		if len(blocks) > 0 {
			return blocks
		}
	case map[string]interface{}:
		if text := strings.TrimSpace(flattenStructuredContentPart(value)); text != "" {
			return []anthropicContentBlock{{Type: "text", Text: text}}
		}
	}

	flattened := strings.TrimSpace(flattenMessageContent(content))
	if flattened == "" {
		return nil
	}
	return []anthropicContentBlock{{Type: "text", Text: flattened}}
}

func convertToolsToAnthropic(raw json.RawMessage) ([]anthropicToolDefinition, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	var items []map[string]interface{}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("failed to decode provider tools: %w", err)
	}

	tools := make([]anthropicToolDefinition, 0, len(items))
	for _, item := range items {
		itemType, _ := item["type"].(string)
		if strings.TrimSpace(itemType) != "" && !strings.EqualFold(strings.TrimSpace(itemType), "function") {
			continue
		}
		functionValue, ok := item["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := functionValue["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		description, _ := functionValue["description"].(string)
		inputSchema := functionValue["parameters"]
		if inputSchema == nil {
			inputSchema = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
		}
		tools = append(tools, anthropicToolDefinition{
			Name:        strings.TrimSpace(name),
			Description: strings.TrimSpace(description),
			InputSchema: inputSchema,
		})
	}

	return tools, nil
}

func convertToolChoiceToAnthropic(raw json.RawMessage) (map[string]interface{}, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode tool choice: %w", err)
	}

	switch value := parsed.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "none":
			return nil, nil
		case "required":
			return map[string]interface{}{"type": "any"}, nil
		case "auto", "any":
			return map[string]interface{}{"type": strings.ToLower(strings.TrimSpace(value))}, nil
		}
	case map[string]interface{}:
		choiceType, _ := value["type"].(string)
		switch strings.ToLower(strings.TrimSpace(choiceType)) {
		case "none", "":
			return nil, nil
		case "function":
			functionValue, _ := value["function"].(map[string]interface{})
			name, _ := functionValue["name"].(string)
			if strings.TrimSpace(name) == "" {
				return nil, nil
			}
			return map[string]interface{}{"type": "tool", "name": strings.TrimSpace(name)}, nil
		case "tool":
			return value, nil
		case "auto", "any":
			return map[string]interface{}{"type": strings.ToLower(strings.TrimSpace(choiceType))}, nil
		}
	}

	return nil, nil
}

func convertStopSequences(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		single = strings.TrimSpace(single)
		if single == "" {
			return nil, nil
		}
		return []string{single}, nil
	}

	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("failed to decode stop sequences: %w", err)
	}

	filtered := make([]string, 0, len(many))
	for _, item := range many {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	return filtered, nil
}

func parseToolCallArguments(raw string) interface{} {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]interface{}{}
	}

	var parsed interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return map[string]interface{}{
			"raw": trimmed,
		}
	}
	return parsed
}

func normalizeAnthropicResponse(response anthropicMessageResponse) ([]byte, string, int, int, int, error) {
	normalized := ChatCompletionResponse{
		ID:      defaultIfBlank(response.ID, "chatcmpl-anthropic"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   response.Model,
		Usage: struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		}{
			PromptTokens:     response.Usage.InputTokens,
			CompletionTokens: response.Usage.OutputTokens,
			TotalTokens:      response.Usage.InputTokens + response.Usage.OutputTokens,
		},
	}

	contentValue, toolCalls := convertAnthropicBlocksToOpenAIMessage(response.Content)
	normalized.Choices = []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string      `json:"role"`
			Content   interface{} `json:"content"`
			ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
			Refusal   interface{} `json:"refusal,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason,omitempty"`
	}{
		{
			Index: 0,
			Message: struct {
				Role      string      `json:"role"`
				Content   interface{} `json:"content"`
				ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
				Refusal   interface{} `json:"refusal,omitempty"`
			}{
				Role:      defaultIfBlank(response.Role, "assistant"),
				Content:   contentValue,
				ToolCalls: toolCalls,
			},
			FinishReason: normalizeAnthropicStopReason(response.StopReason),
		},
	}

	body, err := json.Marshal(normalized)
	if err != nil {
		return nil, "", 0, 0, 0, err
	}

	return body, extractAssistantContent(normalized), normalized.Usage.PromptTokens, normalized.Usage.CompletionTokens, normalized.Usage.TotalTokens, nil
}

func convertAnthropicBlocksToOpenAIMessage(blocks []anthropicContentBlock) (interface{}, []ToolCall) {
	textParts := make([]string, 0)
	toolCalls := make([]ToolCall, 0)
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				textParts = append(textParts, block.Text)
			}
		case "tool_use":
			inputPayload := "{}"
			if block.Input != nil {
				if encoded, err := json.Marshal(block.Input); err == nil {
					inputPayload = string(encoded)
				}
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:   defaultIfBlank(block.ID, "tool_call"),
				Type: "function",
				Function: &ToolCallFunction{
					Name:      strings.TrimSpace(block.Name),
					Arguments: inputPayload,
				},
			})
		}
	}

	if len(textParts) > 0 {
		return strings.Join(textParts, "\n"), toolCalls
	}
	if len(toolCalls) > 0 {
		return nil, toolCalls
	}
	return "", nil
}

func normalizeAnthropicStopReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return ""
	}
}

func renderAnthropicToolCalls(state *anthropicStreamState) string {
	if len(state.ToolCalls) == 0 {
		return ""
	}

	indexes := make([]int, 0, len(state.ToolCalls))
	for index := range state.ToolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	parts := make([]string, 0, len(state.ToolCalls))
	for _, index := range indexes {
		toolState := state.ToolCalls[index]
		if toolState == nil {
			continue
		}
		args := strings.TrimSpace(toolState.Arguments.String())
		switch {
		case toolState.Name != "" && args != "":
			parts = append(parts, fmt.Sprintf("tool_call %s(%s)", toolState.Name, args))
		case toolState.Name != "":
			parts = append(parts, "tool_call "+toolState.Name)
		}
	}
	return strings.Join(parts, "\n")
}

func defaultIfBlank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (s *service) recordBlockedInvocation(traceID, requestID string, req ChatCompletionRequest, userID int, model *models.LLMModel, reason string) *int {
	requestPayload := mustJSONString(req)
	responsePayload := reason
	invocation := &models.ModelInvocation{
		TraceID:             traceID,
		SessionID:           req.SessionID,
		RequestID:           requestID,
		UserID:              intPtr(userID),
		InstanceID:          req.InstanceID,
		ModelID:             intPtr(model.ID),
		ProviderType:        model.ProviderType,
		RequestedModel:      req.Model,
		ActualProviderModel: model.ProviderModelName,
		TrafficClass:        models.TrafficClassLLM,
		RequestPayload:      &requestPayload,
		ResponsePayload:     &responsePayload,
		IsStreaming:         false,
		Status:              models.ModelInvocationStatusBlocked,
		ErrorMessage:        stringPtr(reason),
	}
	if err := s.invocationService.RecordInvocation(invocation); err != nil {
		return nil
	}
	return intPtr(invocation.ID)
}

func (s *service) resolveTargetModel(selectedModel *models.LLMModel, analysis services.RiskAnalysis) (*models.LLMModel, string, error) {
	if selectedModel == nil {
		return nil, models.RiskActionAllow, errors.New("model is not active or does not exist")
	}
	if !analysis.IsSensitive {
		return selectedModel, models.RiskActionAllow, nil
	}
	if analysis.HighestAction == "require_approval" || analysis.HighestAction == models.RiskActionBlock {
		return nil, models.RiskActionBlock, errors.New("request was blocked by risk policy")
	}
	if selectedModel.IsSecure {
		return selectedModel, models.RiskActionAllow, nil
	}

	activeModels, err := s.modelRepo.ListActive()
	if err != nil {
		return nil, models.RiskActionBlock, fmt.Errorf("failed to list active secure models: %w", err)
	}
	for _, item := range activeModels {
		if item.IsSecure {
			resolved := item
			return &resolved, models.RiskActionRouteSecureModel, nil
		}
	}
	return nil, models.RiskActionBlock, errors.New("sensitive content requires an active secure model")
}

func (s *service) resolveRequestedModel(requestedModel string) (*models.LLMModel, error) {
	if isAutoModelRequest(requestedModel) {
		return s.selectAutoModel()
	}

	selectedModel, err := s.modelRepo.GetByDisplayName(requestedModel)
	if err != nil {
		return nil, fmt.Errorf("failed to get model: %w", err)
	}
	if selectedModel == nil || !selectedModel.IsActive {
		return nil, errors.New("model is not active or does not exist")
	}
	return selectedModel, nil
}

func (s *service) selectAutoModel() (*models.LLMModel, error) {
	items, err := s.modelRepo.ListActive()
	if err != nil {
		return nil, fmt.Errorf("failed to list active models: %w", err)
	}
	if len(items) == 0 {
		return nil, errors.New("no active models are configured")
	}

	for _, item := range items {
		if !item.IsSecure {
			selected := item
			return &selected, nil
		}
	}

	selected := items[0]
	return &selected, nil
}

func isAutoModelRequest(requestedModel string) bool {
	return strings.EqualFold(strings.TrimSpace(requestedModel), autoModelID)
}

func calculateEstimatedCost(model *models.LLMModel, promptTokens, completionTokens int) float64 {
	return (float64(promptTokens) * model.InputPrice / 1_000_000.0) + (float64(completionTokens) * model.OutputPrice / 1_000_000.0)
}

func normalizeOrCreateID(value *string, prefix string) string {
	if value != nil {
		if normalized := normalizeExistingIdentifier(*value, prefix); normalized != "" {
			return normalized
		}
	}
	return prefix + "_" + randomHex(8)
}

func normalizeExistingIdentifier(rawValue string, prefix string) string {
	trimmed := strings.TrimSpace(rawValue)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= maxStoredIdentifierLength {
		return trimmed
	}

	sum := sha256.Sum256([]byte(trimmed))
	normalized := prefix + "_" + hex.EncodeToString(sum[:16])
	if len(normalized) > maxStoredIdentifierLength {
		return normalized[:maxStoredIdentifierLength]
	}
	return normalized
}

func (s *service) resolveTraceID(userID int, req ChatCompletionRequest, sessionID string) string {
	if req.TraceID != nil {
		if normalized := normalizeExistingIdentifier(*req.TraceID, "trc"); normalized != "" {
			return normalized
		}
	}

	toolReferenceIDs := extractToolReferenceIDs(req.Messages)
	if len(toolReferenceIDs) == 0 {
		return normalizeOrCreateID(nil, "trc")
	}

	normalizedSessionID := strings.TrimSpace(sessionID)
	if normalizedSessionID != "" {
		if session, err := s.chatSessionService.GetSession(normalizedSessionID); err == nil && session != nil && session.LastTraceID != nil {
			lastTraceID := normalizeExistingIdentifier(*session.LastTraceID, "trc")
			if lastTraceID != "" {
				if instanceIDsCompatible(req.InstanceID, session.InstanceID) {
					return lastTraceID
				}
				items, listErr := s.invocationService.ListInvocationsByTraceID(lastTraceID)
				if listErr != nil {
					logPersistenceError("load session last trace invocations", lastTraceID, listErr)
				} else if invocationsContainAnyToolReference(items, toolReferenceIDs) {
					return lastTraceID
				}
			}
		} else if err != nil {
			logPersistenceError("load chat session for trace reuse", normalizedSessionID, err)
		}

		items, err := s.invocationService.ListInvocationsBySessionID(normalizedSessionID, 100)
		if err != nil {
			logPersistenceError("list invocations by session for trace reuse", normalizedSessionID, err)
		} else {
			for _, invocation := range items {
				if strings.TrimSpace(invocation.TraceID) == "" || invocation.ResponsePayload == nil {
					continue
				}
				if req.InstanceID != nil && invocation.InstanceID != nil && *req.InstanceID != *invocation.InstanceID {
					continue
				}
				if payloadContainsAny(*invocation.ResponsePayload, toolReferenceIDs) {
					return normalizeExistingIdentifier(invocation.TraceID, "trc")
				}
			}
		}
	}

	items, err := s.invocationService.ListInvocationsByUserID(userID, 200)
	if err != nil {
		logPersistenceError("list invocations for trace reuse", "", err)
		return normalizeOrCreateID(nil, "trc")
	}

	for _, invocation := range items {
		if strings.TrimSpace(invocation.TraceID) == "" || invocation.ResponsePayload == nil {
			continue
		}
		if req.InstanceID != nil && invocation.InstanceID != nil && *req.InstanceID != *invocation.InstanceID {
			continue
		}
		if payloadContainsAny(*invocation.ResponsePayload, toolReferenceIDs) {
			return normalizeExistingIdentifier(invocation.TraceID, "trc")
		}
	}

	return normalizeOrCreateID(nil, "trc")
}

func randomHex(size int) string {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

func intPtr(value int) *int {
	return &value
}

func userIDOrZero(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func instanceIDsCompatible(requestInstanceID, sessionInstanceID *int) bool {
	if requestInstanceID == nil || sessionInstanceID == nil {
		return true
	}
	return *requestInstanceID == *sessionInstanceID
}

func stringPtr(value string) *string {
	return &value
}

func mustJSONString(value interface{}) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func rawOrJSONRequestPayload(req ChatCompletionRequest) string {
	if len(req.RawBody) > 0 {
		return string(req.RawBody)
	}
	return mustJSONString(req)
}

func cloneProxyHeaders(headers http.Header) http.Header {
	cloned := make(http.Header)
	copyProxyHeaders(cloned, headers)
	return cloned
}

func copyProxyHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(header string) bool {
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func fallbackCurrency(currency string) string {
	if trimmed := strings.TrimSpace(currency); trimmed != "" {
		return trimmed
	}
	return "USD"
}

func resolveSessionID(req ChatCompletionRequest) string {
	if normalized := normalizeOptionalString(req.SessionID); normalized != "" {
		return normalizeExistingIdentifier(normalized, "sess")
	}
	if normalized := normalizeOptionalString(req.User); normalized != "" {
		return normalizeExistingIdentifier(normalized, "sess")
	}
	return ""
}

func normalizeSessionID(value *string, traceID string) string {
	if value != nil {
		if normalized := normalizeExistingIdentifier(*value, "sess"); normalized != "" {
			return normalized
		}
	}
	return normalizeExistingIdentifier("sess_"+traceID, "sess")
}

func deriveSessionTitle(messages []ChatMessage) *string {
	for _, message := range messages {
		if strings.TrimSpace(message.Role) != "user" {
			continue
		}
		content := strings.TrimSpace(flattenChatMessage(message))
		if content == "" {
			continue
		}
		runes := []rune(content)
		if len(runes) > 72 {
			content = string(runes[:72])
		}
		return &content
	}
	return nil
}

func buildPersistedMessages(messages []ChatMessage) []services.PersistedChatMessage {
	currentTurn := currentTurnMessages(messages)
	items := make([]services.PersistedChatMessage, 0, len(currentTurn))
	for index, message := range currentTurn {
		content := strings.TrimSpace(flattenChatMessage(message))
		role := strings.TrimSpace(message.Role)
		if role == "" || content == "" {
			continue
		}
		items = append(items, services.PersistedChatMessage{
			Role:       role,
			Content:    content,
			SequenceNo: index + 1,
		})
	}
	return items
}

func resolveUsage(messages []ChatMessage, responsePayload string, promptTokens, completionTokens, totalTokens int) (int, int, int, bool) {
	if totalTokens > 0 || promptTokens > 0 || completionTokens > 0 {
		if totalTokens == 0 {
			totalTokens = promptTokens + completionTokens
		}
		return promptTokens, completionTokens, totalTokens, false
	}

	promptEstimate := estimateTokens(flattenMessages(messages))
	completionEstimate := estimateTokens(responsePayload)
	return promptEstimate, completionEstimate, promptEstimate + completionEstimate, true
}

func estimateTokens(text string) int {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	runes := len([]rune(trimmed))
	tokens := runes / 4
	if runes%4 != 0 {
		tokens++
	}
	if tokens == 0 {
		return 1
	}
	return tokens
}

func flattenMessages(messages []ChatMessage) string {
	var parts []string
	for _, message := range messages {
		content := flattenChatMessage(message)
		if content == "" {
			continue
		}
		if message.Role != "" {
			parts = append(parts, message.Role+": "+content)
			continue
		}
		parts = append(parts, content)
	}
	return strings.Join(parts, "\n")
}

func extractToolReferenceIDs(messages []ChatMessage) []string {
	currentTurn := currentTurnMessages(messages)
	if len(currentTurn) > 0 && strings.EqualFold(strings.TrimSpace(currentTurn[0].Role), "user") {
		currentTurn = currentTurn[1:]
	}

	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, message := range currentTurn {
		if normalized := normalizeToolReferenceID(message.ToolCallID); normalized != "" {
			if _, exists := seen[normalized]; !exists {
				seen[normalized] = struct{}{}
				ids = append(ids, normalized)
			}
		}
		for _, toolCall := range message.ToolCalls {
			if normalized := normalizeToolReferenceID(toolCall.ID); normalized != "" {
				if _, exists := seen[normalized]; !exists {
					seen[normalized] = struct{}{}
					ids = append(ids, normalized)
				}
			}
		}
	}
	return ids
}

func normalizeToolReferenceID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(trimmed))
	for _, char := range trimmed {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			builder.WriteRune(unicode.ToLower(char))
		}
	}
	return builder.String()
}

func extractToolReferenceIDsFromPayload(payload string) map[string]struct{} {
	ids := make(map[string]struct{})
	addID := func(value string) {
		if normalized := normalizeToolReferenceID(value); normalized != "" {
			ids[normalized] = struct{}{}
		}
	}

	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return ids
	}

	if strings.HasPrefix(trimmed, "data:") {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			chunkPayload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if chunkPayload == "" || chunkPayload == "[DONE]" {
				continue
			}

			var chunk openAIStreamChunk
			if err := json.Unmarshal([]byte(chunkPayload), &chunk); err != nil {
				continue
			}

			for _, choice := range chunk.Choices {
				for _, toolCall := range choice.Delta.ToolCalls {
					addID(toolCall.ID)
				}
			}
		}
		return ids
	}

	var response ChatCompletionResponse
	if err := json.Unmarshal([]byte(trimmed), &response); err == nil {
		for _, choice := range response.Choices {
			for _, toolCall := range choice.Message.ToolCalls {
				addID(toolCall.ID)
			}
		}
	}

	return ids
}

func normalizedToolReferenceSet(candidates []string) map[string]struct{} {
	items := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if normalized := normalizeToolReferenceID(candidate); normalized != "" {
			items[normalized] = struct{}{}
		}
	}
	return items
}

func payloadContainsAny(payload string, candidates []string) bool {
	if payload == "" || len(candidates) == 0 {
		return false
	}

	normalizedCandidates := normalizedToolReferenceSet(candidates)
	if len(normalizedCandidates) == 0 {
		return false
	}

	for payloadID := range extractToolReferenceIDsFromPayload(payload) {
		if _, exists := normalizedCandidates[payloadID]; exists {
			return true
		}
	}

	normalizedPayload := normalizeToolReferenceID(payload)
	for candidate := range normalizedCandidates {
		if candidate != "" && strings.Contains(normalizedPayload, candidate) {
			return true
		}
	}
	return false
}

func currentTurnMessages(messages []ChatMessage) []ChatMessage {
	startIndex := 0
	for index := len(messages) - 1; index >= 0; index-- {
		if strings.EqualFold(strings.TrimSpace(messages[index].Role), "user") {
			startIndex = index
			break
		}
	}
	return messages[startIndex:]
}

func normalizeOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func invocationsContainAnyToolReference(items []models.ModelInvocation, toolReferenceIDs []string) bool {
	for _, invocation := range items {
		if invocation.ResponsePayload == nil {
			continue
		}
		if payloadContainsAny(*invocation.ResponsePayload, toolReferenceIDs) {
			return true
		}
	}
	return false
}

func logPersistenceError(action, traceID string, err error) {
	if err == nil {
		return
	}
	if strings.TrimSpace(traceID) == "" {
		log.Printf("ai gateway: %s: %v", action, err)
		return
	}
	log.Printf("ai gateway: %s for trace %s: %v", action, traceID, err)
}

func flattenMessageContent(content interface{}) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []interface{}:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if text := flattenStructuredContentPart(item); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		return mustJSONString(value)
	case map[string]interface{}:
		if text := flattenStructuredContentPart(value); text != "" {
			return text
		}
		return mustJSONString(value)
	default:
		return mustJSONString(value)
	}
}

func flattenStructuredContentPart(content interface{}) string {
	part, ok := content.(map[string]interface{})
	if !ok {
		return ""
	}

	if text, ok := part["text"].(string); ok {
		return strings.TrimSpace(text)
	}
	if text, ok := part["input_text"].(string); ok {
		return strings.TrimSpace(text)
	}
	if text, ok := part["output_text"].(string); ok {
		return strings.TrimSpace(text)
	}

	return ""
}

func flattenChatMessage(message ChatMessage) string {
	parts := []string{}

	content := strings.TrimSpace(flattenMessageContent(message.Content))
	if content != "" {
		parts = append(parts, content)
	}

	toolCalls := strings.TrimSpace(flattenToolCalls(message.ToolCalls))
	if toolCalls != "" {
		parts = append(parts, toolCalls)
	}

	return strings.Join(parts, "\n")
}

func flattenToolCalls(toolCalls []ToolCall) string {
	if len(toolCalls) == 0 {
		return ""
	}

	parts := make([]string, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		if toolCall.Function == nil {
			parts = append(parts, mustJSONString(toolCall))
			continue
		}

		name := strings.TrimSpace(toolCall.Function.Name)
		args := strings.TrimSpace(toolCall.Function.Arguments)
		switch {
		case name != "" && args != "":
			parts = append(parts, fmt.Sprintf("tool_call %s(%s)", name, args))
		case name != "":
			parts = append(parts, "tool_call "+name)
		default:
			parts = append(parts, mustJSONString(toolCall))
		}
	}

	return strings.Join(parts, "\n")
}

func extractAssistantContent(response ChatCompletionResponse) string {
	var parts []string
	for _, choice := range response.Choices {
		content := strings.TrimSpace(flattenMessageContent(choice.Message.Content))
		if content != "" {
			parts = append(parts, content)
			continue
		}

		toolCalls := strings.TrimSpace(flattenToolCalls(choice.Message.ToolCalls))
		if toolCalls != "" {
			parts = append(parts, toolCalls)
		}
	}
	return strings.Join(parts, "\n")
}

// Passthrough proxies an arbitrary OpenAI-compatible POST (e.g. /responses)
// directly to the upstream provider configured for the requested model.
// Risk detection, audit, and cost accounting are intentionally skipped.
//
// Caller MUST close the returned http.Response.Body. For streaming requests,
// caller is responsible for incrementally reading + flushing to the client.
func (s *service) Passthrough(ctx context.Context, userID int, req PassthroughRequest) (*http.Response, *models.LLMModel, error) {
	model, err := s.resolveRequestedModel(strings.TrimSpace(req.Model))
	if err != nil {
		return nil, nil, err
	}
	if model == nil {
		return nil, nil, errors.New("model is not active or does not exist")
	}
	if req.InstanceID != nil && s.instanceRepo != nil {
		instance, err := s.instanceRepo.GetByID(*req.InstanceID)
		if err != nil {
			return nil, model, fmt.Errorf("failed to get instance: %w", err)
		}
		if instance == nil {
			return nil, model, errors.New("instance not found")
		}
		if instance.UserID != userID {
			return nil, model, errors.New("access denied")
		}
	}

	resolvedAPIKey, err := s.secretRefService.ResolveString(ctx, model.APIKey, model.APIKeySecretRef)
	if err != nil {
		return nil, model, err
	}

	// Rewrite the request body so the upstream provider receives the real
	// provider_model_name instead of ClawManager's display name (or "auto").
	// Also strip ClawManager-internal fields. This mirrors what
	// buildOpenAICompatibleRequestBody does for /chat/completions.
	rewrittenBody, err := rewritePassthroughBody(req.RawBody, model)
	if err != nil {
		return nil, model, err
	}
	// Some downstream LLM gateways (Bifrost + Qwen, vLLM, etc.) reject
	// OpenAI Responses messages that use the `developer` role or contain
	// multiple system messages. Normalize the input[] array so they receive
	// at most one leading system message followed by user/assistant/tool
	// messages in the original order.
	if isResponsesEndpoint(req.SubPath) {
		normalized, normErr := normalizeResponsesPayload(rewrittenBody)
		if normErr != nil {
			return nil, model, fmt.Errorf("failed to normalize responses payload: %w", normErr)
		}
		if passthroughDebugEnabled() {
			log.Printf("ai gateway passthrough: normalized responses payload (sub_path=%s, before=%d bytes, after=%d bytes)",
				req.SubPath, len(rewrittenBody), len(normalized))
		}
		rewrittenBody = normalized
	} else if passthroughDebugEnabled() {
		log.Printf("ai gateway passthrough: skipping responses normalization (sub_path=%q does not look like /responses)", req.SubPath)
	}

	subPath := strings.TrimSpace(req.SubPath)
	if subPath == "" {
		return nil, model, errors.New("passthrough sub path is required")
	}
	if !strings.HasPrefix(subPath, "/") {
		subPath = "/" + subPath
	}
	endpoint := strings.TrimRight(strings.TrimSpace(model.BaseURL), "/") + subPath

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rewrittenBody))
	if err != nil {
		return nil, model, fmt.Errorf("failed to build provider request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	// Always emit X-Trace-ID / X-Request-ID for upstream correlation,
	// generating one when the caller did not supply it (same behavior as
	// /chat/completions). X-User-ID surfaces the ClawManager user that
	// owns the request so upstream gateways can audit per-user usage.
	traceID := strings.TrimSpace(req.TraceID)
	if traceID == "" {
		traceID = normalizeOrCreateID(nil, "trc")
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		requestID = normalizeOrCreateID(nil, "req")
	}
	req.TraceID = traceID
	req.RequestID = requestID
	httpReq.Header.Set("X-Trace-ID", traceID)
	httpReq.Header.Set("X-Request-ID", requestID)
	httpReq.Header.Set("X-User-ID", strconv.Itoa(userID))
	if req.InstanceID != nil {
		httpReq.Header.Set("X-Instance-ID", strconv.Itoa(*req.InstanceID))
	}
	if resolvedAPIKey != nil && strings.TrimSpace(*resolvedAPIKey) != "" {
		httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*resolvedAPIKey))
	}

	// Apply per-model custom headers (preserving csotai customization), with a
	// minimal variable set since we do not parse the request body.
	variables := map[string]string{}
	addHeaderVariable(variables, "user.id", strconv.Itoa(userID))
	addHeaderVariable(variables, "user_id", strconv.Itoa(userID))
	if req.TraceID != "" {
		addHeaderVariable(variables, "request.trace_id", req.TraceID)
		addHeaderVariable(variables, "trace_id", req.TraceID)
	}
	if req.RequestID != "" {
		addHeaderVariable(variables, "request.request_id", req.RequestID)
		addHeaderVariable(variables, "request_id", req.RequestID)
	}
	if req.InstanceID != nil {
		addHeaderVariable(variables, "instance.id", strconv.Itoa(*req.InstanceID))
		addHeaderVariable(variables, "instance_id", strconv.Itoa(*req.InstanceID))
	}
	addHeaderVariable(variables, "model.id", strconv.Itoa(model.ID))
	addHeaderVariable(variables, "model.display_name", model.DisplayName)
	addHeaderVariable(variables, "model.provider_model_name", model.ProviderModelName)
	if err := applyCustomProviderHeaders(httpReq, model, variables); err != nil {
		return nil, model, err
	}

	if passthroughDebugEnabled() {
		log.Printf("ai gateway passthrough: POST %s model=%s body=%s",
			endpoint, model.DisplayName, summarizeForLog(rewrittenBody, 4096))
	}

	client := s.passthroughClient
	if client == nil {
		client = s.httpClient
	}
	response, err := client.Do(httpReq)
	if err != nil {
		return nil, model, fmt.Errorf("provider call failed: %w", err)
	}

	// Always surface upstream 4xx/5xx with both request and response bodies so
	// errors like Bifrost/Qwen "system message must be at the beginning" are
	// immediately diagnosable from backend logs, even without
	// GATEWAY_PASSTHROUGH_DEBUG. Bodies are truncated to 1 KB by default to
	// keep production logs bounded; raise the limit by enabling debug mode.
	if response.StatusCode >= 400 {
		bodyBytes, peekErr := io.ReadAll(response.Body)
		response.Body.Close()
		limit := 1024
		if passthroughDebugEnabled() {
			limit = 4096
		}
		if peekErr == nil {
			log.Printf("ai gateway passthrough: upstream %d for %s (model=%s) request=%s response=%s",
				response.StatusCode, endpoint, model.DisplayName,
				summarizeForLog(rewrittenBody, limit),
				summarizeForLog(bodyBytes, limit))
		}
		response.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	return response, model, nil
}

func passthroughDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GATEWAY_PASSTHROUGH_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func summarizeForLog(data []byte, max int) string {
	if len(data) <= max {
		return string(data)
	}
	return string(data[:max]) + fmt.Sprintf("...(truncated, %d bytes total)", len(data))
}

// rewritePassthroughBody decodes a passthrough body, swaps the `model` field
// with the resolved provider_model_name, and strips ClawManager-internal
// session/instance/trace/request fields. Bodies that fail to parse as JSON
// are passed through unchanged so non-JSON formats (form-encoded, binary)
// still work.
func rewritePassthroughBody(rawBody []byte, model *models.LLMModel) ([]byte, error) {
	if model == nil {
		return rawBody, nil
	}
	if len(bytes.TrimSpace(rawBody)) == 0 {
		return rawBody, nil
	}

	payload := map[string]json.RawMessage{}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		// Not a JSON object — let the upstream provider handle it as-is.
		return rawBody, nil
	}
	if payload == nil {
		payload = map[string]json.RawMessage{}
	}

	delete(payload, "session_id")
	delete(payload, "instance_id")
	delete(payload, "trace_id")
	delete(payload, "request_id")

	modelPayload, err := json.Marshal(model.ProviderModelName)
	if err != nil {
		return nil, fmt.Errorf("failed to encode provider model name: %w", err)
	}
	payload["model"] = json.RawMessage(modelPayload)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to encode rewritten passthrough body: %w", err)
	}
	return body, nil
}

// isResponsesEndpoint reports whether the passthrough sub path targets the
// OpenAI Responses API and therefore needs message-role normalization.
// Accepts "/responses", "/responses/...", "/v1/responses", and any version
// prefix variant a caller might supply (e.g. when wiring the gateway
// behind a generic "/v1/*" proxy).
func isResponsesEndpoint(subPath string) bool {
	p := strings.TrimSpace(strings.ToLower(subPath))
	if p == "" {
		return false
	}
	// Strip any leading slash + optional version prefix like "v1", "v2".
	trimmed := strings.TrimPrefix(p, "/")
	if idx := strings.Index(trimmed, "/"); idx > 0 {
		head := trimmed[:idx]
		if len(head) >= 2 && head[0] == 'v' && head[1] >= '0' && head[1] <= '9' {
			trimmed = trimmed[idx+1:]
		}
	}
	return trimmed == "responses" || strings.HasPrefix(trimmed, "responses/")
}

// normalizeResponsesPayload normalizes the `input` array of an OpenAI
// Responses request body so downstream gateways that translate Responses ->
// chat completions (Bifrost + Qwen, vLLM, ...) do not choke on:
//
//   - role "developer" (mapped to "system")
//   - multiple system/developer messages (merged into one leading system
//     message, separated by "\n\n", preserving original order)
//   - unknown role values (mapped to "user")
//   - empty / whitespace-only message content (dropped)
//   - typed content blocks of unknown type (dropped per-block while
//     keeping the surrounding message)
//
// Non-message input items (function_call, reasoning, tool_call_output,
// computer_use, ...) are preserved in their original relative order after
// the merged system message. If `input` is missing, a plain string, or not
// a JSON array, the body is returned unchanged.
func normalizeResponsesPayload(rawBody []byte) ([]byte, error) {
	if len(bytes.TrimSpace(rawBody)) == 0 {
		return rawBody, nil
	}
	payload := map[string]json.RawMessage{}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return rawBody, nil
	}

	// Pull the top-level `instructions` field (OpenAI Responses API treats it
	// as the highest-priority system message) so it can be folded into the
	// leading merged system item below. Removing it after the merge prevents
	// downstream gateways from re-inserting it at the wrong position in the
	// translated chat completions request.
	leadingSystemTexts := make([]string, 0, 2)
	if instructionsRaw, ok := payload["instructions"]; ok && len(bytes.TrimSpace(instructionsRaw)) > 0 {
		if text := extractResponsesMessageText(instructionsRaw); strings.TrimSpace(text) != "" {
			leadingSystemTexts = append(leadingSystemTexts, text)
		}
	}

	inputRaw, exists := payload["input"]
	if !exists || len(bytes.TrimSpace(inputRaw)) == 0 {
		if len(leadingSystemTexts) > 0 {
			// instructions-only request: inline it as input so the upstream
			// receives a well-ordered Responses payload.
			systemItem := map[string]any{
				"type":    "message",
				"role":    "system",
				"content": strings.Join(leadingSystemTexts, "\n\n"),
			}
			systemRaw, err := json.Marshal(systemItem)
			if err != nil {
				return nil, fmt.Errorf("failed to encode instructions system message: %w", err)
			}
			arr, err := json.Marshal([]json.RawMessage{systemRaw})
			if err != nil {
				return nil, err
			}
			payload["input"] = arr
			delete(payload, "instructions")
			return json.Marshal(payload)
		}
		return rawBody, nil
	}

	var inputArray []json.RawMessage
	if err := json.Unmarshal(inputRaw, &inputArray); err != nil {
		// input is a plain string or some other JSON value; leave it alone.
		return rawBody, nil
	}

	systemTexts := append([]string{}, leadingSystemTexts...)
	outItems := make([]json.RawMessage, 0, len(inputArray))

	for _, itemRaw := range inputArray {
		probe := map[string]json.RawMessage{}
		if err := json.Unmarshal(itemRaw, &probe); err != nil {
			// Not a JSON object — pass through untouched.
			outItems = append(outItems, itemRaw)
			continue
		}

		itemType := ""
		if rawType, ok := probe["type"]; ok {
			_ = json.Unmarshal(rawType, &itemType)
		}
		roleRaw, hasRole := probe["role"]
		contentRaw, hasContent := probe["content"]

		// Treat anything with role+content (or explicit type=="message") as a
		// chat message. Other typed items (function_call, reasoning, ...) are
		// preserved verbatim so the upstream still sees them.
		isMessage := strings.EqualFold(itemType, "message") || (itemType == "" && hasRole && hasContent)
		if !isMessage {
			outItems = append(outItems, itemRaw)
			continue
		}

		role := ""
		if hasRole {
			_ = json.Unmarshal(roleRaw, &role)
		}
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "developer" {
			role = "system"
		}
		switch role {
		case "system", "user", "assistant", "tool":
			// known
		default:
			role = "user"
		}

		text := extractResponsesMessageText(contentRaw)
		if strings.TrimSpace(text) == "" {
			// Empty content message — drop it entirely.
			continue
		}

		if role == "system" {
			systemTexts = append(systemTexts, text)
			continue
		}

		// Rebuild this message, preserving any extra fields the caller set
		// (name, tool_call_id, function_call_id, ...). Only type/role/content
		// are overwritten with the normalized values.
		out := map[string]json.RawMessage{}
		for k, v := range probe {
			out[k] = v
		}
		typeRaw, _ := json.Marshal("message")
		roleJSON, _ := json.Marshal(role)
		contentJSON, _ := json.Marshal(text)
		out["type"] = typeRaw
		out["role"] = roleJSON
		out["content"] = contentJSON

		newRaw, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("failed to re-encode normalized message: %w", err)
		}
		outItems = append(outItems, newRaw)
	}

	finalItems := outItems
	if len(systemTexts) > 0 {
		merged := strings.Join(systemTexts, "\n\n")
		systemItem := map[string]any{
			"type":    "message",
			"role":    "system",
			"content": merged,
		}
		systemRaw, err := json.Marshal(systemItem)
		if err != nil {
			return nil, fmt.Errorf("failed to encode merged system message: %w", err)
		}
		finalItems = append([]json.RawMessage{systemRaw}, outItems...)
	}

	newInputRaw, err := json.Marshal(finalItems)
	if err != nil {
		return nil, fmt.Errorf("failed to encode normalized input array: %w", err)
	}
	payload["input"] = newInputRaw
	// Strip the original instructions field so downstream gateways don't
	// translate it a second time into a misordered system message.
	delete(payload, "instructions")

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to encode normalized passthrough body: %w", err)
	}
	return body, nil
}

// extractResponsesMessageText flattens a Responses-API `content` field into a
// single string. Accepts either a raw string, an array of strings, or an array
// of typed content blocks (input_text, output_text, text). Unknown block
// types are skipped silently.
func extractResponsesMessageText(contentRaw json.RawMessage) string {
	if len(bytes.TrimSpace(contentRaw)) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(contentRaw, &str); err == nil {
		return str
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, blockRaw := range blocks {
		var s string
		if err := json.Unmarshal(blockRaw, &s); err == nil {
			if s != "" {
				parts = append(parts, s)
			}
			continue
		}
		block := map[string]json.RawMessage{}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		blockType := ""
		if rawT, ok := block["type"]; ok {
			_ = json.Unmarshal(rawT, &blockType)
		}
		switch strings.ToLower(strings.TrimSpace(blockType)) {
		case "input_text", "output_text", "text":
			text := ""
			if rawText, ok := block["text"]; ok {
				_ = json.Unmarshal(rawText, &text)
			}
			if text != "" {
				parts = append(parts, text)
			}
		default:
			// Unknown block type — drop silently so downstream doesn't reject.
		}
	}
	return strings.Join(parts, "\n")
}

