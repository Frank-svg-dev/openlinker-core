package task_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
	"github.com/OpenLinker-ai/openlinker-core/pkg/task"
)

const truncateTaskTables = "TRUNCATE webhook_deliveries, runs, task_queries, agent_skills, agents, users RESTART IDENTITY CASCADE"

type fakeSkillRecommender struct {
	skills      []db.Skill
	matches     []task.AgentMatch
	gotSkillIDs []string
	listErr     error
}

type fakeRuntimeStarter struct {
	gotUserID uuid.UUID
	gotReq    *runtime.RunRequest
	gotSource string
	resp      *runtime.RunResponse
}

func (f *fakeRuntimeStarter) StartRun(_ context.Context, userID uuid.UUID, req *runtime.RunRequest, source string) (*runtime.RunResponse, error) {
	f.gotUserID = userID
	f.gotReq = req
	f.gotSource = source
	if f.resp != nil {
		return f.resp, nil
	}
	return &runtime.RunResponse{RunID: uuid.NewString(), Status: "running", Source: source}, nil
}

func (f *fakeSkillRecommender) ListAll(context.Context) ([]db.Skill, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]db.Skill{}, f.skills...), nil
}

func (f *fakeSkillRecommender) RecommendAgentsBySkills(_ context.Context, skillIDs []string, limit int) ([]task.AgentMatch, error) {
	f.gotSkillIDs = append([]string{}, skillIDs...)
	out := append([]task.AgentMatch{}, f.matches...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func setupTaskTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过 task 集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx))
	_, err = pool.Exec(ctx, truncateTaskTables)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanCtx, truncateTaskTables)
		pool.Close()
	})
	return pool
}

func testSkills() []db.Skill {
	now := time.Now()
	return []db.Skill{
		{
			ID:          "content/summarization",
			Category:    "content",
			Name:        "摘要",
			Description: "长文压缩、要点提取、会议纪要生成",
			SortOrder:   1,
			CreatedAt:   now,
		},
		{
			ID:          "content/structured-data",
			Category:    "content",
			Name:        "结构化抽取",
			Description: "从非结构化文本中抽取字段",
			SortOrder:   2,
			CreatedAt:   now,
		},
		{
			ID:          "data/sql-query",
			Category:    "data",
			Name:        "SQL 查询",
			Description: "自然语言转 SQL、慢查询优化、schema 解读",
			SortOrder:   1,
			CreatedAt:   now,
		},
		{
			ID:          "data/analysis",
			Category:    "data",
			Name:        "数据分析",
			Description: "统计、趋势、同比环比、生成洞察文字",
			SortOrder:   2,
			CreatedAt:   now,
		},
		{
			ID:          "dev/code-review",
			Category:    "dev",
			Name:        "代码审查",
			Description: "PR 评审、风格检查、潜在 bug 提示",
			SortOrder:   1,
			CreatedAt:   now,
		},
		{
			ID:          "ops/web-scraping",
			Category:    "ops",
			Name:        "网页抓取",
			Description: "抓取站点 / API / 监控 / 价格追踪",
			SortOrder:   1,
			CreatedAt:   now,
		},
		{
			ID:          "ops/document-generate",
			Category:    "ops",
			Name:        "文档生成",
			Description: "PDF / Word / 报告 / 合同 / 简历",
			SortOrder:   2,
			CreatedAt:   now,
		},
	}
}

func insertTaskUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, display_name)
		 VALUES ($1, $2, 'x', 'Task User')`,
		id, "task-u-"+id.String()[:8]+"@example.com")
	require.NoError(t, err)
	return id
}

func insertTaskCreator(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, display_name, is_creator, creator_verified)
		 VALUES ($1, $2, 'x', 'Task Creator', TRUE, TRUE)`,
		id, "task-c-"+id.String()[:8]+"@example.com")
	require.NoError(t, err)
	return id
}

func insertTaskAgent(t *testing.T, pool *pgxpool.Pool, creatorID uuid.UUID, slug, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	lifecycle := "active"
	cert := "unreviewed"
	switch status {
	case "approved":
		// defaults
	case "disabled":
		lifecycle = "disabled"
	case "pending":
		cert = "pending"
	case "rejected":
		cert = "rejected"
	default:
		require.Failf(t, "insertTaskAgent unknown legacy status", "%q", status)
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO agents (
			id, creator_id, slug, name, description, endpoint_url, price_per_call_cents,
			tags, lifecycle_status, visibility, certification_status
		) VALUES ($1, $2, $3, $4, 'Task test agent', $5, 100, '{data}', $6, 'public', $7)`,
		id, creatorID, slug, "Task Agent "+slug, "https://example.com/agent/"+slug, lifecycle, cert)
	require.NoError(t, err)
	return id
}

func insertTaskAgentSkills(t *testing.T, pool *pgxpool.Pool, agentID uuid.UUID, skillIDs ...string) {
	t.Helper()
	for _, skillID := range skillIDs {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO agent_skills (agent_id, skill_id) VALUES ($1, $2)`,
			agentID, skillID)
		require.NoError(t, err)
	}
}

func insertSuccessfulTaskRun(t *testing.T, pool *pgxpool.Pool, userID, agentID uuid.UUID, summary string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO runs (
			id, user_id, agent_id, input, output, status,
			cost_cents, platform_fee_cents, creator_revenue_cents,
			duration_ms, source, finished_at
		) VALUES (
			$1, $2, $3, '{"text":"task"}'::jsonb, $4::jsonb, 'success',
			0, 0, 0, 12, 'web', NOW()
		)`,
		id, userID, agentID, `{"summary": "`+summary+`"}`)
	require.NoError(t, err)
	return id
}

func assertTaskHTTPStatus(t *testing.T, err error, want int) {
	t.Helper()
	require.Error(t, err)
	var he *httpx.HTTPError
	require.True(t, errors.As(err, &he), "expected *httpx.HTTPError, got %T (%v)", err, err)
	assert.Equal(t, want, he.Status)
}

func decodeTaskHandlerJSON(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out))
}

func newTaskHandlerContext(e *echo.Echo, method, path, body string, userID, taskID uuid.UUID) (echo.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	c := e.NewContext(req, rec)
	if userID != uuid.Nil {
		c.Set(string(httpx.CtxKeyUserID), userID.String())
	}
	if taskID != uuid.Nil {
		c.SetParamNames("id")
		c.SetParamValues(taskID.String())
	}
	return c, rec
}

func TestRecommendPersistsAndDetailRoundTrip(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	creatorID := insertTaskCreator(t, pool)
	firstAgent := insertTaskAgent(t, pool, creatorID, "task-first-"+uuid.NewString()[:8], "approved")
	secondAgent := insertTaskAgent(t, pool, creatorID, "task-second-"+uuid.NewString()[:8], "approved")
	insertTaskAgentSkills(t, pool, firstAgent, "data/sql-query", "data/analysis")
	insertTaskAgentSkills(t, pool, secondAgent, "data/sql-query")

	fake := &fakeSkillRecommender{
		skills: testSkills(),
		matches: []task.AgentMatch{
			{AgentID: firstAgent, MatchCount: 2},
			{AgentID: secondAgent, MatchCount: 1},
		},
	}
	svc := task.NewService(pool, nil, fake)

	resp, err := svc.Recommend(context.Background(), userID, &task.RecommendRequest{
		Query:    "请帮我做 SQL 查询和数据分析",
		SkillIDs: []string{"data/sql-query"},
		MCPTools: []string{"create_task", "run_agent"},
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, resp.TaskID)
	assert.Equal(t, "private", resp.Visibility)
	require.GreaterOrEqual(t, len(fake.gotSkillIDs), 2)
	assert.Equal(t, []string{"data/sql-query", "data/analysis"}, fake.gotSkillIDs[:2])
	require.Len(t, resp.ParsedSkillRefs, len(fake.gotSkillIDs))
	assert.Equal(t, "SQL 查询", resp.ParsedSkillRefs[0].Name)
	assert.Equal(t, []string{"create_task", "run_agent"}, resp.MCPTools)
	require.Len(t, resp.MCPToolRefs, 2)
	assert.Equal(t, "create_task", resp.MCPToolRefs[0].Name)
	require.Len(t, resp.Recommendations, 2)
	assert.Equal(t, firstAgent.String(), resp.Recommendations[0].Agent.ID)
	assert.InDelta(t, float32(2)/float32(len(fake.gotSkillIDs)), resp.Recommendations[0].MatchScore, 0.001)
	assert.Equal(t, []string{"data"}, resp.Recommendations[0].Agent.Tags)
	require.Len(t, resp.Recommendations[0].MatchedSkills, 2)
	assert.Equal(t, "data/sql-query", resp.Recommendations[0].MatchedSkills[0].ID)
	assert.Equal(t, "匹配 SQL 查询 + 数据分析", resp.Recommendations[0].Why)
	assert.Equal(t, secondAgent.String(), resp.Recommendations[1].Agent.ID)
	require.Len(t, resp.Recommendations[1].MatchedSkills, 1)
	assert.Equal(t, "data/sql-query", resp.Recommendations[1].MatchedSkills[0].ID)
	assert.Equal(t, "匹配 SQL 查询", resp.Recommendations[1].Why)
	assert.NotContains(t, resp.Recommendations[1].Why, "数据分析")

	var stored []uuid.UUID
	var storedMCP []string
	err = pool.QueryRow(context.Background(),
		`SELECT recommended_agent_ids, mcp_tools FROM task_queries WHERE id=$1`, resp.TaskID).Scan(&stored, &storedMCP)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{firstAgent, secondAgent}, stored)
	assert.Equal(t, []string{"create_task", "run_agent"}, storedMCP)

	detail, err := svc.GetByID(context.Background(), resp.TaskID, userID)
	require.NoError(t, err)
	require.Len(t, detail.ParsedSkillRefs, len(fake.gotSkillIDs))
	assert.Equal(t, []string{"create_task", "run_agent"}, detail.MCPTools)
	require.Len(t, detail.MCPToolRefs, 2)
	require.Len(t, detail.Recommendations, 2)
	assert.Equal(t, firstAgent.String(), detail.Recommendations[0].Agent.ID)
	require.Len(t, detail.Recommendations[0].MatchedSkills, 2)
	assert.Equal(t, "匹配 SQL 查询 + 数据分析", detail.Recommendations[0].Why)
	assert.Equal(t, "匹配 SQL 查询", detail.Recommendations[1].Why)
	assert.Equal(t, secondAgent.String(), detail.Recommendations[1].Agent.ID)
	require.Len(t, detail.Recommendations[1].MatchedSkills, 1)

	require.NoError(t, svc.Choose(context.Background(), resp.TaskID, userID, secondAgent))
	history, err := svc.ListMine(context.Background(), userID, 20)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.NotNil(t, history[0].ChosenAgentID)
	assert.Equal(t, secondAgent.String(), *history[0].ChosenAgentID)
	assert.Equal(t, "matched", history[0].Status)
	assert.Equal(t, []string{"create_task", "run_agent"}, history[0].MCPTools)
}

func TestRecommendPersistsPendingExplicitSkill(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	fake := &fakeSkillRecommender{skills: testSkills()}
	svc := task.NewService(pool, nil, fake)

	resp, err := svc.Recommend(context.Background(), userID, &task.RecommendRequest{
		Query:    "I need an Agent for a missing capability",
		SkillIDs: []string{"ai/custom-capability"},
		MCPTools: []string{"create_task", "run_agent"},
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, resp.TaskID)
	assert.Equal(t, []string{"ai/custom-capability"}, resp.ParsedSkills)
	require.Len(t, resp.ParsedSkillRefs, 1)
	assert.Equal(t, "ai/custom-capability", resp.ParsedSkillRefs[0].ID)
	assert.Equal(t, "ai/custom-capability", resp.ParsedSkillRefs[0].Name)
	assert.Empty(t, resp.Recommendations)
	require.NotNil(t, resp.NextAction)
	assert.Equal(t, "connect_agent", resp.NextAction.Type)
	assert.Equal(t, "no_public_agent", resp.NextAction.ReasonCode)
	assert.Contains(t, resp.NextAction.Href, "/publish?")
	assert.Contains(t, resp.NextAction.Href, "skill=ai%2Fcustom-capability")

	var parsed []string
	var recommended []uuid.UUID
	err = pool.QueryRow(context.Background(),
		`SELECT parsed_skills, recommended_agent_ids FROM task_queries WHERE id=$1`,
		resp.TaskID,
	).Scan(&parsed, &recommended)
	require.NoError(t, err)
	assert.Equal(t, []string{"ai/custom-capability"}, parsed)
	assert.Empty(t, recommended)

	detail, err := svc.GetByID(context.Background(), resp.TaskID, userID)
	require.NoError(t, err)
	assert.Equal(t, []string{"ai/custom-capability"}, detail.ParsedSkills)
	require.Len(t, detail.ParsedSkillRefs, 1)
	assert.Equal(t, "ai/custom-capability", detail.ParsedSkillRefs[0].Name)
	assert.Empty(t, detail.Recommendations)
}

func TestRecommendPendingExplicitSkillLimitsRecommendationScope(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	fake := &fakeSkillRecommender{skills: testSkills()}
	svc := task.NewService(pool, nil, fake)

	resp, err := svc.Recommend(context.Background(), userID, &task.RecommendRequest{
		Query:    "请帮我做 SQL 查询和数据分析，但是需要一个当前目录没有的新能力",
		SkillIDs: []string{"ai/custom-capability"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"ai/custom-capability"}, fake.gotSkillIDs)
	assert.Contains(t, resp.ParsedSkills, "ai/custom-capability")
	assert.Contains(t, resp.ParsedSkills, "data/sql-query")
	assert.Contains(t, resp.ParsedSkills, "data/analysis")
	assert.Empty(t, resp.Recommendations)
	require.NotNil(t, resp.NextAction)
	assert.Equal(t, "connect_agent", resp.NextAction.Type)
	assert.Contains(t, resp.NextAction.Href, "skill_ids=")
}

func TestRecommendPreferredAgentSlugRanksFirst(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	creatorID := insertTaskCreator(t, pool)
	firstSlug := "task-auto-" + uuid.NewString()[:8]
	preferredSlug := "task-preferred-" + uuid.NewString()[:8]
	firstAgent := insertTaskAgent(t, pool, creatorID, firstSlug, "approved")
	preferredAgent := insertTaskAgent(t, pool, creatorID, preferredSlug, "approved")
	insertTaskAgentSkills(t, pool, firstAgent, "dev/code-review")
	insertTaskAgentSkills(t, pool, preferredAgent, "dev/code-review")

	fake := &fakeSkillRecommender{
		skills: testSkills(),
		matches: []task.AgentMatch{
			{AgentID: firstAgent, MatchCount: 1},
			{AgentID: preferredAgent, MatchCount: 1},
		},
	}
	svc := task.NewService(pool, nil, fake)

	resp, err := svc.Recommend(context.Background(), userID, &task.RecommendRequest{
		Query:      "请帮我审查这段代码有没有明显问题",
		SkillIDs:   []string{"dev/code-review"},
		AgentSlugs: []string{preferredSlug},
	})
	require.NoError(t, err)
	require.Len(t, resp.Recommendations, 2)
	assert.Equal(t, preferredAgent.String(), resp.Recommendations[0].Agent.ID)
	assert.Equal(t, preferredSlug, resp.Recommendations[0].Agent.Slug)
	assert.Equal(t, firstAgent.String(), resp.Recommendations[1].Agent.ID)
	require.Len(t, resp.Recommendations[0].MatchedSkills, 1)
	assert.Equal(t, "dev/code-review", resp.Recommendations[0].MatchedSkills[0].ID)

	var stored []uuid.UUID
	err = pool.QueryRow(context.Background(),
		`SELECT recommended_agent_ids FROM task_queries WHERE id=$1`, resp.TaskID).Scan(&stored)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{preferredAgent, firstAgent}, stored)
}

func TestTaskTemplatesAndTemplateIDDriveRecommendationSkills(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	creatorID := insertTaskCreator(t, pool)
	agentID := insertTaskAgent(t, pool, creatorID, "task-support-"+uuid.NewString()[:8], "approved")
	insertTaskAgentSkills(t, pool, agentID, "content/summarization", "content/structured-data")

	fake := &fakeSkillRecommender{
		skills:  testSkills(),
		matches: []task.AgentMatch{{AgentID: agentID, MatchCount: 2}},
	}
	svc := task.NewService(pool, nil, fake)

	templates, err := svc.ListTaskTemplates(context.Background())
	require.NoError(t, err)
	require.Len(t, templates, 5)
	assert.Equal(t, "support-review", templates[0].ID)
	assert.Equal(t, "private", templates[0].DefaultVisibility)
	assert.Equal(t, []string{"content/summarization", "content/structured-data"}, templates[0].RequiredSkillIDs)
	assert.Equal(t, []string{"create_task", "run_agent", "get_run"}, templates[0].RequiredMCPTools)
	require.Len(t, templates[0].RequiredSkillRefs, 2)
	require.Len(t, templates[0].RequiredMCPToolRefs, 3)
	assert.Equal(t, "摘要", templates[0].RequiredSkillRefs[0].Name)
	assert.Equal(t, "create_task", templates[0].RequiredMCPToolRefs[0].Name)

	resp, err := svc.Recommend(context.Background(), userID, &task.RecommendRequest{
		TemplateID: "support-review",
		Query:      "请复盘这段客服对话，输出问题分类和下一步动作",
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(fake.gotSkillIDs), 2)
	assert.Equal(t, []string{"content/summarization", "content/structured-data"}, fake.gotSkillIDs[:2])
	require.Len(t, resp.Recommendations, 1)
	assert.Equal(t, agentID.String(), resp.Recommendations[0].Agent.ID)
	require.Len(t, resp.ParsedSkillRefs, len(fake.gotSkillIDs))
	assert.Equal(t, "结构化抽取", resp.ParsedSkillRefs[1].Name)
}

func TestRecommendWithoutMatchesReturnsPrivateDraftNextAction(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	svc := task.NewService(pool, nil, &fakeSkillRecommender{skills: testSkills()})

	resp, err := svc.Recommend(context.Background(), userID, &task.RecommendRequest{
		Query:    "请帮我做 SQL 查询和数据分析",
		SkillIDs: []string{"data/sql-query"},
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, resp.TaskID)
	assert.Equal(t, "private", resp.Visibility)
	require.Empty(t, resp.Recommendations)
	require.NotNil(t, resp.NextAction)
	assert.Equal(t, "connect_agent", resp.NextAction.Type)
	assert.Contains(t, resp.NextAction.Href, "/publish?")
	assert.Contains(t, resp.NextAction.Href, "q=")
	assert.Contains(t, resp.NextAction.Href, "data%2Fsql-query")
	assert.NotContains(t, resp.NextAction.Href, resp.TaskID.String())

}

func TestTaskHandlersListMineReturnsOnlyPrivateOwnerTasks(t *testing.T) {
	pool := setupTaskTestDB(t)
	ownerID := insertTaskUser(t, pool)
	otherID := insertTaskUser(t, pool)
	creatorID := insertTaskCreator(t, pool)
	agentID := insertTaskAgent(t, pool, creatorID, "task-handler-"+uuid.NewString()[:8], "approved")

	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO task_queries (
			user_id, query, parsed_skills, mcp_tools, recommended_agent_ids
		) VALUES ($1, 'owner private task', '{data/sql-query}', '{create_task}', $2)`,
		ownerID, []uuid.UUID{agentID})
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO task_queries (
			user_id, query, parsed_skills, mcp_tools, recommended_agent_ids
		) VALUES ($1, 'other private task', '{data/sql-query}', '{}', $2)`,
		otherID, []uuid.UUID{agentID})
	require.NoError(t, err)

	h := task.NewHandler(task.NewService(pool, nil, &fakeSkillRecommender{skills: testSkills()}))
	e := echo.New()
	mineRec := httptest.NewRecorder()
	mineCtx := e.NewContext(httptest.NewRequest(http.MethodGet, "/api/v1/tasks/me?limit=99", nil), mineRec)
	mineCtx.Set(string(httpx.CtxKeyUserID), ownerID.String())
	require.NoError(t, h.ListMine(mineCtx))
	require.Equal(t, http.StatusOK, mineRec.Code)
	var body struct {
		Items []task.HistoryItem `json:"items"`
	}
	decodeTaskHandlerJSON(t, mineRec, &body)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "owner private task", body.Items[0].Query)
	assert.Equal(t, "private", body.Items[0].Visibility)
}

func TestRunTaskUsesSelectedAgentAndTaskInput(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	creatorID := insertTaskCreator(t, pool)
	agentID := insertTaskAgent(t, pool, creatorID, "task-run-"+uuid.NewString()[:8], "approved")

	var taskID uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO task_queries (
			user_id, query, parsed_skills, mcp_tools, recommended_agent_ids, chosen_agent_id, chosen_at
		) VALUES (
			$1, '做 SQL 查询', '{data/sql-query}', '{run_agent}', $2, $3, NOW()
		) RETURNING id`,
		userID, []uuid.UUID{agentID}, agentID).Scan(&taskID)
	require.NoError(t, err)

	runner := &fakeRuntimeStarter{}
	svc := task.NewService(pool, nil, &fakeSkillRecommender{skills: testSkills()})
	svc.SetRunStarter(runner)
	resp, err := svc.RunTask(context.Background(), taskID, userID, &task.RunTaskRequest{
		AgentID:        agentID,
		IdempotencyKey: "task-selected-agent-run-1",
	})
	require.NoError(t, err)
	assert.Equal(t, taskID.String(), resp.TaskID)
	assert.Equal(t, "matched", resp.Status)
	require.NotNil(t, runner.gotReq)
	assert.Equal(t, agentID.String(), runner.gotReq.AgentID)
	assert.Equal(t, "做 SQL 查询", runner.gotReq.Input["text"])
	assert.Equal(t, taskID.String(), runner.gotReq.Metadata["task_id"])
	assert.Equal(t, []string{"run_agent"}, runner.gotReq.Metadata["used_mcp_tools"])
	assert.Equal(t, "task-selected-agent-run-1", runner.gotReq.IdempotencyKey)
	assert.Equal(t, "task", runner.gotReq.CreationProtocol)
	assert.Equal(t, "run", runner.gotReq.CreationMethod)
	assert.Equal(t, "web", runner.gotSource)
	assert.Equal(t, userID, runner.gotUserID)
}

func TestRunTaskRejectsMissingOrUnsafeIdempotencyKeyBeforeDatabaseLookup(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		wantClass runtime.IdempotencyErrorClass
	}{
		{name: "missing", wantClass: runtime.IdempotencyErrorKeyRequired},
		{name: "control character", key: "task\nrun", wantClass: runtime.IdempotencyErrorKeyInvalid},
		{name: "too long", key: strings.Repeat("x", 256), wantClass: runtime.IdempotencyErrorKeyInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRuntimeStarter{}
			svc := task.NewService(nil, nil, nil)
			svc.SetRunStarter(runner)

			_, err := svc.RunTask(context.Background(), uuid.New(), uuid.New(), &task.RunTaskRequest{
				AgentID:        uuid.New(),
				IdempotencyKey: tt.key,
			})
			require.Error(t, err)
			require.Nil(t, runner.gotReq)

			var httpErr *httpx.HTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, http.StatusUnprocessableEntity, httpErr.Status)
			require.Equal(t, httpx.ErrorCode(tt.wantClass), httpErr.Code)
			if tt.key != "" {
				require.NotContains(t, httpErr.Message, tt.key)
			}
		})
	}
}

func TestRecommendRejectsUnknownAssociations(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	svc := task.NewService(pool, nil, &fakeSkillRecommender{skills: testSkills()})

	_, err := svc.Recommend(context.Background(), userID, &task.RecommendRequest{
		Query:    "请帮我做 SQL 查询和数据分析",
		SkillIDs: []string{"Missing Skill"},
	})
	assertTaskHTTPStatus(t, err, http.StatusUnprocessableEntity)

	_, err = svc.Recommend(context.Background(), userID, &task.RecommendRequest{
		Query:    "请帮我做 SQL 查询和数据分析",
		MCPTools: []string{"unknown_tool"},
	})
	assertTaskHTTPStatus(t, err, http.StatusUnprocessableEntity)
}

func TestChooseRejectsUnrecommendedAndWrongOwner(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	otherUserID := insertTaskUser(t, pool)
	creatorID := insertTaskCreator(t, pool)
	recommended := insertTaskAgent(t, pool, creatorID, "task-recommended-"+uuid.NewString()[:8], "approved")
	notRecommended := insertTaskAgent(t, pool, creatorID, "task-not-rec-"+uuid.NewString()[:8], "approved")

	var taskID uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO task_queries (user_id, query, parsed_skills, recommended_agent_ids)
		 VALUES ($1, '做 SQL 查询', '{data/sql-query}', $2)
		 RETURNING id`,
		userID, []uuid.UUID{recommended}).Scan(&taskID)
	require.NoError(t, err)

	err = task.NewService(pool, nil, &fakeSkillRecommender{skills: testSkills()}).
		Choose(context.Background(), taskID, userID, notRecommended)
	assertTaskHTTPStatus(t, err, http.StatusBadRequest)

	err = task.NewService(pool, nil, &fakeSkillRecommender{skills: testSkills()}).
		Choose(context.Background(), taskID, otherUserID, recommended)
	assertTaskHTTPStatus(t, err, http.StatusNotFound)
}

func TestGetByIDSkipsDisabledHistoricalRecommendation(t *testing.T) {
	pool := setupTaskTestDB(t)
	userID := insertTaskUser(t, pool)
	creatorID := insertTaskCreator(t, pool)
	approvedAgent := insertTaskAgent(t, pool, creatorID, "task-live-"+uuid.NewString()[:8], "approved")
	disabledAgent := insertTaskAgent(t, pool, creatorID, "task-disabled-"+uuid.NewString()[:8], "disabled")

	var taskID uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO task_queries (user_id, query, parsed_skills, recommended_agent_ids)
		 VALUES ($1, '做 SQL 查询', '{data/sql-query}', $2)
		 RETURNING id`,
		userID, []uuid.UUID{disabledAgent, approvedAgent}).Scan(&taskID)
	require.NoError(t, err)

	detail, err := task.NewService(pool, nil, &fakeSkillRecommender{skills: testSkills()}).
		GetByID(context.Background(), taskID, userID)
	require.NoError(t, err)
	require.Len(t, detail.Recommendations, 1)
	assert.Equal(t, approvedAgent.String(), detail.Recommendations[0].Agent.ID)
}
