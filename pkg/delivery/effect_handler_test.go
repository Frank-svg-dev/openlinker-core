package delivery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
	runtimepkg "github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

func TestDefaultDeliveryEffectRetriesReuseStableDelivery(t *testing.T) {
	effectID := uuid.New()
	runID := uuid.New()
	userID := uuid.New()
	agentID := uuid.New()
	targetID := uuid.New()
	var deliveryIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryIDs = append(deliveryIDs, r.Header.Get("X-OpenLinker-Delivery"))
		http.Error(w, "retry", http.StatusBadGateway)
	}))
	defer server.Close()

	q := &fakeDeliveryEffectQueries{
		run: db.Run{
			ID: runID, UserID: userID, AgentID: agentID, Status: "success",
			Input: []byte(`{"query":"hello"}`), Output: []byte(`{"answer":"done"}`),
			StartedAt: time.Now(),
		},
		agent: db.Agent{ID: agentID, Slug: "helper", Name: "Helper"},
		target: db.DeliveryTarget{
			ID: targetID, UserID: userID, Type: "webhook",
			Config: []byte(fmt.Sprintf("{\"url\":%q}", server.URL)), Secret: "secret",
		},
	}
	svc := &Service{effectQueries: q, httpClient: server.Client()}
	firstLease := uuid.New()
	effect := db.RunEffectOutbox{
		ID: effectID, RunID: runID, EffectType: runtimepkg.RunEffectTypeDefaultDelivery,
		TargetKey: "delivery_target:" + targetID.String(), Status: "processing",
		LeaseOwner: &firstLease, AttemptCount: 1, MaxAttempts: 3,
	}

	first := svc.AttemptDefaultDeliveryEffect(context.Background(), effect)
	require.False(t, first.Succeeded)
	require.True(t, first.Retryable)
	secondLease := uuid.New()
	effect.LeaseOwner = &secondLease
	effect.AttemptCount = 2
	second := svc.AttemptDefaultDeliveryEffect(context.Background(), effect)
	require.False(t, second.Succeeded)
	require.True(t, second.Retryable)

	require.Equal(t, 1, q.createCount)
	require.Equal(t, effectID, q.delivery.ID)
	require.NotNil(t, q.delivery.EffectOutboxID)
	require.Equal(t, effectID, *q.delivery.EffectOutboxID)
	require.Equal(t, int32(2), q.delivery.AttemptCount)
	require.Equal(t, []string{effectID.String(), effectID.String()}, deliveryIDs)
}

func TestDefaultDeliveryEffectOldAttemptIsFenced(t *testing.T) {
	effectID := uuid.New()
	runID := uuid.New()
	targetID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	secret := "secret"
	q := &fakeDeliveryEffectQueries{
		delivery: db.RunDelivery{
			ID: effectID, RunID: runID, TargetID: &targetID, TargetType: "webhook",
			TargetURL: server.URL, Payload: []byte(`{"event":"run.completed"}`),
			Status: "pending", AttemptCount: 2, EffectOutboxID: &effectID,
		},
		targetSecret: &secret,
	}
	svc := &Service{effectQueries: q, httpClient: server.Client()}
	oldLease := uuid.New()
	result := svc.AttemptDefaultDeliveryEffect(context.Background(), db.RunEffectOutbox{
		ID: effectID, RunID: runID, EffectType: runtimepkg.RunEffectTypeDefaultDelivery,
		TargetKey: "delivery_target:" + targetID.String(), Status: "processing",
		LeaseOwner: &oldLease, AttemptCount: 1, MaxAttempts: 3,
	})

	require.False(t, result.Succeeded)
	require.True(t, result.Retryable)
	require.Equal(t, "DELIVERY_ATTEMPT_SUPERSEDED", result.ErrorCode)
	require.Equal(t, "pending", q.delivery.Status)
	require.Equal(t, int32(2), q.delivery.AttemptCount)
}

func TestResetDefaultDeliveryEffectReconcilesPendingCrashWindow(t *testing.T) {
	effectID := uuid.New()
	runID := uuid.New()
	zero := int64(0)
	q := &fakeDeliveryEffectQueries{
		delivery: db.RunDelivery{
			ID: effectID, RunID: runID, Status: "pending", EffectOutboxID: &effectID,
		},
		resetRows: &zero,
	}
	svc := &Service{effectQueries: q}
	err := svc.ResetDefaultDeliveryEffect(context.Background(), db.RunEffectOutbox{
		ID: effectID, RunID: runID, EffectType: runtimepkg.RunEffectTypeDefaultDelivery,
	})
	require.NoError(t, err)
}

func TestResetDefaultDeliveryEffectRejectsFailedZeroRowReset(t *testing.T) {
	effectID := uuid.New()
	runID := uuid.New()
	zero := int64(0)
	q := &fakeDeliveryEffectQueries{
		delivery: db.RunDelivery{
			ID: effectID, RunID: runID, Status: "failed", EffectOutboxID: &effectID,
		},
		resetRows: &zero,
	}
	svc := &Service{effectQueries: q}
	err := svc.ResetDefaultDeliveryEffect(context.Background(), db.RunEffectOutbox{
		ID: effectID, RunID: runID, EffectType: runtimepkg.RunEffectTypeDefaultDelivery,
	})
	require.ErrorContains(t, err, "remains failed")
}

type fakeDeliveryEffectQueries struct {
	run          db.Run
	agent        db.Agent
	target       db.DeliveryTarget
	delivery     db.RunDelivery
	targetSecret *string
	createCount  int
	resetRows    *int64
}

func (q *fakeDeliveryEffectQueries) GetRunByID(context.Context, uuid.UUID) (db.Run, error) {
	if q.run.ID == uuid.Nil {
		return db.Run{}, pgx.ErrNoRows
	}
	return q.run, nil
}

func (q *fakeDeliveryEffectQueries) GetAgentByID(context.Context, uuid.UUID) (db.Agent, error) {
	if q.agent.ID == uuid.Nil {
		return db.Agent{}, pgx.ErrNoRows
	}
	return q.agent, nil
}

func (q *fakeDeliveryEffectQueries) GetDeliveryTargetByID(context.Context, uuid.UUID) (db.DeliveryTarget, error) {
	if q.target.ID == uuid.Nil {
		return db.DeliveryTarget{}, pgx.ErrNoRows
	}
	return q.target, nil
}

func (q *fakeDeliveryEffectQueries) CreateRunEffectDelivery(_ context.Context, arg db.CreateRunEffectDeliveryParams) (db.RunDelivery, error) {
	if q.delivery.ID == uuid.Nil {
		q.createCount++
		targetID := arg.TargetID
		q.delivery = db.RunDelivery{
			ID: arg.ID, RunID: arg.RunID, TargetID: &targetID, UserID: arg.UserID,
			TargetType: arg.TargetType, TargetURL: arg.TargetURL, Payload: arg.Payload,
			Status: "pending", EffectOutboxID: &arg.ID,
		}
		secret := q.target.Secret
		q.targetSecret = &secret
	}
	return q.delivery, nil
}

func (q *fakeDeliveryEffectQueries) GetRunDeliveryByID(context.Context, uuid.UUID) (db.GetRunDeliveryRow, error) {
	if q.delivery.ID == uuid.Nil {
		return db.GetRunDeliveryRow{}, pgx.ErrNoRows
	}
	return db.GetRunDeliveryRow{
		RunDelivery: q.delivery, TargetSecret: q.targetSecret, TargetConfig: q.target.Config,
	}, nil
}

func (q *fakeDeliveryEffectQueries) MarkRunEffectDeliverySuccess(_ context.Context, arg db.MarkRunEffectDeliverySuccessParams) (int64, error) {
	if q.delivery.Status != "pending" || q.delivery.AttemptCount >= arg.AttemptCount {
		return 0, nil
	}
	q.delivery.Status = "success"
	q.delivery.AttemptCount = arg.AttemptCount
	return 1, nil
}

func (q *fakeDeliveryEffectQueries) MarkRunEffectDeliveryRetry(_ context.Context, arg db.MarkRunEffectDeliveryRetryParams) (int64, error) {
	if q.delivery.Status != "pending" || q.delivery.AttemptCount >= arg.AttemptCount {
		return 0, nil
	}
	q.delivery.AttemptCount = arg.AttemptCount
	return 1, nil
}

func (q *fakeDeliveryEffectQueries) MarkRunEffectDeliveryFailed(_ context.Context, arg db.MarkRunEffectDeliveryFailedParams) (int64, error) {
	if q.delivery.Status != "pending" || q.delivery.AttemptCount >= arg.AttemptCount {
		return 0, nil
	}
	q.delivery.Status = "failed"
	q.delivery.AttemptCount = arg.AttemptCount
	return 1, nil
}

func (q *fakeDeliveryEffectQueries) ResetRunEffectDelivery(context.Context, uuid.UUID) (int64, error) {
	if q.resetRows != nil {
		return *q.resetRows, nil
	}
	q.delivery.Status = "pending"
	q.delivery.AttemptCount = 0
	return 1, nil
}
