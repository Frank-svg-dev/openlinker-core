package task

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/OpenLinker-ai/openlinker-core/pkg/auth"
	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

// Handler 任务驱动 A 形态 HTTP 入口。
type Handler struct {
	svc       *Service
	validator *validator.Validate
}

// NewHandler 构造 Handler。
func NewHandler(svc *Service) *Handler {
	return &Handler{
		svc:       svc,
		validator: validator.New(validator.WithRequiredStructEnabled()),
	}
}

// RegisterProtected 注册需要 JWT 的端点。
//
//	POST /tasks/recommend       自然语言 → 推荐 Top3 Agent
//	POST /tasks/:id/choose      用户选定推荐里某个 Agent
//	POST /tasks/:id/run         从任务直接启动一次 Agent 运行
//	GET  /tasks/me              我的任务历史（最多 20 条）
//	GET  /tasks/:id             单个任务详情（含推荐卡回填）
func (h *Handler) RegisterProtected(api *echo.Group, jwtMiddleware echo.MiddlewareFunc) {
	api.GET("/task-templates", h.ListTaskTemplates)

	g := api.Group("/tasks", jwtMiddleware)
	g.POST("/recommend", h.Recommend)
	g.POST("/:id/choose", h.Choose)
	g.POST("/:id/run", h.Run)
	g.GET("/me", h.ListMine)
	g.GET("/:id", h.GetByID)
}

// ListTaskTemplates GET /task-templates
func (h *Handler) ListTaskTemplates(c echo.Context) error {
	items, err := h.svc.ListTaskTemplates(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

// Recommend POST /tasks/recommend
func (h *Handler) Recommend(c echo.Context) error {
	if err := auth.RequirePermission(c, "tasks:create", "task", nil); err != nil {
		return err
	}
	uid, err := userIDFromCtx(c)
	if err != nil {
		return err
	}
	var req RecommendRequest
	if err := c.Bind(&req); err != nil {
		return httpx.BadRequest("请求体格式错误")
	}
	if err := h.validator.Struct(&req); err != nil {
		return httpx.Unprocessable(err.Error())
	}
	resp, err := h.svc.Recommend(c.Request().Context(), uid, &req)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// Choose POST /tasks/:id/choose
func (h *Handler) Choose(c echo.Context) error {
	uid, err := userIDFromCtx(c)
	if err != nil {
		return err
	}
	taskID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return httpx.BadRequest("id 不是合法 uuid")
	}
	if err := auth.RequirePermission(c, "tasks:create", "task", &taskID); err != nil {
		return err
	}
	var req ChooseRequest
	if err := c.Bind(&req); err != nil {
		return httpx.BadRequest("请求体格式错误")
	}
	if err := h.validator.Struct(&req); err != nil {
		return httpx.Unprocessable(err.Error())
	}
	if err := h.svc.Choose(c.Request().Context(), taskID, uid, req.AgentID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// Run POST /tasks/:id/run
func (h *Handler) Run(c echo.Context) error {
	uid, err := userIDFromCtx(c)
	if err != nil {
		return err
	}
	taskID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return httpx.BadRequest("id 不是合法 uuid")
	}
	if err := auth.RequirePermission(c, "tasks:run", "task", &taskID); err != nil {
		return err
	}
	var req RunTaskRequest
	if err := c.Bind(&req); err != nil {
		return httpx.BadRequest("请求体格式错误")
	}
	if err := h.validator.Struct(&req); err != nil {
		return httpx.Unprocessable(err.Error())
	}
	resp, err := h.svc.RunTask(c.Request().Context(), taskID, uid, &req)
	if err != nil {
		return err
	}
	if resp == nil || resp.Run == nil || strings.TrimSpace(resp.Run.RunID) == "" {
		return httpx.Internal("创建调用记录失败")
	}
	c.Response().Header().Set(echo.HeaderLocation, "/api/v1/runs/"+resp.Run.RunID)
	status := http.StatusCreated
	if resp.Run.Replayed {
		c.Response().Header().Set("Idempotency-Replayed", "true")
		status = http.StatusOK
		runStatus := strings.ToLower(strings.TrimSpace(resp.Run.Status))
		if runStatus == "running" || runStatus == "pending" {
			status = http.StatusAccepted
		}
	}
	return c.JSON(status, resp)
}

// ListMine GET /tasks/me?q=&status=&sort=created_desc&page=1&size=20
func (h *Handler) ListMine(c echo.Context) error {
	if err := auth.RequirePermission(c, "tasks:read", "task", nil); err != nil {
		return err
	}
	uid, err := userIDFromCtx(c)
	if err != nil {
		return err
	}
	page := int32(1)
	if v := c.QueryParam("page"); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 32); perr == nil && n > 0 {
			page = int32(n) // #nosec G115 -- ParseInt bitSize=32 guarantees range.
		}
	}
	size := int32(20)
	if v := firstNonEmpty(c.QueryParam("size"), c.QueryParam("limit")); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 32); perr == nil && n > 0 {
			if n > 50 {
				n = 50
			}
			size = int32(n) // #nosec G115 -- ParseInt bitSize=32 guarantees range, then size is capped.
		}
	}
	resp, err := h.svc.ListMinePage(
		c.Request().Context(),
		uid,
		c.QueryParam("q"),
		c.QueryParam("status"),
		c.QueryParam("sort"),
		page,
		size,
	)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// GetByID GET /tasks/:id
func (h *Handler) GetByID(c echo.Context) error {
	uid, err := userIDFromCtx(c)
	if err != nil {
		return err
	}
	taskID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		// Unknown static paths such as the removed /tasks/board endpoint are
		// captured by Echo's /tasks/:id route. Treat them as absent resources so
		// removed Task-market routes do not leak a misleading validation error.
		return httpx.NotFound("任务不存在")
	}
	if err := auth.RequirePermission(c, "tasks:read", "task", &taskID); err != nil {
		return err
	}
	resp, err := h.svc.GetByID(c.Request().Context(), taskID, uid)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// userIDFromCtx 从 echo.Context 取当前登录 user uuid（JWT 中间件已写入）。
func userIDFromCtx(c echo.Context) (uuid.UUID, error) {
	idStr := httpx.UserIDFrom(c)
	if idStr == "" {
		return uuid.Nil, httpx.Unauthorized("")
	}
	uid, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, httpx.Unauthorized("token 无效")
	}
	return uid, nil
}
