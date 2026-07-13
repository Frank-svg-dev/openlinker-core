-- Reliable Runtime dead-letter inventory.

-- name: CreateRunDeadLetter :one
INSERT INTO run_dead_letters (
    run_id, final_attempt_no, reason_code, reason_redacted
) VALUES ($1, $2, $3, $4)
ON CONFLICT (run_id) DO NOTHING
RETURNING *;

-- name: GetRunDeadLetterByRun :one
SELECT * FROM run_dead_letters WHERE run_id = $1;

-- name: ListRunDeadLetters :many
SELECT dlq.id AS dead_letter_id,
       dlq.run_id,
       r.agent_id,
       a.slug AS agent_slug,
       a.name AS agent_name,
       r.status,
       r.dispatch_state,
       r.attempt_count,
       r.max_attempts,
       r.latest_attempt_id AS final_attempt_id,
       dlq.final_attempt_no,
       r.error_code,
       r.error_message,
       final_attempt.error_detail_redacted,
       dlq.reason_code,
       dlq.reason_redacted,
       r.dead_lettered_at,
       dlq.created_at,
       r.replay_of_run_id,
       ARRAY(
           SELECT replay.id
           FROM runs replay
           WHERE replay.replay_of_run_id = r.id
           ORDER BY replay.started_at ASC, replay.id ASC
       )::uuid[] AS replayed_as_run_ids
FROM run_dead_letters dlq
JOIN runs r ON r.id = dlq.run_id
JOIN agents a ON a.id = r.agent_id
LEFT JOIN run_attempts final_attempt ON final_attempt.id = r.latest_attempt_id
ORDER BY dlq.created_at DESC, dlq.id DESC
LIMIT $1 OFFSET $2;

-- name: CountRunDeadLetters :one
SELECT COUNT(*)::int FROM run_dead_letters;
