package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

const listCallRecordsForUser = `-- name: ListCallRecordsForUser :many
SELECT r.id,
       r.user_id,
       r.agent_id,
       r.status,
       CASE WHEN r.user_id = $1 THEN r.cost_cents ELSE 0 END::int AS cost_cents,
       CASE WHEN a.creator_id = $1 THEN r.creator_revenue_cents ELSE 0 END::int AS creator_revenue_cents,
       r.duration_ms,
       r.started_at,
       r.finished_at,
       r.source,
       a.slug AS agent_slug,
       a.name AS agent_name,
       CASE
           WHEN r.user_id = $1 AND a.creator_id = $1 THEN 'both'
           WHEN r.user_id = $1 THEN 'made'
           ELSE 'received'
       END::text AS direction,
       COALESCE(d.parent_run_id::text, '')::text AS parent_run_id,
       COALESCE(d.caller_agent_id::text, '')::text AS caller_agent_id,
       COALESCE(caller.slug, '')::text AS caller_agent_slug,
       COALESCE(caller.name, '')::text AS caller_agent_name,
       COALESCE(ctx.protocol_context_id, '')::text AS protocol_context_id,
       COALESCE(ctx.protocol_task_id, '')::text AS protocol_task_id,
       COALESCE(ctx.root_context_id, '')::text AS root_context_id,
       COALESCE(ctx.parent_context_id, '')::text AS parent_context_id,
       COALESCE(ctx.parent_task_id, '')::text AS parent_task_id,
       COALESCE(ctx.trace_id, '')::text AS trace_id,
       COALESCE(ctx.reference_task_ids, ARRAY[]::text[]) AS reference_task_ids,
       COALESCE(ctx.source, '')::text AS context_source,
       COALESCE(NULLIF(ctx.protocol_task_id, ''), r.id::text)::text AS call_id,
       COALESCE(children.child_count, 0)::int AS child_count
FROM runs r
JOIN agents a ON a.id = r.agent_id
LEFT JOIN run_delegations d ON d.child_run_id = r.id
LEFT JOIN agents caller ON caller.id = d.caller_agent_id
LEFT JOIN a2a_context_mappings ctx ON ctx.run_id = r.id
LEFT JOIN LATERAL (
    SELECT COUNT(*)::int AS child_count
    FROM run_delegations cd
    WHERE cd.parent_run_id = r.id
) children ON TRUE
WHERE (
    ($2 = 'made' AND r.user_id = $1)
    OR ($2 = 'received' AND a.creator_id = $1)
    OR ($2 = 'all' AND (r.user_id = $1 OR a.creator_id = $1))
)
ORDER BY r.started_at DESC, r.id DESC
LIMIT $3 OFFSET $4`

type ListCallRecordsForUserParams struct {
	UserID uuid.UUID `db:"user_id" json:"user_id"`
	View   string    `db:"view" json:"view"`
	Limit  int32     `db:"limit" json:"limit"`
	Offset int32     `db:"offset" json:"offset"`
}

type ListCallRecordsForUserRow struct {
	ID                  uuid.UUID  `db:"id" json:"id"`
	UserID              uuid.UUID  `db:"user_id" json:"user_id"`
	AgentID             uuid.UUID  `db:"agent_id" json:"agent_id"`
	Status              string     `db:"status" json:"status"`
	CostCents           int32      `db:"cost_cents" json:"cost_cents"`
	CreatorRevenueCents int32      `db:"creator_revenue_cents" json:"creator_revenue_cents"`
	DurationMs          *int32     `db:"duration_ms" json:"duration_ms"`
	StartedAt           time.Time  `db:"started_at" json:"started_at"`
	FinishedAt          *time.Time `db:"finished_at" json:"finished_at"`
	Source              string     `db:"source" json:"source"`
	AgentSlug           string     `db:"agent_slug" json:"agent_slug"`
	AgentName           string     `db:"agent_name" json:"agent_name"`
	Direction           string     `db:"direction" json:"direction"`
	ParentRunID         string     `db:"parent_run_id" json:"parent_run_id"`
	CallerAgentID       string     `db:"caller_agent_id" json:"caller_agent_id"`
	CallerAgentSlug     string     `db:"caller_agent_slug" json:"caller_agent_slug"`
	CallerAgentName     string     `db:"caller_agent_name" json:"caller_agent_name"`
	ProtocolContextID   string     `db:"protocol_context_id" json:"protocol_context_id"`
	ProtocolTaskID      string     `db:"protocol_task_id" json:"protocol_task_id"`
	RootContextID       string     `db:"root_context_id" json:"root_context_id"`
	ParentContextID     string     `db:"parent_context_id" json:"parent_context_id"`
	ParentTaskID        string     `db:"parent_task_id" json:"parent_task_id"`
	TraceID             string     `db:"trace_id" json:"trace_id"`
	ReferenceTaskIDs    []string   `db:"reference_task_ids" json:"reference_task_ids"`
	ContextSource       string     `db:"context_source" json:"context_source"`
	CallID              string     `db:"call_id" json:"call_id"`
	ChildCount          int32      `db:"child_count" json:"child_count"`
}

func (q *Queries) ListCallRecordsForUser(ctx context.Context, arg ListCallRecordsForUserParams) ([]ListCallRecordsForUserRow, error) {
	rows, err := q.db.Query(ctx, listCallRecordsForUser, arg.UserID, arg.View, arg.Limit, arg.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ListCallRecordsForUserRow{}
	for rows.Next() {
		var item ListCallRecordsForUserRow
		if err := rows.Scan(
			&item.ID,
			&item.UserID,
			&item.AgentID,
			&item.Status,
			&item.CostCents,
			&item.CreatorRevenueCents,
			&item.DurationMs,
			&item.StartedAt,
			&item.FinishedAt,
			&item.Source,
			&item.AgentSlug,
			&item.AgentName,
			&item.Direction,
			&item.ParentRunID,
			&item.CallerAgentID,
			&item.CallerAgentSlug,
			&item.CallerAgentName,
			&item.ProtocolContextID,
			&item.ProtocolTaskID,
			&item.RootContextID,
			&item.ParentContextID,
			&item.ParentTaskID,
			&item.TraceID,
			&item.ReferenceTaskIDs,
			&item.ContextSource,
			&item.CallID,
			&item.ChildCount,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

const countCallRecordsForUser = `-- name: CountCallRecordsForUser :one
SELECT COUNT(*)::int AS total
FROM runs r
JOIN agents a ON a.id = r.agent_id
WHERE (
    ($2 = 'made' AND r.user_id = $1)
    OR ($2 = 'received' AND a.creator_id = $1)
    OR ($2 = 'all' AND (r.user_id = $1 OR a.creator_id = $1))
)`

type CountCallRecordsForUserParams struct {
	UserID uuid.UUID `db:"user_id" json:"user_id"`
	View   string    `db:"view" json:"view"`
}

func (q *Queries) CountCallRecordsForUser(ctx context.Context, arg CountCallRecordsForUserParams) (int32, error) {
	row := q.db.QueryRow(ctx, countCallRecordsForUser, arg.UserID, arg.View)
	var total int32
	err := row.Scan(&total)
	return total, err
}
