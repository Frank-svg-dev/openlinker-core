-- Persistent authorization evidence for moving an immutable Attempt identity
-- from its original Session to a replacement Session. The Attempt itself is
-- never rewritten; Event/Result/continue callers present and lock this grant.
-- Principal locks from runtime_leases.sql must be held before grant locks.

-- name: CreateRuntimeResumeGrant :one
WITH database_clock AS MATERIALIZED (
    SELECT clock_timestamp() AS database_now
)
INSERT INTO runtime_resume_grants (
    id, run_id, attempt_id, lease_id, fencing_token, agent_id, node_id,
    worker_id, source_session_id, source_credential_id, target_session_id,
    target_credential_id, permission, granted_by_core_instance_id,
    granted_at, expires_at
)
SELECT
    sqlc.arg(grant_id), a.run_id, a.id, a.lease_id, a.fencing_token,
    a.agent_id, a.node_id, a.runtime_worker_id, a.runtime_session_id,
    a.runtime_token_id, target.runtime_session_id, target.credential_id,
    sqlc.arg(permission), sqlc.arg(core_instance_id), c.database_now,
    CASE sqlc.arg(permission)
        WHEN 'continue_execution' THEN LEAST(
            c.database_now + (sqlc.arg(grant_ttl_ms)::bigint * INTERVAL '1 millisecond'),
            a.lease_expires_at,
            a.attempt_deadline_at,
            r.run_deadline_at
        )
        ELSE c.database_now
            + (sqlc.arg(grant_ttl_ms)::bigint * INTERVAL '1 millisecond')
    END
FROM run_attempts a
JOIN runs r
  ON r.id = a.run_id
 AND r.agent_id = a.agent_id
JOIN runtime_sessions source
  ON source.runtime_session_id = a.runtime_session_id
 AND source.node_id = a.node_id
 AND source.agent_id = a.agent_id
 AND source.credential_id = a.runtime_token_id
 AND source.worker_id = a.runtime_worker_id
JOIN runtime_sessions target
  ON target.runtime_session_id = sqlc.arg(target_session_id)
 AND target.node_id = a.node_id
 AND target.agent_id = a.agent_id
 AND target.worker_id = a.runtime_worker_id
JOIN runtime_nodes n ON n.node_id = target.node_id
JOIN agent_tokens source_token
  ON source_token.id = source.credential_id
 AND source_token.agent_id = source.agent_id
JOIN agent_tokens target_token
  ON target_token.id = target.credential_id
 AND target_token.agent_id = target.agent_id
CROSS JOIN database_clock c
WHERE a.run_id = sqlc.arg(run_id)
  AND a.id = sqlc.arg(attempt_id)
  AND a.lease_id = sqlc.arg(lease_id)
  AND a.fencing_token = sqlc.arg(fencing_token)
  AND a.executor_type = 'agent_node'
  AND a.accepted_at IS NOT NULL
  AND r.runtime_contract_id = 'openlinker.runtime.v2'
  AND source.runtime_session_id <> target.runtime_session_id
  AND source.status IN ('offline', 'closed')
  AND target.status IN ('active', 'draining')
  AND target.attached_core_instance_id = sqlc.arg(core_instance_id)
  AND n.status IN ('active', 'draining')
  AND n.revoked_at IS NULL
  AND target_token.status = 'active_runtime'
  AND target_token.revoked_at IS NULL
  AND (target_token.expires_at IS NULL OR target_token.expires_at > c.database_now)
  AND target_token.scopes @> ARRAY['agent:pull']::text[]
  AND (
      target_token.id = source_token.id
      OR (
          target_token.rotation_predecessor_id = source_token.id
          AND source_token.status = 'revoked'
          AND source_token.revocation_kind = 'planned_rotation'
      )
  )
  AND sqlc.arg(permission) IN ('upload_spool_only', 'continue_execution')
  AND (
      sqlc.arg(permission) = 'upload_spool_only'
      OR (
          sqlc.arg(permission) = 'continue_execution'
          AND a.finished_at IS NULL
          AND a.lease_expires_at > c.database_now
          AND a.attempt_deadline_at > c.database_now
          AND r.status = 'running'
          AND r.dispatch_state = 'executing'
          AND r.active_attempt_id = a.id
          AND r.lease_id = a.lease_id
          AND r.fencing_token = a.fencing_token
          AND r.run_deadline_at > c.database_now
      )
  )
  AND sqlc.arg(grant_ttl_ms)::bigint BETWEEN 1 AND 86400000
  AND NOT EXISTS (
      SELECT 1
      FROM runtime_resume_grants active_grant
      WHERE active_grant.attempt_id = a.id
        AND active_grant.revoked_at IS NULL
  )
  AND NOT EXISTS (
      SELECT 1
      FROM runtime_resume_grants denied_grant
      WHERE denied_grant.attempt_id = a.id
        AND denied_grant.revoked_at IS NOT NULL
        AND NOT (
            denied_grant.revoke_reason = 'expired'
            AND denied_grant.revoked_by_type = 'system'
            AND denied_grant.expires_at <= denied_grant.revoked_at
        )
  )
RETURNING *;

-- name: GetActiveRuntimeResumeGrant :one
SELECT *
FROM runtime_resume_grants
WHERE id = sqlc.arg(grant_id)
  AND run_id = sqlc.arg(run_id)
  AND attempt_id = sqlc.arg(attempt_id)
  AND lease_id = sqlc.arg(lease_id)
  AND fencing_token = sqlc.arg(fencing_token)
  AND target_session_id = sqlc.arg(target_session_id)
  AND target_credential_id = sqlc.arg(target_credential_id)
  AND revoked_at IS NULL
  AND expires_at > clock_timestamp();

-- name: LockActiveRuntimeResumeGrant :one
SELECT g.id, g.run_id, g.attempt_id, g.lease_id, g.fencing_token,
       g.agent_id, g.node_id, g.worker_id, g.source_session_id,
       g.source_credential_id, g.target_session_id, g.target_credential_id,
       g.permission, g.granted_by_core_instance_id, g.granted_at,
       g.expires_at, g.first_used_at, g.revoked_at, g.revoked_by_type,
       g.revoked_by_id, g.revoke_reason,
       clock_timestamp() AS database_now
FROM runtime_resume_grants g
WHERE g.id = sqlc.arg(grant_id)
  AND g.run_id = sqlc.arg(run_id)
  AND g.attempt_id = sqlc.arg(attempt_id)
  AND g.lease_id = sqlc.arg(lease_id)
  AND g.fencing_token = sqlc.arg(fencing_token)
  AND g.agent_id = sqlc.arg(agent_id)
  AND g.node_id = sqlc.arg(node_id)
  AND g.worker_id = sqlc.arg(worker_id)
  AND g.target_session_id = sqlc.arg(target_session_id)
  AND g.target_credential_id = sqlc.arg(target_credential_id)
  AND g.permission = sqlc.arg(permission)
  AND g.revoked_at IS NULL
  AND g.expires_at > clock_timestamp()
  AND (
      g.permission = 'upload_spool_only'
      OR EXISTS (
          SELECT 1
          FROM runs active_run
          JOIN run_attempts active_attempt
            ON active_attempt.run_id = active_run.id
           AND active_attempt.id = active_run.active_attempt_id
          WHERE active_run.id = g.run_id
            AND active_attempt.id = g.attempt_id
            AND active_attempt.lease_id = g.lease_id
            AND active_attempt.fencing_token = g.fencing_token
            AND active_run.status = 'running'
            AND active_run.dispatch_state = 'executing'
            AND active_run.lease_expires_at > clock_timestamp()
            AND active_run.run_deadline_at > clock_timestamp()
            AND active_attempt.finished_at IS NULL
            AND active_attempt.lease_expires_at > clock_timestamp()
            AND active_attempt.attempt_deadline_at > clock_timestamp()
      )
  )
  AND EXISTS (
      SELECT 1
      FROM runtime_sessions target
      JOIN runtime_nodes n ON n.node_id = target.node_id
      JOIN agent_tokens t
        ON t.id = target.credential_id
       AND t.agent_id = target.agent_id
      WHERE target.runtime_session_id = g.target_session_id
        AND target.node_id = g.node_id
        AND target.agent_id = g.agent_id
        AND target.credential_id = g.target_credential_id
        AND target.worker_id = g.worker_id
        AND target.attached_core_instance_id = sqlc.arg(core_instance_id)
        AND target.status IN ('active', 'draining')
        AND n.status IN ('active', 'draining')
        AND n.revoked_at IS NULL
        AND t.status = 'active_runtime'
        AND t.revoked_at IS NULL
        AND (t.expires_at IS NULL OR t.expires_at > clock_timestamp())
  )
FOR UPDATE OF g;

-- name: LockActiveRuntimeResumeGrantForAttemptTarget :one
-- Event/Result payloads intentionally carry only immutable Attempt identity.
-- The authenticated replacement Session locates the single unrevoked grant
-- with that identity; continue_execution is a superset of spool upload.
SELECT g.id, g.run_id, g.attempt_id, g.lease_id, g.fencing_token,
       g.agent_id, g.node_id, g.worker_id, g.source_session_id,
       g.source_credential_id, g.target_session_id, g.target_credential_id,
       g.permission, g.granted_by_core_instance_id, g.granted_at,
       g.expires_at, g.first_used_at, g.revoked_at, g.revoked_by_type,
       g.revoked_by_id, g.revoke_reason,
       clock_timestamp() AS database_now
FROM runtime_resume_grants g
WHERE g.run_id = sqlc.arg(run_id)
  AND g.attempt_id = sqlc.arg(attempt_id)
  AND g.lease_id = sqlc.arg(lease_id)
  AND g.fencing_token = sqlc.arg(fencing_token)
  AND g.agent_id = sqlc.arg(agent_id)
  AND g.node_id = sqlc.arg(node_id)
  AND g.worker_id = sqlc.arg(worker_id)
  AND g.target_session_id = sqlc.arg(target_session_id)
  AND g.target_credential_id = sqlc.arg(target_credential_id)
  AND (
      g.permission = sqlc.arg(allowed_permission)
      OR (
          sqlc.arg(allowed_permission) = 'upload_spool_only'
          AND g.permission = 'continue_execution'
      )
  )
  AND g.revoked_at IS NULL
  AND g.expires_at > clock_timestamp()
  AND (
      g.permission = 'upload_spool_only'
      OR EXISTS (
          SELECT 1
          FROM runs active_run
          JOIN run_attempts active_attempt
            ON active_attempt.run_id = active_run.id
           AND active_attempt.id = active_run.active_attempt_id
          WHERE active_run.id = g.run_id
            AND active_attempt.id = g.attempt_id
            AND active_attempt.lease_id = g.lease_id
            AND active_attempt.fencing_token = g.fencing_token
            AND active_run.status = 'running'
            AND active_run.dispatch_state = 'executing'
            AND active_run.lease_expires_at > clock_timestamp()
            AND active_run.run_deadline_at > clock_timestamp()
            AND active_attempt.finished_at IS NULL
            AND active_attempt.lease_expires_at > clock_timestamp()
            AND active_attempt.attempt_deadline_at > clock_timestamp()
      )
  )
  AND EXISTS (
      SELECT 1
      FROM runtime_sessions target
      JOIN runtime_nodes n ON n.node_id = target.node_id
      JOIN agent_tokens t
        ON t.id = target.credential_id
       AND t.agent_id = target.agent_id
      WHERE target.runtime_session_id = g.target_session_id
        AND target.node_id = g.node_id
        AND target.agent_id = g.agent_id
        AND target.credential_id = g.target_credential_id
        AND target.worker_id = g.worker_id
        AND target.attached_core_instance_id = sqlc.arg(core_instance_id)
        AND target.status IN ('active', 'draining')
        AND n.status IN ('active', 'draining')
        AND n.revoked_at IS NULL
        AND t.status = 'active_runtime'
        AND t.revoked_at IS NULL
        AND (t.expires_at IS NULL OR t.expires_at > clock_timestamp())
  )
FOR UPDATE OF g;

-- name: LockConsumedRuntimeResumeGrantForStoredReplay :one
-- Once an active grant has authorized and persisted an Event/Result, the same
-- target principal may recover the stored ACK after natural expiry. Explicit
-- security/operator revocation never qualifies for this replay-only path.
SELECT g.id, g.run_id, g.attempt_id, g.lease_id, g.fencing_token,
       g.agent_id, g.node_id, g.worker_id, g.source_session_id,
       g.source_credential_id, g.target_session_id, g.target_credential_id,
       g.permission, g.granted_by_core_instance_id, g.granted_at,
       g.expires_at, g.first_used_at, g.revoked_at, g.revoked_by_type,
       g.revoked_by_id, g.revoke_reason,
       clock_timestamp() AS database_now
FROM runtime_resume_grants g
WHERE g.run_id = sqlc.arg(run_id)
  AND g.attempt_id = sqlc.arg(attempt_id)
  AND g.lease_id = sqlc.arg(lease_id)
  AND g.fencing_token = sqlc.arg(fencing_token)
  AND g.agent_id = sqlc.arg(agent_id)
  AND g.node_id = sqlc.arg(node_id)
  AND g.worker_id = sqlc.arg(worker_id)
  AND g.target_session_id = sqlc.arg(target_session_id)
  AND g.target_credential_id = sqlc.arg(target_credential_id)
  AND g.first_used_at IS NOT NULL
  AND (
      g.revoked_at IS NULL
      OR (
          g.revoke_reason = 'expired'
          AND g.revoked_by_type = 'system'
          AND g.expires_at <= g.revoked_at
      )
  )
FOR SHARE OF g;

-- name: ConsumeRuntimeResumeGrant :one
UPDATE runtime_resume_grants
SET first_used_at = COALESCE(first_used_at, clock_timestamp())
WHERE id = sqlc.arg(grant_id)
  AND run_id = sqlc.arg(run_id)
  AND attempt_id = sqlc.arg(attempt_id)
  AND lease_id = sqlc.arg(lease_id)
  AND fencing_token = sqlc.arg(fencing_token)
  AND target_session_id = sqlc.arg(target_session_id)
  AND target_credential_id = sqlc.arg(target_credential_id)
  AND permission = sqlc.arg(permission)
  AND revoked_at IS NULL
  AND expires_at > clock_timestamp()
RETURNING *;

-- name: RevokeRuntimeResumeGrant :one
UPDATE runtime_resume_grants
SET revoked_at = clock_timestamp(),
    revoked_by_type = sqlc.arg(revoked_by_type),
    revoked_by_id = sqlc.narg(revoked_by_id),
    revoke_reason = sqlc.arg(revoke_reason)
WHERE id = sqlc.arg(grant_id)
  AND run_id = sqlc.arg(run_id)
  AND attempt_id = sqlc.arg(attempt_id)
  AND lease_id = sqlc.arg(lease_id)
  AND fencing_token = sqlc.arg(fencing_token)
  AND revoked_at IS NULL
RETURNING *;

-- name: LockExpiredRuntimeResumeGrants :many
SELECT *
FROM runtime_resume_grants
WHERE revoked_at IS NULL
  AND expires_at <= clock_timestamp()
ORDER BY expires_at ASC, id ASC
LIMIT sqlc.arg(batch_limit)
FOR UPDATE SKIP LOCKED;

-- name: RevokeExpiredRuntimeResumeGrant :one
UPDATE runtime_resume_grants
SET revoked_at = clock_timestamp(),
    revoked_by_type = 'system',
    revoked_by_id = NULL,
    revoke_reason = 'expired'
WHERE id = sqlc.arg(grant_id)
  AND expires_at = sqlc.arg(expected_expires_at)
  AND expires_at <= clock_timestamp()
  AND revoked_at IS NULL
RETURNING *;
