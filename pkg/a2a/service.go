package a2a

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/OpenLinker-ai/openlinker-core/pkg/credential"
	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

const (
	maxRuntimeTokens      = 10
	maxDelegationDepth    = 8
	defaultParentPage     = 1
	defaultParentPageSize = 10
	maxParentPageSize     = 50
	maxParentSearchLen    = 120
)

type Service struct {
	queries             *db.Queries
	pool                *pgxpool.Pool
	runtime             *runtime.Service
	taskCallbackManager taskCallbackManager
	maxDelegationDepth  int
}

func NewService(pool *pgxpool.Pool, runtimeSvc *runtime.Service) *Service {
	return &Service{
		queries:            db.New(pool),
		pool:               pool,
		runtime:            runtimeSvc,
		maxDelegationDepth: maxDelegationDepth,
	}
}

func (s *Service) CreateRuntimeToken(ctx context.Context, userID, agentID uuid.UUID, req *CreateRuntimeTokenRequest) (*RuntimeTokenResponse, error) {
	agent, err := s.ownerAgent(ctx, userID, agentID)
	if err != nil {
		return nil, err
	}
	count, err := s.queries.CountActiveAgentRuntimeTokens(ctx, agentID)
	if err != nil {
		log.Error().Err(err).Str("agent_id", agentID.String()).Msg("a2a.CreateRuntimeToken: count")
		return nil, httpx.Internal("查询访问令牌失败")
	}
	if count >= maxRuntimeTokens {
		return nil, httpx.BadRequest("访问令牌数量已达上限（10 个），请先撤销旧令牌")
	}

	plaintext, prefix, err := credential.GenerateAgentToken()
	if err != nil {
		return nil, httpx.Internal("生成访问令牌失败")
	}
	token, err := s.queries.CreateAgentRuntimeToken(ctx, db.CreateAgentRuntimeTokenParams{
		AgentID:         agentID,
		CreatedByUserID: userID,
		Name:            strings.TrimSpace(req.Name),
		Prefix:          prefix,
		TokenHash:       credential.FastTokenHash(plaintext),
		Scopes:          runtimeTokenScopesForAgent(agent),
	})
	if err != nil {
		log.Error().Err(err).Str("agent_id", agentID.String()).Msg("a2a.CreateRuntimeToken: insert")
		return nil, httpx.Internal("创建访问令牌失败")
	}
	resp := tokenResponse(token)
	resp.PlaintextToken = plaintext
	return &resp, nil
}

func runtimeTokenScopesForAgent(agent db.Agent) []string {
	scopes := []string{"agent:call"}
	if agent.ConnectionMode == "runtime_pull" || agent.ConnectionMode == "runtime_ws" {
		scopes = append(scopes, "agent:pull")
	}
	return scopes
}

func isQueuedRuntimeConnectionMode(mode string) bool {
	return mode == "runtime_pull" || mode == "runtime_ws"
}

func (s *Service) ListRuntimeTokens(ctx context.Context, userID, agentID uuid.UUID) ([]RuntimeTokenResponse, error) {
	if _, err := s.ownerAgent(ctx, userID, agentID); err != nil {
		return nil, err
	}
	tokens, err := s.queries.ListAgentRuntimeTokensForOwner(ctx, db.ListAgentRuntimeTokensForOwnerParams{
		AgentID: agentID,
		UserID:  userID,
	})
	if err != nil {
		return nil, httpx.Internal("查询访问令牌失败")
	}
	items := make([]RuntimeTokenResponse, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, tokenResponse(token))
	}
	return items, nil
}

func (s *Service) GetRuntimeWorkbench(ctx context.Context, userID, agentID uuid.UUID) (*RuntimeWorkbenchResponse, error) {
	agent, err := s.ownerAgent(ctx, userID, agentID)
	if err != nil {
		return nil, err
	}
	tokens, err := s.ListRuntimeTokens(ctx, userID, agentID)
	if err != nil {
		return nil, err
	}
	pendingCount := int32(0)
	if agent.ConnectionMode == "runtime_pull" || agent.ConnectionMode == "runtime_ws" {
		count, countErr := s.queries.CountClaimableRuntimePullRuns(ctx, agentID)
		if countErr != nil {
			log.Warn().Err(countErr).Str("agent_id", agentID.String()).Msg("a2a.GetRuntimeWorkbench: CountClaimableRuntimePullRuns")
		} else {
			pendingCount = count
		}
	}
	runs, err := s.queries.ListRunsByCreatorAgentWithAgent(ctx, db.ListRunsByCreatorAgentWithAgentParams{
		CreatorID: userID,
		AgentID:   agentID,
		Limit:     10,
		Offset:    0,
	})
	if err != nil {
		log.Error().Err(err).Str("agent_id", agentID.String()).Msg("a2a.GetRuntimeWorkbench: ListRunsByCreatorAgentWithAgent")
		return nil, httpx.Internal("查询 Agent 运行记录失败")
	}

	recentRuns := make([]RuntimeWorkbenchRun, 0, len(runs))
	var lastClaimedAt *string
	var lastResultAt *string
	for _, run := range runs {
		item := RuntimeWorkbenchRun{
			RunID:        run.ID.String(),
			Status:       run.Status,
			Source:       run.Source,
			StartedAt:    run.StartedAt.UTC().Format(time.RFC3339),
			ErrorCode:    run.ErrorCode,
			ErrorMessage: run.ErrorMessage,
			DetailURL:    "/run/" + run.ID.String(),
		}
		if run.ClaimedAt != nil {
			value := run.ClaimedAt.UTC().Format(time.RFC3339)
			item.ClaimedAt = &value
			if lastClaimedAt == nil {
				lastClaimedAt = &value
			}
		}
		if run.FinishedAt != nil {
			value := run.FinishedAt.UTC().Format(time.RFC3339)
			item.FinishedAt = &value
			if lastResultAt == nil {
				lastResultAt = &value
			}
		}
		recentRuns = append(recentRuns, item)
	}

	var lastActivity *string
	for _, token := range tokens {
		if token.RevokedAt != nil || token.LastUsedAt == nil {
			continue
		}
		if lastActivity == nil || *token.LastUsedAt > *lastActivity {
			value := *token.LastUsedAt
			lastActivity = &value
		}
	}

	availability := runtimeWorkbenchAvailability(agent, tokens, recentRuns)
	resp := &RuntimeWorkbenchResponse{
		Agent: RuntimeWorkbenchAgent{
			ID:                  agent.ID.String(),
			Slug:                agent.Slug,
			Name:                agent.Name,
			ConnectionMode:      agent.ConnectionMode,
			LifecycleStatus:     agent.LifecycleStatus,
			Visibility:          agent.Visibility,
			CertificationStatus: agent.CertificationStatus,
			ReadinessCallable:   availability == "healthy",
			AvailabilityStatus:  availability,
		},
		Runtime: RuntimeWorkbenchRuntime{
			ActiveTokenCount:                 activeRuntimeTokenCount(tokens),
			PendingRunCount:                  pendingCount,
			ClaimNow:                         pendingCount > 0,
			LastRuntimeActivityAt:            lastActivity,
			LastClaimedAt:                    lastClaimedAt,
			LastResultAt:                     lastResultAt,
			RecommendedHeartbeatAfterSeconds: 60,
			MaxClaimWaitSeconds:              30,
		},
		Tokens:      tokens,
		RecentRuns:  recentRuns,
		Diagnostics: runtimeWorkbenchDiagnostics(agent, tokens, pendingCount, recentRuns, lastActivity),
	}
	return resp, nil
}

func (s *Service) RevokeRuntimeToken(ctx context.Context, userID, tokenID uuid.UUID) error {
	affected, err := s.queries.RevokeAgentRuntimeTokenForOwner(ctx, db.RevokeAgentRuntimeTokenForOwnerParams{
		ID: tokenID, UserID: userID,
	})
	if err != nil {
		return httpx.Internal("撤销访问令牌失败")
	}
	if affected == 0 {
		return httpx.NotFound("访问令牌不存在或已撤销")
	}
	return nil
}

func activeRuntimeTokenCount(tokens []RuntimeTokenResponse) int32 {
	var count int32
	for _, token := range tokens {
		if token.RevokedAt == nil {
			count++
		}
	}
	return count
}

func runtimeWorkbenchAvailability(agent db.Agent, tokens []RuntimeTokenResponse, runs []RuntimeWorkbenchRun) string {
	if agent.LifecycleStatus != "active" {
		return "disabled"
	}
	for _, run := range runs {
		if run.Status == "success" {
			return "healthy"
		}
	}
	for _, token := range tokens {
		if token.RevokedAt == nil && token.LastUsedAt != nil &&
			(!isQueuedRuntimeConnectionMode(agent.ConnectionMode) || hasScope(token.Scopes, "agent:pull")) {
			return "active"
		}
	}
	return "unknown"
}

func runtimeWorkbenchDiagnostics(
	agent db.Agent,
	tokens []RuntimeTokenResponse,
	pendingCount int32,
	runs []RuntimeWorkbenchRun,
	lastActivity *string,
) []RuntimeWorkbenchDiagnostic {
	diagnostics := []RuntimeWorkbenchDiagnostic{}
	if !isQueuedRuntimeConnectionMode(agent.ConnectionMode) {
		return append(diagnostics, RuntimeWorkbenchDiagnostic{
			Code:       "not_runtime_pull",
			Severity:   "info",
			Message:    "此 Agent 通过 Endpoint 或 MCP 接入；请用健康检查确认可用性。",
			NextAction: "run_health_check",
		})
	}
	if activeRuntimeTokenCount(tokens) == 0 {
		diagnostics = append(diagnostics, RuntimeWorkbenchDiagnostic{
			Code:       "no_agent_token",
			Severity:   "warning",
			Message:    "没有可用的 Agent Token。创建凭证后再启动 Agent Node。",
			NextAction: "create_agent_token",
		})
	}
	if activeRuntimeTokenCount(tokens) > 0 && !hasActiveRuntimePullToken(tokens) {
		diagnostics = append(diagnostics, RuntimeWorkbenchDiagnostic{
			Code:       "scope_missing",
			Severity:   "error",
			Message:    "现有 Agent Token 缺少 agent:pull 权限，无法建立连接或领取任务。",
			NextAction: "create_agent_token",
		})
	}
	if lastActivity == nil {
		diagnostics = append(diagnostics, RuntimeWorkbenchDiagnostic{
			Code:       "no_recent_runtime_activity",
			Severity:   "warning",
			Message:    "还没有收到 Agent Node 的连接或心跳。请确认进程已启动。",
			NextAction: "start_worker",
		})
	}
	if pendingCount > 0 {
		diagnostics = append(diagnostics, RuntimeWorkbenchDiagnostic{
			Code:       "pending_claimable_runs",
			Severity:   "warning",
			Message:    "有运行正在等待 Agent Node。请确认 WebSocket 已连接，或长轮询仍在工作。",
			NextAction: "check_claim_loop",
		})
	}
	for _, run := range runs {
		if run.ErrorCode == nil {
			continue
		}
		switch *run.ErrorCode {
		case "RUNTIME_PULL_NOT_CLAIMED":
			diagnostics = append(diagnostics, RuntimeWorkbenchDiagnostic{
				Code:       "pending_not_claimed",
				Severity:   "error",
				Message:    "最近有运行在等待领取时超时。请检查 Agent Node 连接。",
				NextAction: "start_worker",
			})
		case "RUNTIME_PULL_RESULT_TIMEOUT":
			diagnostics = append(diagnostics, RuntimeWorkbenchDiagnostic{
				Code:       "result_timeout",
				Severity:   "error",
				Message:    "最近有运行已被领取，但没有在时限内返回结果。请检查 Agent 后端和日志。",
				NextAction: "inspect_worker_result",
			})
		}
	}
	if len(diagnostics) == 0 {
		diagnostics = append(diagnostics, RuntimeWorkbenchDiagnostic{
			Code:       "runtime_ready",
			Severity:   "success",
			Message:    "Agent Node 连接和近期运行未发现异常。",
			NextAction: "keep_worker_supervised",
		})
	}
	return diagnostics
}

func hasActiveRuntimePullToken(tokens []RuntimeTokenResponse) bool {
	for _, token := range tokens {
		if token.RevokedAt == nil && hasScope(token.Scopes, "agent:pull") {
			return true
		}
	}
	return false
}

func (s *Service) GetCallPolicy(ctx context.Context, userID, agentID uuid.UUID) (*CallPolicyResponse, error) {
	if _, err := s.ownerAgent(ctx, userID, agentID); err != nil {
		return nil, err
	}
	policy, err := s.queries.GetAgentCallPolicy(ctx, agentID)
	if err != nil {
		return nil, httpx.Internal("查询 A2A 策略失败")
	}
	return &CallPolicyResponse{AgentID: agentID.String(), CallableBy: policy}, nil
}

func (s *Service) UpdateCallPolicy(ctx context.Context, userID, agentID uuid.UUID, req *UpdateCallPolicyRequest) (*CallPolicyResponse, error) {
	policy, err := s.queries.UpsertAgentCallPolicyForOwner(ctx, db.UpsertAgentCallPolicyForOwnerParams{
		AgentID: agentID, UserID: userID, CallableBy: req.CallableBy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("Agent 不存在")
	}
	if err != nil {
		return nil, httpx.Internal("更新 A2A 策略失败")
	}
	return &CallPolicyResponse{
		AgentID:    policy.AgentID.String(),
		CallableBy: policy.CallableBy,
		UpdatedAt:  policy.UpdatedAt.UTC().Format(time.RFC3339),
	}, nil
}

func (s *Service) ListChildren(ctx context.Context, userID, parentRunID uuid.UUID) ([]ChildRunResponse, error) {
	maxDepth := s.maxDelegationDepth
	if maxDepth <= 0 {
		maxDepth = maxDelegationDepth
	}
	return s.listChildrenTree(ctx, userID, parentRunID, maxDepth, map[uuid.UUID]struct{}{})
}

func (s *Service) listChildrenTree(ctx context.Context, userID, parentRunID uuid.UUID, remainingDepth int, seen map[uuid.UUID]struct{}) ([]ChildRunResponse, error) {
	if remainingDepth <= 0 {
		return []ChildRunResponse{}, nil
	}
	if _, ok := seen[parentRunID]; ok {
		return []ChildRunResponse{}, nil
	}
	seen[parentRunID] = struct{}{}
	rows, err := s.queries.ListChildRunsByParentAndUser(ctx, db.ListChildRunsByParentAndUserParams{
		ParentRunID: parentRunID, UserID: userID,
	})
	if err != nil {
		return nil, httpx.Internal("查询 Agent 协作运行失败")
	}
	items := make([]ChildRunResponse, 0, len(rows))
	for _, row := range rows {
		item := childRunResponseFromRow(row)
		children, err := s.listChildrenTree(ctx, userID, row.ChildRunID, remainingDepth-1, seen)
		if err != nil {
			return nil, err
		}
		if len(children) > 0 {
			item.Children = children
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) ListParentRuns(ctx context.Context, userID uuid.UUID, page, size int32, search string) (*ParentRunListResponse, error) {
	if page < 1 {
		page = defaultParentPage
	}
	if size < 1 {
		size = defaultParentPageSize
	}
	if size > maxParentPageSize {
		size = maxParentPageSize
	}
	search = normalizeParentSearch(search)
	rows, err := s.queries.ListParentRunsWithDelegationsByUser(ctx, db.ListParentRunsWithDelegationsByUserParams{
		UserID: userID,
		Search: search,
		Limit:  size,
		Offset: (page - 1) * size,
	})
	if err != nil {
		log.Error().Err(err).Str("user_id", userID.String()).Msg("a2a.ListParentRuns: list")
		return nil, httpx.Internal("查询 Parent 调用链失败")
	}
	total, err := s.queries.CountParentRunsWithDelegationsByUser(ctx, db.CountParentRunsWithDelegationsByUserParams{
		UserID: userID,
		Search: search,
	})
	if err != nil {
		log.Error().Err(err).Str("user_id", userID.String()).Msg("a2a.ListParentRuns: count")
		return nil, httpx.Internal("查询 Parent 调用链失败")
	}
	items := make([]ParentRunSummary, 0, len(rows))
	for _, row := range rows {
		item := ParentRunSummary{
			ParentRunID: row.ParentRunID.String(), CallerAgentID: row.CallerAgentID.String(),
			CallerAgentSlug: row.CallerAgentSlug, CallerAgentName: row.CallerAgentName,
			CallerAgentTags: row.CallerAgentTags, CallerSkills: skillRefs(row.CallerSkillIDs, row.CallerSkillNames),
			Source: row.ParentSource, ActiveAgentTokenCount: row.ActiveRuntimeTokenCount,
			Status: row.Status, DurationMs: row.DurationMs, StartedAt: row.StartedAt.UTC().Format(time.RFC3339),
			ChildCount: row.ChildCount, SuccessfulChildCount: row.SuccessfulChildCount,
			RunningChildCount: row.RunningChildCount,
			A2AContext:        contextRefFromParentRow(row),
		}
		if row.FinishedAt != nil {
			formatted := row.FinishedAt.UTC().Format(time.RFC3339)
			item.FinishedAt = &formatted
		}
		if row.LastRuntimeTokenUsedAt != nil {
			formatted := row.LastRuntimeTokenUsedAt.UTC().Format(time.RFC3339)
			item.LastAgentTokenUsedAt = &formatted
		}
		items = append(items, item)
	}
	return &ParentRunListResponse{Items: items, Total: total, Page: page, Size: size}, nil
}

func normalizeParentSearch(search string) string {
	search = strings.TrimSpace(search)
	runes := []rune(search)
	if len(runes) > maxParentSearchLen {
		return string(runes[:maxParentSearchLen])
	}
	return search
}

func skillRefs(ids, names []string) []SkillRef {
	if len(ids) == 0 {
		return []SkillRef{}
	}
	items := make([]SkillRef, 0, len(ids))
	for i, id := range ids {
		name := id
		if i < len(names) && strings.TrimSpace(names[i]) != "" {
			name = names[i]
		}
		items = append(items, SkillRef{ID: id, Name: name})
	}
	return items
}

func childRunResponseFromRow(row db.ListChildRunsByParentAndUserRow) ChildRunResponse {
	item := ChildRunResponse{
		ChildRunID: row.ChildRunID.String(), ParentRunID: row.ParentRunID.String(),
		CallerAgentID: row.CallerAgentID.String(), TargetAgentID: row.TargetAgentID.String(),
		CallerAgentSlug: row.CallerAgentSlug, CallerAgentName: row.CallerAgentName,
		CallerAgentTags: row.CallerAgentTags, CallerSkills: skillRefs(row.CallerSkillIDs, row.CallerSkillNames),
		TargetAgentSlug: row.TargetAgentSlug, TargetAgentName: row.TargetAgentName,
		TargetAgentTags: row.TargetAgentTags, TargetSkills: skillRefs(row.TargetSkillIDs, row.TargetSkillNames),
		Reason: row.Reason, Status: row.Status, CostCents: row.CostCents,
		DurationMs: row.DurationMs, StartedAt: row.StartedAt.UTC().Format(time.RFC3339),
		Source: row.Source, BillingMode: "free_delegation",
		A2AContext: contextRefFromChildRow(row),
	}
	if row.FinishedAt != nil {
		formatted := row.FinishedAt.UTC().Format(time.RFC3339)
		item.FinishedAt = &formatted
	}
	return item
}

func contextRefFromChildRow(row db.ListChildRunsByParentAndUserRow) *A2AContextRef {
	if row.ProtocolContextID == "" && row.RootContextID == "" && row.TraceID == "" {
		return nil
	}
	return &A2AContextRef{
		ProtocolContextID: row.ProtocolContextID,
		ProtocolTaskID:    row.ProtocolTaskID,
		RootContextID:     row.RootContextID,
		ParentContextID:   row.ParentContextID,
		ParentTaskID:      row.ParentTaskID,
		TraceID:           row.TraceID,
		ReferenceTaskIDs:  row.ReferenceTaskIDs,
		Source:            row.ContextSource,
	}
}

func contextRefFromParentRow(row db.ListParentRunsWithDelegationsByUserRow) *A2AContextRef {
	if row.ProtocolContextID == "" && row.RootContextID == "" && row.TraceID == "" {
		return nil
	}
	return &A2AContextRef{
		ProtocolContextID: row.ProtocolContextID,
		ProtocolTaskID:    row.ProtocolTaskID,
		RootContextID:     row.RootContextID,
		TraceID:           row.TraceID,
	}
}

func (s *Service) ownerAgent(ctx context.Context, userID, agentID uuid.UUID) (db.Agent, error) {
	agent, err := s.queries.GetAgentByIDForOwner(ctx, db.GetAgentByIDForOwnerParams{ID: agentID, CreatorID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Agent{}, httpx.NotFound("Agent 不存在")
	}
	if err != nil {
		return db.Agent{}, httpx.Internal("查询 Agent 失败")
	}
	return agent, nil
}

func hasScope(scopes []string, expected string) bool {
	for _, scope := range scopes {
		if scope == expected {
			return true
		}
	}
	return false
}

func tokenResponse(token db.AgentRuntimeToken) RuntimeTokenResponse {
	resp := RuntimeTokenResponse{
		ID: token.ID.String(), AgentID: token.AgentID.String(), Name: token.Name,
		Prefix: token.Prefix, Scopes: token.Scopes,
		CreatedAt: token.CreatedAt.UTC().Format(time.RFC3339),
	}
	if token.LastUsedAt != nil {
		value := token.LastUsedAt.UTC().Format(time.RFC3339)
		resp.LastUsedAt = &value
	}
	if token.RevokedAt != nil {
		value := token.RevokedAt.UTC().Format(time.RFC3339)
		resp.RevokedAt = &value
	}
	return resp
}
