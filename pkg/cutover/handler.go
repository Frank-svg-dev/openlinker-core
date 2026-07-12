package cutover

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register exposes evidence only. Maintenance mutations remain CLI-only so a
// compromised browser session cannot drain or reopen the runtime cluster.
func (h *Handler) Register(api *echo.Group, jwtMiddleware, adminMiddleware echo.MiddlewareFunc) {
	api.GET("/admin/runtime/maintenance", h.Status, jwtMiddleware, adminMiddleware)
}

func (h *Handler) Status(c echo.Context) error {
	report, err := h.service.Status(c.Request().Context())
	if err != nil {
		return httpx.ServiceUnavailable("Runtime maintenance state is unavailable")
	}
	return c.JSON(http.StatusOK, report)
}
