package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/pkg/ipmatch"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// rotation_gate.go serves the signed-in user's deposit eligibility and explicit
// risk confirmation. The status and confirmation routes stay reachable when
// deposits are blocked. Deposit middleware runs after authentication and stops
// dispatch if the gate cannot establish that the request is eligible.

// KeyRotationGateHandler exposes self-service remediation and deposit admission middleware.
type KeyRotationGateHandler struct {
	gate *apikey.KeyRotationGate // deposit policy and audit service
}

// acknowledgeKeyRotationRiskRequest accepts the user's explicit affirmative consent.
type acknowledgeKeyRotationRiskRequest struct {
	AcknowledgeRisk bool `json:"acknowledge_risk"` // explicit affirmative consent
}

// NewKeyRotationGateHandler binds HTTP routes to the deposit gate.
func NewKeyRotationGateHandler(gate *apikey.KeyRotationGate) *KeyRotationGateHandler {
	return &KeyRotationGateHandler{gate: gate}
}

// Register binds both self-service routes below the JWT-authenticated /api/v1
// group. The status and confirmation endpoints must remain reachable without
// the deposit guard to let a blocked user inspect or address their status.
func (h *KeyRotationGateHandler) Register(authenticated *gin.RouterGroup) {
	authenticated.GET("/user/key-rotation-gate", h.getStatus)
	authenticated.POST("/user/security/key-rotation-risk-ack", h.acknowledgeRisk)
}

// Guard checks deposit policy after authentication and before the deposit
// handler. Missing identity or an unavailable evaluation stops the request.
// A denial carries the remediation code consumed by the deposit UI.
func (h *KeyRotationGateHandler) Guard(c *gin.Context) {
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, httpx.Response{
			Code: http.StatusUnauthorized, Message: "User not authenticated",
		})
		return
	}
	status, err := h.gate.Check(c.Request.Context(), keyRotationActor(c, subject.UserID))
	if httpx.ErrorFrom(c, err) {
		c.Abort()
		return
	}
	if status.Required {
		// The fixed-source response carries its remediation message in addition to the machine-readable reason.
		c.AbortWithStatusJSON(http.StatusForbidden, httpx.Response{
			Code:    http.StatusForbidden,
			Message: status.Message,
			Reason:  status.ErrorCode,
			Metadata: map[string]string{
				"error_code":       status.ErrorCode,
				"remediation_path": status.RemediationPath,
				"reason":           status.Reason,
			},
		})
		return
	}
	c.Next()
}

// getStatus returns current deposit eligibility for the authenticated user.
func (h *KeyRotationGateHandler) getStatus(c *gin.Context) {
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		httpx.Unauthorized(c, "User not authenticated")
		return
	}
	status, err := h.gate.Evaluate(c.Request.Context(), subject.UserID)
	if !httpx.ErrorFrom(c, err) {
		httpx.Success(c, status)
	}
}

// acknowledgeRisk records affirmative consent from the authenticated user only.
// A successful response carries the persisted lifetime after the dedicated
// audit and the confirmation have both committed to the database.
func (h *KeyRotationGateHandler) acknowledgeRisk(c *gin.Context) {
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		httpx.Unauthorized(c, "User not authenticated")
		return
	}
	var input acknowledgeKeyRotationRiskRequest
	if err := c.ShouldBindJSON(&input); err != nil || !input.AcknowledgeRisk {
		c.JSON(http.StatusBadRequest, httpx.Response{
			Code: http.StatusBadRequest, Message: "acknowledge_risk must be true",
		})
		return
	}
	ack, err := h.gate.Acknowledge(c.Request.Context(), keyRotationActor(c, subject.UserID))
	if apperror.Reason(err) == apikey.KeyRotationRiskAckDisabled {
		c.JSON(http.StatusForbidden, httpx.Response{
			Code:    http.StatusForbidden,
			Message: apperror.Message(err),
			Reason:  apikey.KeyRotationRiskAckDisabled,
		})
		return
	}
	if httpx.ErrorFrom(c, err) {
		return
	}
	httpx.Success(c, ack)
}

// keyRotationActor captures audit metadata without credential or body data.
// The authenticated subject supplies the actor ID, and the security IP resolver
// applies the same trusted forwarding policy as session binding.
func keyRotationActor(c *gin.Context, userID int64) apikey.KeyRotationActor {
	path := c.FullPath()
	if path == "" {
		path = c.Request.URL.Path
	}
	ctx := c.Request.Context()
	requestID, _ := ctx.Value(telemetry.RequestID).(string)
	clientIP := ipmatch.NormalizeIP(c.ClientIP())
	userAgent := c.Request.UserAgent()
	if binding := identity.SessionBindingFromContext(ctx); binding != nil {
		if strings.TrimSpace(binding.IP) != "" {
			clientIP = binding.IP
		}
		userAgent = binding.UserAgent
	}
	return apikey.KeyRotationActor{
		UserID:    userID,
		Method:    c.Request.Method,
		Path:      path,
		RequestID: requestID,
		ClientIP:  clientIP,
		UserAgent: strings.ToValidUTF8(userAgent, "�"),
	}
}
