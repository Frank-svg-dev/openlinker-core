package servicebridge

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSQLStorePersistsHostedContractEvidence(t *testing.T) {
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
	buyerID, sellerID := uuid.New(), uuid.New()
	for _, userID := range []uuid.UUID{buyerID, sellerID} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (id,email,password_hash,display_name) VALUES ($1,$2,'x','Bridge Test')`, userID, userID.String()+"@bridge.test"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM hosted_service_executions WHERE buyer_user_id=$1 OR seller_user_id=$2`, buyerID, sellerID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1 OR id=$2`, buyerID, sellerID)
	})

	orderID, targetID, executionID := uuid.New(), uuid.New(), uuid.New()
	contractHash := "hct:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	inputFingerprint := sha256.Sum256([]byte("execution"))
	schemaFingerprint := sha256.Sum256([]byte("schema"))
	store := NewSQLStore(pool)
	record, err := store.Reserve(ctx, ExecutionRecord{
		ExternalOrderID: orderID, BuyerUserID: buyerID, SellerUserID: sellerID,
		TargetType: TargetTypeAgent, TargetID: targetID, InputFingerprint: inputFingerprint[:],
		ExpectedContractHash: &contractHash, InputSchemaFingerprint: schemaFingerprint[:], TraceID: "trace-live-db",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ExpectedContractHash == nil || *record.ExpectedContractHash != contractHash {
		t.Fatalf("expected contract hash = %#v", record.ExpectedContractHash)
	}
	if string(record.InputSchemaFingerprint) != string(schemaFingerprint[:]) {
		t.Fatal("schema fingerprint did not round trip")
	}
	attached, err := store.Attach(ctx, orderID, "run", executionID)
	if err != nil {
		t.Fatal(err)
	}
	if attached.ExecutionID == nil || *attached.ExecutionID != executionID {
		t.Fatalf("execution ID = %#v", attached.ExecutionID)
	}
}
