package externalexecution

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSQLStorePersistsExternalContractEvidence(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	actorID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,email,password_hash,display_name) VALUES ($1,$2,'x','External Execution Test')`, actorID, actorID.String()+"@external-execution.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM external_executions WHERE caller_service_id=$1 AND actor_user_id=$2`, "integration-test", actorID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, actorID)
	})

	requestID, targetID, executionID := uuid.New(), uuid.New(), uuid.New()
	contractHash := "hct:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	inputFingerprint := sha256.Sum256([]byte("execution"))
	schemaFingerprint := sha256.Sum256([]byte("schema"))
	store := NewSQLStore(pool)
	record, err := store.Reserve(ctx, ExecutionRecord{
		CallerServiceID: "integration-test", ExternalRequestID: requestID, ActorUserID: actorID,
		RequestFingerprintVersion: currentRequestFingerprintVersion,
		TargetType:                TargetTypeAgent, TargetID: targetID, InputFingerprint: inputFingerprint[:],
		ExpectedContractHash: &contractHash, InputSchemaFingerprint: schemaFingerprint[:], TraceID: "trace-live-db",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ExpectedContractHash == nil || *record.ExpectedContractHash != contractHash {
		t.Fatalf("expected contract hash = %#v", record.ExpectedContractHash)
	}
	if record.RequestFingerprintVersion != currentRequestFingerprintVersion {
		t.Fatalf("request fingerprint version = %d", record.RequestFingerprintVersion)
	}
	if string(record.InputSchemaFingerprint) != string(schemaFingerprint[:]) {
		t.Fatal("schema fingerprint did not round trip")
	}
	if record.StartState != startStatePending {
		t.Fatalf("initial start state = %q", record.StartState)
	}
	firstToken, replacementToken := uuid.New(), uuid.New()
	claimed, acquired, err := store.ClaimStartEvaluation(ctx, "integration-test", requestID, firstToken, time.Minute)
	if err != nil || !acquired || claimed.StartToken == nil || *claimed.StartToken != firstToken {
		t.Fatalf("first evaluation claim = %#v, %v, %v", claimed, acquired, err)
	}
	contended, acquired, err := store.ClaimStartEvaluation(ctx, "integration-test", requestID, replacementToken, time.Minute)
	if err != nil || acquired || contended.StartToken == nil || *contended.StartToken != firstToken {
		t.Fatalf("contended evaluation claim = %#v, %v, %v", contended, acquired, err)
	}
	if _, authorized, err := store.AuthorizeStart(ctx, "integration-test", requestID, replacementToken, actorID); err != nil || authorized {
		t.Fatalf("wrong-token authorization = %v, %v", authorized, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE external_executions SET start_lease_until=clock_timestamp()-interval '1 second' WHERE caller_service_id=$1 AND external_request_id=$2`, "integration-test", requestID); err != nil {
		t.Fatal(err)
	}
	reclaimed, acquired, err := store.ClaimStartEvaluation(ctx, "integration-test", requestID, replacementToken, time.Minute)
	if err != nil || !acquired || reclaimed.StartToken == nil || *reclaimed.StartToken != replacementToken {
		t.Fatalf("expired evaluation reclaim = %#v, %v, %v", reclaimed, acquired, err)
	}
	if _, authorized, err := store.AuthorizeStart(ctx, "integration-test", requestID, firstToken, actorID); err != nil || authorized {
		t.Fatalf("expired claimant late authorization = %v, %v", authorized, err)
	}
	authorizedRecord, authorized, err := store.AuthorizeStart(ctx, "integration-test", requestID, replacementToken, actorID)
	if err != nil || !authorized || authorizedRecord.StartState != startStateAuthorized ||
		authorizedRecord.AuthorizedTargetOwnerID == nil || *authorizedRecord.AuthorizedTargetOwnerID != actorID {
		t.Fatalf("replacement authorization = %#v, %v, %v", authorizedRecord, authorized, err)
	}
	attached, err := store.Attach(ctx, "integration-test", requestID, "run", executionID)
	if err != nil {
		t.Fatal(err)
	}
	if attached.ExecutionID == nil || *attached.ExecutionID != executionID {
		t.Fatalf("execution ID = %#v", attached.ExecutionID)
	}
	if attached.StartState != startStateAuthorized || attached.AuthorizedTargetOwnerID == nil || *attached.AuthorizedTargetOwnerID != actorID {
		t.Fatalf("attached authorization state = %#v", attached)
	}

	rejectedRequestID, rejectedExecutionID, rejectedToken := uuid.New(), uuid.New(), uuid.New()
	if _, err := store.Reserve(ctx, ExecutionRecord{
		CallerServiceID: "integration-test", ExternalRequestID: rejectedRequestID, ActorUserID: actorID,
		RequestFingerprintVersion: currentRequestFingerprintVersion,
		TargetType:                TargetTypeAgent, TargetID: targetID, InputFingerprint: inputFingerprint[:],
		ExpectedContractHash: &contractHash, InputSchemaFingerprint: schemaFingerprint[:], TraceID: "trace-rejected-attach",
	}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimStartEvaluation(ctx, "integration-test", rejectedRequestID, rejectedToken, time.Minute); err != nil || !claimed {
		t.Fatalf("rejected record claim = %v, %v", claimed, err)
	}
	rejectedRecord, rejected, err := store.RejectStart(ctx, "integration-test", rejectedRequestID, rejectedToken, "DOWNSTREAM_IDENTITY_CONFLICT")
	if err != nil || !rejected || rejectedRecord.StartState != startStateRejected {
		t.Fatalf("durable rejection = %#v, %v, %v", rejectedRecord, rejected, err)
	}
	afterAttach, err := store.Attach(ctx, "integration-test", rejectedRequestID, "run", rejectedExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if afterAttach.StartState != startStateRejected || afterAttach.ExecutionID != nil || afterAttach.ExecutionKind != nil ||
		afterAttach.RejectionCode == nil || *afterAttach.RejectionCode != "DOWNSTREAM_IDENTITY_CONFLICT" {
		t.Fatalf("Attach overwrote durable rejection: %#v", afterAttach)
	}
}
