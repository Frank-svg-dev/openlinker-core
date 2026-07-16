package externalexecution

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ExecutionRecord struct {
	CallerServiceID           string
	ExternalRequestID         uuid.UUID
	RequestFingerprintVersion int16
	ActorUserID               uuid.UUID
	TargetType                string
	TargetID                  uuid.UUID
	InputFingerprint          []byte
	ExpectedContractHash      *string
	InputSchemaFingerprint    []byte
	TraceID                   string
	DownstreamReplayIdentity  []byte
	StartState                string
	StartToken                *uuid.UUID
	StartLeaseUntil           *time.Time
	AuthorizedTargetOwnerID   *uuid.UUID
	RejectionCode             *string
	ExecutionKind             *string
	ExecutionID               *uuid.UUID
}

type Store interface {
	ResolveTargetOwner(context.Context, string, uuid.UUID) (uuid.UUID, error)
	Reserve(context.Context, ExecutionRecord) (ExecutionRecord, error)
	PromoteLegacyReservation(context.Context, ExecutionRecord) (ExecutionRecord, error)
	Get(context.Context, string, uuid.UUID) (ExecutionRecord, error)
	ClaimStartEvaluation(context.Context, string, uuid.UUID, uuid.UUID, time.Duration) (ExecutionRecord, bool, error)
	AuthorizeStart(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (ExecutionRecord, bool, error)
	RejectStart(context.Context, string, uuid.UUID, uuid.UUID, string) (ExecutionRecord, bool, error)
	ReleaseStartEvaluation(context.Context, string, uuid.UUID, uuid.UUID) error
	Attach(context.Context, string, uuid.UUID, string, uuid.UUID) (ExecutionRecord, error)
}

func (s *SQLStore) PromoteLegacyReservation(ctx context.Context, record ExecutionRecord) (ExecutionRecord, error) {
	if s.pool == nil {
		return ExecutionRecord{}, errors.New("external execution database is not configured")
	}
	row := s.pool.QueryRow(ctx, `
			UPDATE external_executions
			SET request_fingerprint_version = $3,
			    input_fingerprint = $8,
			    expected_contract_hash = $9,
			    input_schema_fingerprint = $10
			WHERE caller_service_id = $1
			  AND external_request_id = $2
			  AND request_fingerprint_version = 1
			  AND actor_user_id = $4
			  AND target_type = $5
			  AND target_id = $6
			  AND trace_id = $7
			  AND execution_kind IS NULL
			  AND execution_id IS NULL
			RETURNING `+executionRecordColumns+`
		`, record.CallerServiceID, record.ExternalRequestID, record.RequestFingerprintVersion,
		record.ActorUserID, record.TargetType, record.TargetID, record.TraceID,
		record.InputFingerprint, record.ExpectedContractHash, record.InputSchemaFingerprint)
	promoted, err := scanExecutionRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.Get(ctx, record.CallerServiceID, record.ExternalRequestID)
	}
	return promoted, err
}

type SQLStore struct {
	pool *pgxpool.Pool
}

const executionRecordColumns = `caller_service_id, external_request_id, request_fingerprint_version, actor_user_id, target_type,
	target_id, input_fingerprint, trace_id, expected_contract_hash, input_schema_fingerprint,
	downstream_replay_identity, start_state, start_token, start_lease_until, authorized_target_owner_id, rejection_code,
	execution_kind, execution_id`

func NewSQLStore(pool *pgxpool.Pool) *SQLStore {
	return &SQLStore{pool: pool}
}

func (s *SQLStore) ResolveTargetOwner(ctx context.Context, targetType string, targetID uuid.UUID) (uuid.UUID, error) {
	if s.pool == nil {
		return uuid.Nil, errors.New("external execution database is not configured")
	}
	var ownerID uuid.UUID
	var query string
	switch targetType {
	case TargetTypeAgent:
		query = `SELECT creator_id FROM agents WHERE id = $1`
	case TargetTypeWorkflow:
		query = `SELECT user_id FROM workflows WHERE id = $1`
	default:
		return uuid.Nil, errors.New("unsupported external execution target type")
	}
	if err := s.pool.QueryRow(ctx, query, targetID).Scan(&ownerID); err != nil {
		return uuid.Nil, err
	}
	return ownerID, nil
}

func (s *SQLStore) Reserve(ctx context.Context, record ExecutionRecord) (ExecutionRecord, error) {
	if s.pool == nil {
		return ExecutionRecord{}, errors.New("external execution database is not configured")
	}
	_, err := s.pool.Exec(ctx, `
			INSERT INTO external_executions (
				caller_service_id, external_request_id, request_fingerprint_version, actor_user_id, target_type,
				target_id, input_fingerprint, trace_id, expected_contract_hash, input_schema_fingerprint
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (caller_service_id, external_request_id) DO NOTHING
		`, record.CallerServiceID, record.ExternalRequestID, record.RequestFingerprintVersion, record.ActorUserID, record.TargetType,
		record.TargetID, record.InputFingerprint, record.TraceID, record.ExpectedContractHash, record.InputSchemaFingerprint)
	if err != nil {
		return ExecutionRecord{}, err
	}
	return s.Get(ctx, record.CallerServiceID, record.ExternalRequestID)
}

func (s *SQLStore) Get(ctx context.Context, callerServiceID string, externalRequestID uuid.UUID) (ExecutionRecord, error) {
	if s.pool == nil {
		return ExecutionRecord{}, errors.New("external execution database is not configured")
	}
	row := s.pool.QueryRow(ctx, `
			SELECT `+executionRecordColumns+`
			FROM external_executions
			WHERE caller_service_id = $1 AND external_request_id = $2
		`, callerServiceID, externalRequestID)
	return scanExecutionRecord(row)
}

func (s *SQLStore) ClaimStartEvaluation(
	ctx context.Context,
	callerServiceID string,
	externalRequestID, token uuid.UUID,
	lease time.Duration,
) (ExecutionRecord, bool, error) {
	if s.pool == nil {
		return ExecutionRecord{}, false, errors.New("external execution database is not configured")
	}
	leaseMilliseconds := lease.Milliseconds()
	if token == uuid.Nil || leaseMilliseconds < 1 {
		return ExecutionRecord{}, false, errors.New("external execution start evaluation claim is invalid")
	}
	row := s.pool.QueryRow(ctx, `
			UPDATE external_executions
			SET start_state = 'evaluating',
			    start_token = $3,
			    start_lease_until = clock_timestamp() + ($4::bigint * interval '1 millisecond'),
			    rejection_code = NULL
			WHERE caller_service_id = $1
			  AND external_request_id = $2
			  AND execution_id IS NULL
			  AND (
			    start_state = 'pending'
			    OR (start_state = 'evaluating' AND start_lease_until <= clock_timestamp())
			  )
			RETURNING `+executionRecordColumns+`
		`, callerServiceID, externalRequestID, token, leaseMilliseconds)
	record, err := scanExecutionRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		record, err = s.Get(ctx, callerServiceID, externalRequestID)
		return record, false, err
	}
	return record, err == nil, err
}

func (s *SQLStore) AuthorizeStart(
	ctx context.Context,
	callerServiceID string,
	externalRequestID, token, targetOwnerID uuid.UUID,
) (ExecutionRecord, bool, error) {
	if s.pool == nil {
		return ExecutionRecord{}, false, errors.New("external execution database is not configured")
	}
	if token == uuid.Nil || targetOwnerID == uuid.Nil {
		return ExecutionRecord{}, false, errors.New("external execution start authorization is invalid")
	}
	row := s.pool.QueryRow(ctx, `
			UPDATE external_executions
			SET start_state = 'authorized', start_token = NULL, start_lease_until = NULL,
			    authorized_target_owner_id = $4, rejection_code = NULL
			WHERE caller_service_id = $1
			  AND external_request_id = $2
			  AND execution_id IS NULL
			  AND start_state = 'evaluating'
			  AND start_token = $3
			  AND start_lease_until > clock_timestamp()
			RETURNING `+executionRecordColumns+`
		`, callerServiceID, externalRequestID, token, targetOwnerID)
	record, err := scanExecutionRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		record, err = s.Get(ctx, callerServiceID, externalRequestID)
		return record, false, err
	}
	return record, err == nil, err
}

func (s *SQLStore) RejectStart(
	ctx context.Context,
	callerServiceID string,
	externalRequestID, token uuid.UUID,
	rejectionCode string,
) (ExecutionRecord, bool, error) {
	if s.pool == nil {
		return ExecutionRecord{}, false, errors.New("external execution database is not configured")
	}
	row := s.pool.QueryRow(ctx, `
			UPDATE external_executions
			SET start_state = 'rejected', start_token = NULL, start_lease_until = NULL,
			    authorized_target_owner_id = NULL, rejection_code = $4
			WHERE caller_service_id = $1
			  AND external_request_id = $2
			  AND execution_id IS NULL
			  AND start_state = 'evaluating'
			  AND start_token = $3
			  AND start_lease_until > clock_timestamp()
			RETURNING `+executionRecordColumns+`
		`, callerServiceID, externalRequestID, token, rejectionCode)
	record, err := scanExecutionRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		record, err = s.Get(ctx, callerServiceID, externalRequestID)
		return record, false, err
	}
	return record, err == nil, err
}

func (s *SQLStore) ReleaseStartEvaluation(
	ctx context.Context,
	callerServiceID string,
	externalRequestID, token uuid.UUID,
) error {
	if s.pool == nil {
		return errors.New("external execution database is not configured")
	}
	_, err := s.pool.Exec(ctx, `
			UPDATE external_executions
			SET start_state = 'pending', start_token = NULL, start_lease_until = NULL,
			    authorized_target_owner_id = NULL, rejection_code = NULL
			WHERE caller_service_id = $1
			  AND external_request_id = $2
			  AND execution_id IS NULL
			  AND start_state = 'evaluating'
			  AND start_token = $3
		`, callerServiceID, externalRequestID, token)
	return err
}

func (s *SQLStore) Attach(ctx context.Context, callerServiceID string, externalRequestID uuid.UUID, executionKind string, executionID uuid.UUID) (ExecutionRecord, error) {
	if s.pool == nil {
		return ExecutionRecord{}, errors.New("external execution database is not configured")
	}
	row := s.pool.QueryRow(ctx, `
			UPDATE external_executions
			SET execution_kind = $3,
			    execution_id = $4,
			    start_state = 'authorized',
			    start_token = NULL,
			    start_lease_until = NULL,
			    rejection_code = NULL
			WHERE caller_service_id = $1 AND external_request_id = $2
			  AND execution_id IS NULL
			  AND start_state IN ('pending', 'evaluating', 'authorized')
			RETURNING `+executionRecordColumns+`
		`, callerServiceID, externalRequestID, executionKind, executionID)
	record, err := scanExecutionRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.Get(ctx, callerServiceID, externalRequestID)
	}
	return record, err
}

func scanExecutionRecord(row interface{ Scan(...any) error }) (ExecutionRecord, error) {
	var record ExecutionRecord
	err := row.Scan(
		&record.CallerServiceID,
		&record.ExternalRequestID,
		&record.RequestFingerprintVersion,
		&record.ActorUserID,
		&record.TargetType,
		&record.TargetID,
		&record.InputFingerprint,
		&record.TraceID,
		&record.ExpectedContractHash,
		&record.InputSchemaFingerprint,
		&record.DownstreamReplayIdentity,
		&record.StartState,
		&record.StartToken,
		&record.StartLeaseUntil,
		&record.AuthorizedTargetOwnerID,
		&record.RejectionCode,
		&record.ExecutionKind,
		&record.ExecutionID,
	)
	return record, err
}
