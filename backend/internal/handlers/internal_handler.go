package handlers

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"clawreef/internal/models"
	"clawreef/internal/services"
	"clawreef/internal/services/k8s"
	"clawreef/internal/utils"

	"github.com/gin-gonic/gin"
)

// InternalHandler handles internal API requests from other services
// (e.g. Five-Star AI island / openclaw-service).
type InternalHandler struct {
	instanceService services.InstanceService
	userService     services.UserService
	internalToken   string
}

// NewInternalHandler creates a new internal handler
func NewInternalHandler(instanceService services.InstanceService, userService services.UserService) *InternalHandler {
	return &InternalHandler{
		instanceService: instanceService,
		userService:     userService,
		internalToken:   strings.TrimSpace(os.Getenv("INTERNAL_API_TOKEN")),
	}
}

// InternalAuthMiddleware validates the X-Internal-Token header.
// If INTERNAL_API_TOKEN is not configured, requests are allowed (dev mode).
func (h *InternalHandler) InternalAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.internalToken == "" {
			c.Next()
			return
		}
		token := c.GetHeader("X-Internal-Token")
		if token != h.internalToken {
			utils.Error(c, http.StatusUnauthorized, "invalid internal token")
			c.Abort()
			return
		}
		c.Next()
	}
}

// InternalLoginRequest represents an internal login request
type InternalLoginRequest struct {
	InstanceID string `json:"instance_id" binding:"required"`
}

// InternalLogin generates a short-lived access token for an instance.
func (h *InternalHandler) InternalLogin(c *gin.Context) {
	var req InternalLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ValidationError(c, err)
		return
	}

	id, err := strconv.Atoi(req.InstanceID)
	if err != nil {
		utils.Error(c, http.StatusBadRequest, "invalid instance id")
		return
	}

	instance, err := h.instanceService.GetByID(id)
	if err != nil || instance == nil {
		utils.Error(c, http.StatusNotFound, "instance not found")
		return
	}

	token := generateInternalToken(id)

	utils.Success(c, http.StatusOK, "token generated", gin.H{
		"token": token,
	})
}

// ListInstances returns all instances enriched with cross-cluster gateway routing info.
func (h *InternalHandler) ListInstances(c *gin.Context) {
	instances, total, err := h.instanceService.GetAllInstances(0, 10000)
	if err != nil {
		utils.HandleError(c, err)
		return
	}

	gatewayBase := sidecarGatewayBaseURL()
	client := k8s.GetClient()

	result := make([]gin.H, 0, len(instances))
	for i := range instances {
		result = append(result, instanceToInternalResponse(&instances[i], gatewayBase, client))
	}

	utils.Success(c, http.StatusOK, "instances retrieved", gin.H{
		"instances": result,
		"total":     total,
	})
}

// GetInstance returns a single instance with gateway routing info.
func (h *InternalHandler) GetInstance(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		utils.Error(c, http.StatusBadRequest, "invalid instance id")
		return
	}

	instance, err := h.instanceService.GetByID(id)
	if err != nil || instance == nil {
		utils.Error(c, http.StatusNotFound, "instance not found")
		return
	}

	gatewayBase := sidecarGatewayBaseURL()
	client := k8s.GetClient()

	utils.Success(c, http.StatusOK, "instance retrieved",
		instanceToInternalResponse(instance, gatewayBase, client))
}

// RestartInstance restarts an instance.
func (h *InternalHandler) RestartInstance(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		utils.Error(c, http.StatusBadRequest, "invalid instance id")
		return
	}

	if err := h.instanceService.Restart(id); err != nil {
		utils.HandleError(c, err)
		return
	}

	utils.Success(c, http.StatusOK, "instance restarted", nil)
}

// instanceToInternalResponse converts an Instance to the response shape expected
// by openclaw-service (Five-Star AI island).
func instanceToInternalResponse(inst *models.Instance, gatewayBase string, client *k8s.Client) gin.H {
	namespace := ""
	serviceName := ""
	proxyTargetBase := ""
	sidecarServiceDNS := ""

	if client != nil {
		namespace = client.GetNamespace(inst.UserID)
		serviceName = client.GetServiceName(inst.ID, inst.Name)
	}

	if gatewayBase != "" && namespace != "" && serviceName != "" {
		proxyTargetBase = gatewayBase + "/api/" + namespace + "/" + serviceName + "/desktop"
		sidecarServiceDNS = gatewayBase + "/api/" + namespace + "/" + serviceName + "/sidecar"
	}

	return gin.H{
		"id":                  inst.ID,
		"name":                inst.Name,
		"user_id":             inst.UserID,
		"type":                inst.Type,
		"status":              inst.Status,
		"proxy_target_base":   proxyTargetBase,
		"sidecar_service_dns": sidecarServiceDNS,
		"access_token":        inst.AccessToken,
		"namespace":           namespace,
		"service_name":        serviceName,
		"pod_name":            inst.PodName,
		"pod_namespace":       inst.PodNamespace,
		"pod_ip":              inst.PodIP,
		"cpu_cores":           inst.CPUCores,
		"memory_gb":           inst.MemoryGB,
		"disk_gb":             inst.DiskGB,
		"gpu_enabled":         inst.GPUEnabled,
		"gpu_count":           inst.GPUCount,
		"os_type":             inst.OSType,
		"os_version":          inst.OSVersion,
		"created_at":          inst.CreatedAt,
		"updated_at":          inst.UpdatedAt,
	}
}

// sidecarGatewayBaseURL reads the sidecar gateway base URL from environment.
func sidecarGatewayBaseURL() string {
	return strings.TrimSpace(os.Getenv("SIDECAR_GATEWAY_BASE_URL"))
}

// BootstrapCreateUser creates a new user without token/admin checks.
func (h *InternalHandler) BootstrapCreateUser(c *gin.Context) {
	var req CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ValidationError(c, err)
		return
	}

	user, err := h.userService.CreateUser(req.Username, req.Email, req.Password, req.Role)
	if err != nil {
		utils.HandleError(c, err)
		return
	}

	utils.Success(c, http.StatusCreated, "User created successfully", gin.H{
		"id":       user.ID,
		"username": user.Username,
		"email":    user.Email,
		"role":     user.Role,
	})
}

// BootstrapCreateInstance creates an instance for a specific user without token/admin checks.
func (h *InternalHandler) BootstrapCreateInstance(c *gin.Context) {
	userIdStr := c.Param("userId")
	userID, err := strconv.Atoi(userIdStr)
	if err != nil {
		utils.Error(c, http.StatusBadRequest, "invalid user id")
		return
	}

	var req CreateInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ValidationError(c, err)
		return
	}

	createReq := services.CreateInstanceRequest{
		Name:                 req.Name,
		Description:          req.Description,
		Type:                 req.Type,
		RuntimeType:          req.RuntimeType,
		CPUCores:             req.CPUCores,
		MemoryGB:             req.MemoryGB,
		DiskGB:               req.DiskGB,
		GPUEnabled:           req.GPUEnabled,
		GPUCount:             req.GPUCount,
		OSType:               req.OSType,
		OSVersion:            req.OSVersion,
		ImageRegistry:        req.ImageRegistry,
		ImageTag:             req.ImageTag,
		EnvironmentOverrides: req.EnvironmentOverrides,
		StorageClass:         req.StorageClass,
		OpenClawConfigPlan:   req.OpenClawConfigPlan,
	}

	instance, err := h.instanceService.Create(userID, createReq)
	if err != nil {
		utils.HandleError(c, err)
		return
	}

	utils.Success(c, http.StatusCreated, "Instance created successfully",
		instanceToInternalResponse(instance, sidecarGatewayBaseURL(), k8s.GetClient()))
}

// BootstrapDeleteInstance deletes an instance without token/admin checks.
func (h *InternalHandler) BootstrapDeleteInstance(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		utils.Error(c, http.StatusBadRequest, "invalid instance id")
		return
	}

	if err := h.instanceService.Delete(id); err != nil {
		if err.Error() == "instance not found" {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "instance not found"})
			return
		}
		utils.HandleError(c, err)
		return
	}

	utils.Success(c, http.StatusOK, "Instance deleted successfully", nil)
}

// BootstrapGetUserByUsername gets a user by username for internal services.
func (h *InternalHandler) BootstrapGetUserByUsername(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" {
		utils.Error(c, http.StatusBadRequest, "username is required")
		return
	}

	user, err := h.userService.GetUserByUsername(username)
	if err != nil {
		if err.Error() == "user not found" {
			utils.Success(c, http.StatusOK, "User not found", nil)
			return
		}
		utils.HandleError(c, err)
		return
	}

	utils.Success(c, http.StatusOK, "User retrieved successfully", gin.H{
		"id":       user.ID,
		"username": user.Username,
		"email":    user.Email,
		"role":     user.Role,
	})
}

func generateInternalToken(instanceID int) string {
	return "internal_" + strconv.Itoa(instanceID) + "_" + strconv.FormatInt(time.Now().Unix(), 10)
}
