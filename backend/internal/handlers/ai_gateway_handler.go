package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"clawreef/internal/aigateway"
	"clawreef/internal/utils"

	"github.com/gin-gonic/gin"
)

// AIGatewayHandler exposes AI gateway endpoints.
type AIGatewayHandler struct {
	service aigateway.Service
}

// NewAIGatewayHandler creates a new AI gateway handler.
func NewAIGatewayHandler(service aigateway.Service) *AIGatewayHandler {
	return &AIGatewayHandler{service: service}
}

// ListModels returns active models available to the current user.
func (h *AIGatewayHandler) ListModels(c *gin.Context) {
	items, err := h.service.ListAvailableModels()
	if err != nil {
		utils.HandleError(c, err)
		return
	}

	utils.Success(c, http.StatusOK, "Available gateway models retrieved successfully", gin.H{
		"items": items,
	})
}

// ChatCompletions proxies a governed chat completion request.
func (h *AIGatewayHandler) ChatCompletions(c *gin.Context) {
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		utils.Error(c, http.StatusBadRequest, "Failed to read request body")
		return
	}

	var req aigateway.ChatCompletionRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		utils.ValidationError(c, err)
		return
	}
	req.RawBody = rawBody
	if req.SessionID == nil {
		if sessionKey := strings.TrimSpace(c.GetHeader("x-openclaw-session-key")); sessionKey != "" {
			req.SessionID = &sessionKey
		}
	}
	if req.TraceID == nil {
		if runID := strings.TrimSpace(c.GetHeader("x-openclaw-run-id")); runID != "" {
			req.TraceID = &runID
		}
	}

	userID, exists := c.Get("userID")
	if !exists {
		utils.Error(c, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if instanceID, exists := c.Get("instanceID"); exists {
		switch value := instanceID.(type) {
		case int:
			if req.InstanceID == nil {
				req.InstanceID = &value
			} else if *req.InstanceID != value {
				utils.Error(c, http.StatusForbidden, "Gateway token does not match requested instance")
				return
			}
		}
	}

	if req.Stream {
		traceID, err := h.service.StreamChatCompletions(c.Request.Context(), userID.(int), req, c.Writer)
		if traceID != "" {
			c.Header("X-Trace-ID", traceID)
		}
		if err != nil {
			if !c.Writer.Written() {
				utils.HandleError(c, err)
			}
		}
		return
	}

	response, traceID, err := h.service.ChatCompletions(c.Request.Context(), userID.(int), req)
	if err != nil {
		c.Header("X-Trace-ID", traceID)
		utils.HandleError(c, err)
		return
	}

	c.Header("X-Trace-ID", traceID)
	for key, values := range response.Headers {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	c.Status(response.StatusCode)
	_, _ = c.Writer.Write(response.Body)
}

// Responses proxies a POST /v1/responses request straight to the upstream
// provider. ClawManager does not parse OpenAI Responses API bodies — risk,
// audit, and cost accounting are skipped. Use ChatCompletions when those
// guarantees are required.
func (h *AIGatewayHandler) Responses(c *gin.Context) {
	h.passthrough(c, "/responses")
}

func (h *AIGatewayHandler) passthrough(c *gin.Context, subPath string) {
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		utils.Error(c, http.StatusBadRequest, "Failed to read request body")
		return
	}

	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if len(rawBody) > 0 {
		if err := json.Unmarshal(rawBody, &probe); err != nil {
			utils.ValidationError(c, err)
			return
		}
	}
	if strings.TrimSpace(probe.Model) == "" {
		utils.Error(c, http.StatusBadRequest, "model is required")
		return
	}

	userID, exists := c.Get("userID")
	if !exists {
		utils.Error(c, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var instanceIDPtr *int
	if instanceIDValue, exists := c.Get("instanceID"); exists {
		if id, ok := instanceIDValue.(int); ok {
			instanceIDPtr = &id
		}
	}

	traceID := strings.TrimSpace(c.GetHeader("X-Trace-ID"))
	if traceID == "" {
		traceID = strings.TrimSpace(c.GetHeader("x-openclaw-run-id"))
	}
	requestID := strings.TrimSpace(c.GetHeader("X-Request-ID"))

	response, _, err := h.service.Passthrough(c.Request.Context(), userID.(int), aigateway.PassthroughRequest{
		Model:      probe.Model,
		RawBody:    rawBody,
		InstanceID: instanceIDPtr,
		SubPath:    subPath,
		Stream:     probe.Stream,
		TraceID:    traceID,
		RequestID:  requestID,
	})
	if err != nil {
		utils.HandleError(c, err)
		return
	}
	defer response.Body.Close()

	for key, values := range response.Header {
		// Strip hop-by-hop headers so gin can manage the response correctly.
		switch strings.ToLower(key) {
		case "connection", "transfer-encoding", "content-length":
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	if traceID != "" {
		c.Writer.Header().Set("X-Trace-ID", traceID)
	}
	c.Status(response.StatusCode)

	if probe.Stream || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		flusher, _ := c.Writer.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, readErr := response.Body.Read(buf)
			if n > 0 {
				if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if readErr != nil {
				return
			}
		}
	}

	_, _ = io.Copy(c.Writer, response.Body)
}
