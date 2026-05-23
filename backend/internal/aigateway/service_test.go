package aigateway

import (
	"encoding/json"
	"strings"
	"testing"

	"clawreef/internal/models"
)

type stubModelInvocationService struct {
	items []models.ModelInvocation
}

func (s *stubModelInvocationService) RecordInvocation(invocation *models.ModelInvocation) error {
	return nil
}

func (s *stubModelInvocationService) GetInvocationByID(id int) (*models.ModelInvocation, error) {
	return nil, nil
}

func (s *stubModelInvocationService) ListInvocationsByTraceID(traceID string) ([]models.ModelInvocation, error) {
	return s.items, nil
}

func (s *stubModelInvocationService) ListInvocationsBySessionID(sessionID string, limit int) ([]models.ModelInvocation, error) {
	return s.items, nil
}

func (s *stubModelInvocationService) ListInvocationsByUserID(userID, limit int) ([]models.ModelInvocation, error) {
	return s.items, nil
}

type stubChatSessionService struct {
	session *models.ChatSession
}

func (s *stubChatSessionService) GetSession(sessionID string) (*models.ChatSession, error) {
	return s.session, nil
}

func (s *stubChatSessionService) EnsureSession(sessionID string, userID, instanceID *int, traceID *string, title *string) (*models.ChatSession, error) {
	return s.session, nil
}

func TestBuildProviderRequestPreservesToolConfiguration(t *testing.T) {
	req := ChatCompletionRequest{
		Model:   "gateway-model",
		RawBody: []byte(`{"model":"gateway-model","messages":[{"role":"user","content":[{"type":"text","text":"weather in shanghai"}]}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}},"stream":true,"stream_options":{"include_usage":false,"custom":"value"},"custom_field":{"nested":[1,2,3]},"session_id":"sess_123","request_id":"req_123","trace_id":"trc_123","instance_id":99}`),
	}

	model := &models.LLMModel{
		ProviderModelName: "provider-model",
	}

	providerRequestBody, err := buildProviderRequestBody(req, model)
	if err != nil {
		t.Fatalf("buildProviderRequestBody returned error: %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(providerRequestBody, &payload); err != nil {
		t.Fatalf("failed to decode provider request body: %v", err)
	}

	var modelName string
	if err := json.Unmarshal(payload["model"], &modelName); err != nil {
		t.Fatalf("failed to decode model name: %v", err)
	}
	if modelName != "provider-model" {
		t.Fatalf("expected provider model name to be replaced, got %q", modelName)
	}
	if _, ok := payload["messages"]; !ok {
		t.Fatalf("expected messages to be forwarded")
	}
	if _, ok := payload["tools"]; !ok {
		t.Fatalf("expected tools to be forwarded")
	}
	if _, ok := payload["tool_choice"]; !ok {
		t.Fatalf("expected tool_choice to be forwarded")
	}
	if _, ok := payload["custom_field"]; !ok {
		t.Fatalf("expected unknown provider fields to survive")
	}
	if _, ok := payload["session_id"]; ok {
		t.Fatalf("expected internal session_id to be stripped")
	}
	if _, ok := payload["request_id"]; ok {
		t.Fatalf("expected internal request_id to be stripped")
	}
	if _, ok := payload["trace_id"]; ok {
		t.Fatalf("expected internal trace_id to be stripped")
	}
	if _, ok := payload["instance_id"]; ok {
		t.Fatalf("expected internal instance_id to be stripped")
	}
	if string(payload["stream_options"]) != `{"include_usage":false,"custom":"value"}` {
		t.Fatalf("expected stream_options to pass through unchanged, got %s", string(payload["stream_options"]))
	}
}

func TestBuildProviderRequestUsesAnthropicProtocolForLocalModel(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "gateway-model",
		Messages: []ChatMessage{
			{
				Role:    "user",
				Content: "hello from local anthropic",
			},
		},
	}

	model := &models.LLMModel{
		ProviderType:      models.ProviderTypeLocal,
		ProtocolType:      models.ProtocolTypeAnthropic,
		ProviderModelName: "claude-local",
	}

	providerRequestBody, err := buildProviderRequestBody(req, model)
	if err != nil {
		t.Fatalf("buildProviderRequestBody returned error: %v", err)
	}

	var payload anthropicRequestPayload
	if err := json.Unmarshal(providerRequestBody, &payload); err != nil {
		t.Fatalf("expected anthropic payload, got decode error: %v", err)
	}

	if payload.Model != "claude-local" {
		t.Fatalf("expected provider model name to be replaced, got %q", payload.Model)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" {
		t.Fatalf("expected anthropic user message, got %#v", payload.Messages)
	}
}

func TestRewriteStreamLineKeepsToolCalls(t *testing.T) {
	line := "data: {\"id\":\"chatcmpl-1\",\"model\":\"provider-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"Shanghai\\\"}\"}}]},\"finish_reason\":null}]}\n"

	var assistantText strings.Builder
	var promptTokens int
	var completionTokens int
	var totalTokens int

	done := inspectStreamLine(line, &assistantText, &promptTokens, &completionTokens, &totalTokens)
	if done {
		t.Fatalf("expected tool chunk not to finish the stream")
	}
	if assistantText.String() != "" {
		t.Fatalf("expected tool chunks not to be appended as assistant text, got %q", assistantText.String())
	}

	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
	var chunk openAIStreamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		t.Fatalf("failed to decode chunk: %v", err)
	}

	if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected tool_calls to be preserved, got %#v", chunk.Choices)
	}
	if chunk.Choices[0].Delta.ToolCalls[0].Function == nil || chunk.Choices[0].Delta.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("expected function metadata to survive, got %#v", chunk.Choices[0].Delta.ToolCalls[0])
	}
}

func TestExtractAssistantContentFallsBackToToolCalls(t *testing.T) {
	response := ChatCompletionResponse{
		Choices: []struct {
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
					Role:    "assistant",
					Content: nil,
					ToolCalls: []ToolCall{
						{
							ID:   "call_1",
							Type: "function",
							Function: &ToolCallFunction{
								Name:      "get_weather",
								Arguments: `{"city":"Shanghai"}`,
							},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
	}

	content := extractAssistantContent(response)
	if !strings.Contains(content, "get_weather") {
		t.Fatalf("expected tool call content to be included, got %q", content)
	}
	if strings.Contains(content, "null") {
		t.Fatalf("expected nil content not to become \"null\", got %q", content)
	}
}

func TestResolveTraceIDReusesRecentTraceForToolLoop(t *testing.T) {
	toolCallID := "call_weather_123"
	svc := &service{
		chatSessionService: &stubChatSessionService{
			session: &models.ChatSession{
				SessionID:   "agent:openclaw:main",
				LastTraceID: stringPtr("trc_existing"),
			},
		},
		invocationService: &stubModelInvocationService{
			items: []models.ModelInvocation{
				{
					TraceID:         "trc_existing",
					InstanceID:      intPtr(9),
					ResponsePayload: stringPtr(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_weather_123","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}}]}}]}`),
				},
			},
		},
	}

	traceID := svc.resolveTraceID(7, ChatCompletionRequest{
		InstanceID: intPtr(9),
		Messages: []ChatMessage{
			{
				Role:       "tool",
				ToolCallID: toolCallID,
				Content:    `{"temp_c":25}`,
			},
		},
	}, "agent:openclaw:main")

	if traceID != "trc_existing" {
		t.Fatalf("expected tool continuation to reuse trace trc_existing, got %q", traceID)
	}
}

func TestResolveTraceIDReusesSessionLastTraceBeforeInvocationIsPersisted(t *testing.T) {
	toolCallID := "call_weather_123"
	svc := &service{
		chatSessionService: &stubChatSessionService{
			session: &models.ChatSession{
				SessionID:   "agent:openclaw:main",
				LastTraceID: stringPtr("trc_existing"),
			},
		},
		invocationService: &stubModelInvocationService{},
	}

	traceID := svc.resolveTraceID(7, ChatCompletionRequest{
		InstanceID: intPtr(9),
		Messages: []ChatMessage{
			{
				Role:    "user",
				Content: "What is the weather in Beijing now?",
			},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID: toolCallID,
						Function: &ToolCallFunction{
							Name:      "get_weather",
							Arguments: `{"city":"Beijing"}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: toolCallID,
				Content:    `{"temp_c":25}`,
			},
		},
	}, "agent:openclaw:main")

	if traceID != "trc_existing" {
		t.Fatalf("expected session last trace to be reused before invocation persistence, got %q", traceID)
	}
}

func TestResolveTraceIDReusesTraceWhenOpenClawStripsToolCallUnderscore(t *testing.T) {
	svc := &service{
		invocationService: &stubModelInvocationService{
			items: []models.ModelInvocation{
				{
					TraceID:         "trc_existing",
					UserID:          intPtr(7),
					InstanceID:      intPtr(19),
					ResponsePayload: stringPtr("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_d1cad4719db94d1594f93456\",\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"exec\",\"arguments\":\"\"}}]}}]}\n\ndata: [DONE]\n"),
				},
			},
		},
	}

	traceID := svc.resolveTraceID(7, ChatCompletionRequest{
		InstanceID: intPtr(19),
		Messages: []ChatMessage{
			{
				Role:    "user",
				Content: "What is the weather in Changchun?",
			},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID: "calld1cad4719db94d1594f93456",
						Function: &ToolCallFunction{
							Name:      "exec",
							Arguments: `{"command":"curl -s \"wttr.in/Changchun\""}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "calld1cad4719db94d1594f93456",
				Content:    "changchun: +9C",
			},
		},
	}, "")

	if traceID != "trc_existing" {
		t.Fatalf("expected trace reuse when tool id separators differ, got %q", traceID)
	}
}

func TestResolveTraceIDDoesNotReuseHistoricalToolLoopAcrossNewUserTurn(t *testing.T) {
	toolCallID := "call_weather_123"
	svc := &service{
		chatSessionService: &stubChatSessionService{
			session: &models.ChatSession{
				SessionID:   "agent:openclaw:main",
				LastTraceID: stringPtr("trc_existing"),
			},
		},
		invocationService: &stubModelInvocationService{
			items: []models.ModelInvocation{
				{
					TraceID:         "trc_existing",
					InstanceID:      intPtr(9),
					ResponsePayload: stringPtr(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_weather_123","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}}]}}]}`),
				},
			},
		},
	}

	traceID := svc.resolveTraceID(7, ChatCompletionRequest{
		InstanceID: intPtr(9),
		Messages: []ChatMessage{
			{
				Role:    "user",
				Content: "What is the weather in Beijing now?",
			},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID: toolCallID,
						Function: &ToolCallFunction{
							Name:      "get_weather",
							Arguments: `{"city":"Beijing"}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: toolCallID,
				Content:    `{"temp_c":25}`,
			},
			{
				Role:    "assistant",
				Content: "Beijing is cloudy and 25C.",
			},
			{
				Role:    "user",
				Content: "How about Shanghai?",
			},
		},
	}, "agent:openclaw:main")

	if traceID == "trc_existing" {
		t.Fatalf("expected a new trace for the next user turn, got %q", traceID)
	}
	if !strings.HasPrefix(traceID, "trc_") {
		t.Fatalf("expected generated trace id to keep trc_ prefix, got %q", traceID)
	}
}

func TestBuildPersistedMessagesKeepsOnlyCurrentTurnMessages(t *testing.T) {
	items := buildPersistedMessages([]ChatMessage{
		{
			Role:    "system",
			Content: "system prompt",
		},
		{
			Role:    "user",
			Content: "Previous turn question",
		},
		{
			Role:    "assistant",
			Content: "Previous turn answer",
		},
		{
			Role:    "user",
			Content: "Current turn question",
		},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{
					ID: "call_current",
					Function: &ToolCallFunction{
						Name:      "get_weather",
						Arguments: `{"city":"Shanghai"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_current",
			Content:    `{"temp_c":16}`,
		},
	})

	if len(items) != 3 {
		t.Fatalf("expected only current turn messages to be persisted, got %d", len(items))
	}
	if items[0].Role != "user" || items[0].Content != "Current turn question" {
		t.Fatalf("expected current turn user message first, got %#v", items[0])
	}
	if items[1].Role != "assistant" || !strings.Contains(items[1].Content, "get_weather") {
		t.Fatalf("expected current turn assistant tool call second, got %#v", items[1])
	}
	if items[2].Role != "tool" || items[2].Content != `{"temp_c":16}` {
		t.Fatalf("expected current turn tool output third, got %#v", items[2])
	}
	if items[0].SequenceNo != 1 || items[1].SequenceNo != 2 || items[2].SequenceNo != 3 {
		t.Fatalf("expected current turn sequence numbers to restart at 1, got %#v", items)
	}
}

func TestResolveTraceIDKeepsExplicitTraceID(t *testing.T) {
	explicit := "trc_explicit"
	svc := &service{}

	traceID := svc.resolveTraceID(7, ChatCompletionRequest{
		TraceID: &explicit,
		Messages: []ChatMessage{
			{
				Role:    "user",
				Content: "hello",
			},
		},
	}, "")

	if traceID != explicit {
		t.Fatalf("expected explicit trace id %q, got %q", explicit, traceID)
	}
}

func TestResolveSessionIDPrefersExplicitSessionThenOpenAIUser(t *testing.T) {
	explicitSession := "agent:main:direct:alice"
	if got := resolveSessionID(ChatCompletionRequest{SessionID: &explicitSession}); got != explicitSession {
		t.Fatalf("expected explicit session id %q, got %q", explicitSession, got)
	}

	openAIUser := "agent:main:direct:bob"
	if got := resolveSessionID(ChatCompletionRequest{User: &openAIUser}); got != openAIUser {
		t.Fatalf("expected OpenAI user to become session id %q, got %q", openAIUser, got)
	}
}

func TestNormalizeExistingIdentifierHashesLongOpenClawSessionKeys(t *testing.T) {
	longKey := "agent:very-long-openclaw-agent-id:discord:default:direct:12345678901234567890123456789012345678901234567890"
	normalized := normalizeExistingIdentifier(longKey, "sess")
	if normalized == longKey {
		t.Fatalf("expected long session key to be normalized")
	}
	if len(normalized) > maxStoredIdentifierLength {
		t.Fatalf("expected normalized id length <= %d, got %d", maxStoredIdentifierLength, len(normalized))
	}
}

func TestRewritePassthroughBodyReplacesModelWithProviderName(t *testing.T) {
	model := &models.LLMModel{
		DisplayName:       "codex",
		ProviderModelName: "gpt-5-codex",
	}
	raw := []byte(`{"model":"auto","input":[{"role":"user","content":"hi"}],"session_id":"abc","stream":true}`)

	rewritten, err := rewritePassthroughBody(raw, model)
	if err != nil {
		t.Fatalf("rewritePassthroughBody returned error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("failed to decode rewritten body: %v", err)
	}
	if payload["model"] != "gpt-5-codex" {
		t.Fatalf("expected model to be rewritten to gpt-5-codex, got %v", payload["model"])
	}
	if _, exists := payload["session_id"]; exists {
		t.Fatalf("expected session_id to be stripped from passthrough body")
	}
	if payload["stream"] != true {
		t.Fatalf("expected stream flag to be preserved, got %v", payload["stream"])
	}
	if _, exists := payload["input"]; !exists {
		t.Fatalf("expected input field to be preserved")
	}
}

func TestRewritePassthroughBodyKeepsNonJSONBodyAsIs(t *testing.T) {
	model := &models.LLMModel{ProviderModelName: "gpt-5"}
	raw := []byte("not-json")
	rewritten, err := rewritePassthroughBody(raw, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(rewritten) != "not-json" {
		t.Fatalf("expected non-JSON body to pass through unchanged, got %q", string(rewritten))
	}
}

func TestRewritePassthroughBodyHandlesEmptyBody(t *testing.T) {
	model := &models.LLMModel{ProviderModelName: "gpt-5"}
	if rewritten, err := rewritePassthroughBody(nil, model); err != nil || rewritten != nil {
		t.Fatalf("expected nil body to pass through, got %q (err=%v)", string(rewritten), err)
	}
	empty := []byte("   ")
	if rewritten, err := rewritePassthroughBody(empty, model); err != nil || string(rewritten) != "   " {
		t.Fatalf("expected whitespace body to pass through unchanged, got %q (err=%v)", string(rewritten), err)
	}
}

func TestNormalizeResponsesPayloadMergesDeveloperAndSystem(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[
		{"type":"message","role":"developer","content":"You are X"},
		{"type":"message","role":"system","content":"Stay polite"},
		{"type":"message","role":"user","content":"hi"},
		{"type":"message","role":"developer","content":"reply briefly"}
	]}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}

	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode normalized body: %v", err)
	}
	if len(decoded.Input) != 2 {
		t.Fatalf("expected 2 items after normalize, got %d: %s", len(decoded.Input), string(normalized))
	}
	if decoded.Input[0]["role"] != "system" {
		t.Fatalf("expected first item to be system, got %v", decoded.Input[0]["role"])
	}
	wantSystem := "You are X\n\nStay polite\n\nreply briefly"
	if decoded.Input[0]["content"] != wantSystem {
		t.Fatalf("unexpected merged system content: %v", decoded.Input[0]["content"])
	}
	if decoded.Input[1]["role"] != "user" || decoded.Input[1]["content"] != "hi" {
		t.Fatalf("expected user message preserved, got %v", decoded.Input[1])
	}
}

func TestNormalizeResponsesPayloadFlattensTypedContentBlocks(t *testing.T) {
	raw := []byte(`{"input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"hello"},
			{"type":"input_image","image_url":"data:..."},
			{"type":"input_text","text":"world"}
		]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"be brief"}]}
	]}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}

	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode normalized body: %v", err)
	}
	if len(decoded.Input) != 2 {
		t.Fatalf("expected 2 items, got %d", len(decoded.Input))
	}
	if decoded.Input[0]["role"] != "system" || decoded.Input[0]["content"] != "be brief" {
		t.Fatalf("unexpected system item: %v", decoded.Input[0])
	}
	if decoded.Input[1]["role"] != "user" || decoded.Input[1]["content"] != "hello\nworld" {
		t.Fatalf("expected flattened user text, got %v", decoded.Input[1])
	}
}

func TestNormalizeResponsesPayloadPreservesNonMessageItems(t *testing.T) {
	raw := []byte(`{"input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"function_call","call_id":"abc","name":"foo","arguments":"{}"},
		{"type":"message","role":"developer","content":"be brief"},
		{"type":"reasoning","summary":[{"text":"thinking"}]}
	]}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}

	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode normalized body: %v", err)
	}
	if len(decoded.Input) != 4 {
		t.Fatalf("expected 4 items, got %d: %s", len(decoded.Input), string(normalized))
	}
	if decoded.Input[0]["role"] != "system" || decoded.Input[0]["content"] != "be brief" {
		t.Fatalf("expected merged system first, got %v", decoded.Input[0])
	}
	if decoded.Input[1]["type"] != "message" || decoded.Input[1]["role"] != "user" {
		t.Fatalf("expected user message at position 1, got %v", decoded.Input[1])
	}
	if decoded.Input[2]["type"] != "function_call" {
		t.Fatalf("expected function_call preserved at position 2, got %v", decoded.Input[2])
	}
	if decoded.Input[3]["type"] != "reasoning" {
		t.Fatalf("expected reasoning preserved at position 3, got %v", decoded.Input[3])
	}
}

func TestNormalizeResponsesPayloadDropsEmptyContent(t *testing.T) {
	raw := []byte(`{"input":[
		{"type":"message","role":"user","content":""},
		{"type":"message","role":"developer","content":"   "},
		{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:..."}]},
		{"type":"message","role":"assistant","content":"ok"}
	]}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}

	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode normalized body: %v", err)
	}
	if len(decoded.Input) != 1 {
		t.Fatalf("expected only the assistant message to survive, got %d: %s", len(decoded.Input), string(normalized))
	}
	if decoded.Input[0]["role"] != "assistant" || decoded.Input[0]["content"] != "ok" {
		t.Fatalf("expected assistant ok, got %v", decoded.Input[0])
	}
}

func TestNormalizeResponsesPayloadMapsUnknownRoleToUser(t *testing.T) {
	raw := []byte(`{"input":[{"role":"foobar","content":"weird"}]}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}
	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode normalized body: %v", err)
	}
	if len(decoded.Input) != 1 || decoded.Input[0]["role"] != "user" {
		t.Fatalf("expected single user message, got %v", decoded.Input)
	}
}

func TestNormalizeResponsesPayloadIgnoresStringInput(t *testing.T) {
	raw := []byte(`{"model":"x","input":"hello there"}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}
	if string(normalized) != string(raw) {
		t.Fatalf("expected string input to pass through unchanged: %s", string(normalized))
	}
}

func TestNormalizeResponsesPayloadPreservesExtraFields(t *testing.T) {
	raw := []byte(`{"input":[
		{"type":"message","role":"tool","content":"42","tool_call_id":"call_1"}
	]}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}
	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode normalized body: %v", err)
	}
	if decoded.Input[0]["tool_call_id"] != "call_1" {
		t.Fatalf("expected tool_call_id preserved, got %v", decoded.Input[0])
	}
}

func TestIsResponsesEndpoint(t *testing.T) {
	positives := []string{
		"/responses",
		"/responses/",
		"/Responses",
		"/responses/abc/cancel",
		"/v1/responses",
		"/v1/responses/",
		"/v1/responses/abc/cancel",
		"/v2/responses",
	}
	for _, p := range positives {
		if !isResponsesEndpoint(p) {
			t.Fatalf("expected %q to be detected as responses endpoint", p)
		}
	}
	negatives := []string{
		"/chat/completions",
		"/v1/chat/completions",
		"/embeddings",
		"",
		"/responsesfoo",
		"/v1/embeddings",
	}
	for _, p := range negatives {
		if isResponsesEndpoint(p) {
			t.Fatalf("expected %q NOT to be detected as responses endpoint", p)
		}
	}
}

func TestNormalizeResponsesPayloadFoldsInstructionsIntoSystem(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5",
		"instructions":"You are X",
		"input":[
			{"type":"message","role":"developer","content":"Be brief"},
			{"type":"message","role":"user","content":"hi"}
		]
	}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}

	var decoded struct {
		Instructions any              `json:"instructions"`
		Input        []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if decoded.Instructions != nil {
		t.Fatalf("expected instructions to be stripped after folding, got %v", decoded.Instructions)
	}
	if len(decoded.Input) != 2 {
		t.Fatalf("expected 2 input items, got %d: %s", len(decoded.Input), string(normalized))
	}
	want := "You are X\n\nBe brief"
	if decoded.Input[0]["role"] != "system" || decoded.Input[0]["content"] != want {
		t.Fatalf("expected merged leading system %q, got %v", want, decoded.Input[0])
	}
	if decoded.Input[1]["role"] != "user" || decoded.Input[1]["content"] != "hi" {
		t.Fatalf("expected user preserved, got %v", decoded.Input[1])
	}
}

func TestNormalizeResponsesPayloadInstructionsOnly(t *testing.T) {
	raw := []byte(`{"instructions":"behave"}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}
	var decoded struct {
		Instructions any              `json:"instructions"`
		Input        []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if decoded.Instructions != nil {
		t.Fatalf("expected instructions stripped, got %v", decoded.Instructions)
	}
	if len(decoded.Input) != 1 || decoded.Input[0]["role"] != "system" || decoded.Input[0]["content"] != "behave" {
		t.Fatalf("expected single inlined system message, got %v", decoded.Input)
	}
}

func TestNormalizeResponsesPayloadInstructionsPlusUserOnly(t *testing.T) {
	// Real codex CLI scenario: top-level instructions + only user message in input.
	raw := []byte(`{
		"model":"gpt-5",
		"instructions":"You are a coding agent.",
		"input":[{"type":"message","role":"user","content":"refactor foo.go"}]
	}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}
	var decoded struct {
		Instructions any              `json:"instructions"`
		Input        []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if decoded.Instructions != nil {
		t.Fatalf("expected instructions stripped, got %v", decoded.Instructions)
	}
	if len(decoded.Input) != 2 {
		t.Fatalf("expected 2 input items (system + user), got %d: %s", len(decoded.Input), string(normalized))
	}
	if decoded.Input[0]["role"] != "system" {
		t.Fatalf("expected input[0] role=system, got %v", decoded.Input[0]["role"])
	}
	if decoded.Input[0]["content"] != "You are a coding agent." {
		t.Fatalf("expected input[0] content from instructions, got %v", decoded.Input[0]["content"])
	}
	if decoded.Input[1]["role"] != "user" {
		t.Fatalf("expected input[1] role=user, got %v", decoded.Input[1]["role"])
	}
}

func TestNormalizeResponsesPayloadOrderAfterInterleavedSystem(t *testing.T) {
	raw := []byte(`{
		"input":[
			{"type":"message","role":"user","content":"step 1"},
			{"type":"message","role":"assistant","content":"ok"},
			{"type":"message","role":"system","content":"reminder"},
			{"type":"message","role":"user","content":"step 2"}
		]
	}`)
	normalized, err := normalizeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesPayload returned error: %v", err)
	}
	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(decoded.Input) != 4 {
		t.Fatalf("expected 4 items (system + 3 others), got %d: %s", len(decoded.Input), string(normalized))
	}
	if decoded.Input[0]["role"] != "system" || decoded.Input[0]["content"] != "reminder" {
		t.Fatalf("expected system extracted to position 0, got %v", decoded.Input[0])
	}
	roles := []any{decoded.Input[1]["role"], decoded.Input[2]["role"], decoded.Input[3]["role"]}
	want := []any{"user", "assistant", "user"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("expected position %d role=%v, got %v (full: %s)", i+1, want[i], roles[i], string(normalized))
		}
	}
}
