package usertoken

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

const InternalTokenHeader = "X-OpenLinker-Internal-Token"

type IntrospectionHandler struct {
	svc interface {
		Introspect(context.Context, string) IntrospectionResponse
	}
	internalSecret string
}

func NewIntrospectionHandler(svc interface {
	Introspect(context.Context, string) IntrospectionResponse
}, internalSecret string) *IntrospectionHandler {
	return &IntrospectionHandler{svc: svc, internalSecret: strings.TrimSpace(internalSecret)}
}

func (h *IntrospectionHandler) Register(e *echo.Echo) {
	e.POST("/internal/user-tokens/introspect", h.Introspect)
}

func (h *IntrospectionHandler) Introspect(c echo.Context) error {
	provided := strings.TrimSpace(c.Request().Header.Get(InternalTokenHeader))
	if h.internalSecret == "" || len(provided) != len(h.internalSecret) ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(h.internalSecret)) != 1 {
		return httpx.Unauthorized("内部服务凭据无效")
	}
	var req IntrospectionRequest
	if err := c.Bind(&req); err != nil {
		return httpx.BadRequest("请求体格式错误")
	}
	resp := h.svc.Introspect(c.Request().Context(), req.Token)
	return c.JSON(http.StatusOK, resp)
}
