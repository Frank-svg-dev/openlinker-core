package externalexecution

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

type executionService interface {
	ValidateTarget(context.Context, *Principal, *TargetValidationRequest) (*TargetValidationResponse, error)
	StartExecution(context.Context, *Principal, *ExecutionRequest) (*ExecutionStartResponse, error)
	GetExecution(context.Context, *Principal, string) (*ExecutionStatusResponse, error)
}

type Handler struct {
	svc        executionService
	authorizer *Authorizer
}

func NewHandler(svc executionService, authorizer *Authorizer) *Handler {
	return &Handler{svc: svc, authorizer: authorizer}
}

func (h *Handler) Register(e *echo.Echo) {
	e.POST("/internal/external-execution-targets/validate", h.ValidateTarget)
	e.POST("/internal/external-executions", h.StartExecution)
	e.GET("/internal/external-executions/:external_request_id", h.GetExecution)
}

func (h *Handler) ValidateTarget(c echo.Context) error {
	principal, err := h.authorize(c, ScopeValidateTarget)
	if err != nil {
		return err
	}
	var req TargetValidationRequest
	if err := c.Bind(&req); err != nil {
		return httpx.BadRequest("请求体格式错误")
	}
	resp, err := h.svc.ValidateTarget(c.Request().Context(), principal, &req)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

func (h *Handler) StartExecution(c echo.Context) error {
	principal, err := h.authorize(c, ScopeStartExecution)
	if err != nil {
		return err
	}
	var req ExecutionRequest
	if err := c.Bind(&req); err != nil {
		return httpx.BadRequest("请求体格式错误")
	}
	resp, err := h.svc.StartExecution(c.Request().Context(), principal, &req)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

func (h *Handler) GetExecution(c echo.Context) error {
	principal, err := h.authorize(c, ScopeReadExecution)
	if err != nil {
		return err
	}
	resp, err := h.svc.GetExecution(c.Request().Context(), principal, c.Param("external_request_id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

func (h *Handler) authorize(c echo.Context, scope string) (*Principal, error) {
	if h == nil || h.authorizer == nil {
		return nil, httpx.ServiceUnavailable("外部执行认证未配置")
	}
	authorization := strings.TrimSpace(c.Request().Header.Get(echo.HeaderAuthorization))
	scheme, rawToken, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(strings.TrimSpace(scheme), "Bearer") || strings.TrimSpace(rawToken) == "" {
		return nil, httpx.Unauthorized("缺少外部执行服务凭据")
	}
	return h.authorizer.Authorize(c.Request().Context(), strings.TrimSpace(rawToken), scope)
}
