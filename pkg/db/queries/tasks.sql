-- tasks.sql
--
-- 子轮 2.4 任务驱动 A 形态。task_queries 表保存"用户自然语言 → 解析 skill →
-- 推荐 Agent → 用户最终选择"的全过程，便于离线分析推荐质量。

-- name: CreateTaskQuery :one
-- 写入一条任务查询：原始 query + Skill/MCP 引用 + 推荐 agent_id 顺序。
INSERT INTO task_queries (user_id, query, parsed_skills, mcp_tools, recommended_agent_ids)
VALUES ($1, $2, $3, $4, $5)
	RETURNING id, user_id, query, parsed_skills, mcp_tools, recommended_agent_ids,
	          chosen_agent_id, chosen_at,
	          completed_at, completion_summary, completion_run_id,
	          created_at;

-- name: GetTaskQuery :one
-- 按 id 查单条；调用方需自行校验 user_id 归属。
	SELECT id, user_id, query, parsed_skills, mcp_tools, recommended_agent_ids,
	       chosen_agent_id, chosen_at,
	       completed_at, completion_summary, completion_run_id,
	       created_at
	FROM task_queries
	WHERE id = $1;

-- name: MarkTaskQueryChosen :one
-- 用户选定推荐里某个 agent：写入 chosen_agent_id + chosen_at。
-- 限定 user_id 防越权；命中 0 行表示不存在 / 不属于该 user。
UPDATE task_queries
SET chosen_agent_id = $3,
    chosen_at = NOW()
WHERE id = $1 AND user_id = $2
	RETURNING id, user_id, query, parsed_skills, mcp_tools, recommended_agent_ids,
	          chosen_agent_id, chosen_at,
	          completed_at, completion_summary, completion_run_id,
	          created_at;

-- name: ListTaskQueriesByUser :many
-- "我的私有任务"：按搜索、状态、分页返回。
	SELECT id, user_id, query, parsed_skills, mcp_tools, recommended_agent_ids,
	       chosen_agent_id, chosen_at,
	       completed_at, completion_summary, completion_run_id,
	       created_at
	FROM task_queries
WHERE user_id = $1
  AND (
      $2::text = ''
      OR id::text ILIKE '%' || $2 || '%'
      OR query ILIKE '%' || $2 || '%'
	      OR COALESCE(completion_summary, '') ILIKE '%' || $2 || '%'
	      OR COALESCE(completion_run_id::text, '') ILIKE '%' || $2 || '%'
      OR array_to_string(COALESCE(parsed_skills, ARRAY[]::text[]), ' ') ILIKE '%' || $2 || '%'
      OR array_to_string(COALESCE(mcp_tools, ARRAY[]::text[]), ' ') ILIKE '%' || $2 || '%'
      OR COALESCE(recommended_agent_ids::text, '') ILIKE '%' || $2 || '%'
  )
  AND (
	      $3::text = ''
	      OR ($3 = 'completed' AND completed_at IS NOT NULL)
	      OR ($3 = 'matched' AND completed_at IS NULL AND chosen_agent_id IS NOT NULL)
	      OR ($3 = 'needs_agent' AND completed_at IS NULL AND chosen_agent_id IS NULL AND cardinality(recommended_agent_ids) = 0)
	      OR ($3 = 'open' AND completed_at IS NULL AND chosen_agent_id IS NULL AND cardinality(recommended_agent_ids) > 0)
  )
ORDER BY
  CASE WHEN $4 = 'created_asc' THEN created_at END ASC,
  CASE WHEN $4 = 'created_desc' THEN created_at END DESC,
  created_at DESC,
  id DESC
LIMIT $5 OFFSET $6;

-- name: CountTaskQueriesByUser :one
SELECT COUNT(*)::int
FROM task_queries
WHERE user_id = $1
  AND (
      $2::text = ''
      OR id::text ILIKE '%' || $2 || '%'
      OR query ILIKE '%' || $2 || '%'
	      OR COALESCE(completion_summary, '') ILIKE '%' || $2 || '%'
	      OR COALESCE(completion_run_id::text, '') ILIKE '%' || $2 || '%'
      OR array_to_string(COALESCE(parsed_skills, ARRAY[]::text[]), ' ') ILIKE '%' || $2 || '%'
      OR array_to_string(COALESCE(mcp_tools, ARRAY[]::text[]), ' ') ILIKE '%' || $2 || '%'
      OR COALESCE(recommended_agent_ids::text, '') ILIKE '%' || $2 || '%'
  )
  AND (
	      $3::text = ''
	      OR ($3 = 'completed' AND completed_at IS NOT NULL)
	      OR ($3 = 'matched' AND completed_at IS NULL AND chosen_agent_id IS NOT NULL)
	      OR ($3 = 'needs_agent' AND completed_at IS NULL AND chosen_agent_id IS NULL AND cardinality(recommended_agent_ids) = 0)
	      OR ($3 = 'open' AND completed_at IS NULL AND chosen_agent_id IS NULL AND cardinality(recommended_agent_ids) > 0)
  );

-- name: GetAgentsByIDs :many
-- 任务推荐回填：按一组 agent_id 批量取详情（含 creator 显示名）。
-- 只回当前仍公开运行的 Agent；已下架 / private / unlisted 的历史推荐不再展示。
SELECT a.id, a.creator_id, a.slug, a.name, a.description, a.endpoint_url,
       a.endpoint_auth_header, a.price_per_call_cents, a.tags,
       a.lifecycle_status, a.visibility, a.certification_status,
       a.rejection_reason, a.certified_at, a.total_calls, a.total_revenue_cents,
       a.created_at, a.updated_at, u.display_name AS creator_name
FROM agents a
JOIN users u ON u.id = a.creator_id
WHERE a.id = ANY($1::uuid[])
  AND a.visibility = 'public'
  AND a.lifecycle_status = 'active';

-- name: ListAdminTasks :many
-- 管理台私有任务列表：可搜索任务内容、所有者和已选 Agent，并按私有任务状态筛选。
SELECT t.id, t.user_id, t.query, t.parsed_skills, t.mcp_tools, t.recommended_agent_ids,
       t.chosen_agent_id, t.chosen_at,
       t.completed_at, t.completion_summary, t.completion_run_id,
       t.created_at,
       u.email AS user_email,
       u.display_name AS user_display_name,
       chosen.slug AS chosen_agent_slug,
       chosen.name AS chosen_agent_name
FROM task_queries t
JOIN users u ON u.id = t.user_id
LEFT JOIN agents chosen ON chosen.id = t.chosen_agent_id
WHERE (
    $1::text = ''
    OR t.query ILIKE '%' || $1 || '%'
    OR COALESCE(t.completion_summary, '') ILIKE '%' || $1 || '%'
    OR u.email ILIKE '%' || $1 || '%'
    OR u.display_name ILIKE '%' || $1 || '%'
    OR chosen.slug ILIKE '%' || $1 || '%'
    OR chosen.name ILIKE '%' || $1 || '%'
  )
  AND (
    $2::text = ''
    OR ($2 = 'completed' AND t.completed_at IS NOT NULL)
    OR ($2 = 'matched' AND t.completed_at IS NULL AND t.chosen_agent_id IS NOT NULL)
    OR ($2 = 'needs_agent' AND t.completed_at IS NULL AND t.chosen_agent_id IS NULL AND cardinality(t.recommended_agent_ids) = 0)
    OR ($2 = 'open' AND t.completed_at IS NULL AND t.chosen_agent_id IS NULL AND cardinality(t.recommended_agent_ids) > 0)
  )
ORDER BY t.created_at DESC
LIMIT $3 OFFSET $4;

-- name: CountAdminTasks :one
SELECT COUNT(*)::int AS total
FROM task_queries t
JOIN users u ON u.id = t.user_id
LEFT JOIN agents chosen ON chosen.id = t.chosen_agent_id
WHERE (
    $1::text = ''
    OR t.query ILIKE '%' || $1 || '%'
    OR COALESCE(t.completion_summary, '') ILIKE '%' || $1 || '%'
    OR u.email ILIKE '%' || $1 || '%'
    OR u.display_name ILIKE '%' || $1 || '%'
    OR chosen.slug ILIKE '%' || $1 || '%'
    OR chosen.name ILIKE '%' || $1 || '%'
  )
  AND (
    $2::text = ''
    OR ($2 = 'completed' AND t.completed_at IS NOT NULL)
    OR ($2 = 'matched' AND t.completed_at IS NULL AND t.chosen_agent_id IS NOT NULL)
    OR ($2 = 'needs_agent' AND t.completed_at IS NULL AND t.chosen_agent_id IS NULL AND cardinality(t.recommended_agent_ids) = 0)
    OR ($2 = 'open' AND t.completed_at IS NULL AND t.chosen_agent_id IS NULL AND cardinality(t.recommended_agent_ids) > 0)
  );
