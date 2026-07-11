package db

import (
	"strings"
	"testing"
)

func TestEffectDeliveryMarkQueriesFenceCurrentLease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		query              string
		attemptPlaceholder string
		leasePlaceholder   string
	}{
		{name: "webhook success", query: markWebhookEffectDeliverySuccess, attemptPlaceholder: "$4", leasePlaceholder: "$5"},
		{name: "webhook retry", query: markWebhookEffectDeliveryRetry, attemptPlaceholder: "$5", leasePlaceholder: "$6"},
		{name: "webhook failed", query: markWebhookEffectDeliveryFailed, attemptPlaceholder: "$5", leasePlaceholder: "$6"},
		{name: "task callback success", query: markTaskCallbackEffectDeliverySuccess, attemptPlaceholder: "$4", leasePlaceholder: "$5"},
		{name: "task callback retry", query: markTaskCallbackEffectDeliveryRetry, attemptPlaceholder: "$5", leasePlaceholder: "$6"},
		{name: "task callback failed", query: markTaskCallbackEffectDeliveryFailed, attemptPlaceholder: "$5", leasePlaceholder: "$6"},
		{name: "default delivery success", query: markRunEffectDeliverySuccess, attemptPlaceholder: "$4", leasePlaceholder: "$5"},
		{name: "default delivery retry", query: markRunEffectDeliveryRetry, attemptPlaceholder: "$5", leasePlaceholder: "$6"},
		{name: "default delivery failed", query: markRunEffectDeliveryFailed, attemptPlaceholder: "$5", leasePlaceholder: "$6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertSQLContains(t, tt.query,
				"effect_outbox_id = $1",
				"status = 'pending'",
				"attempt_count < "+tt.attemptPlaceholder,
				"FROM run_effect_outbox effect",
				"effect.id = $1",
				"effect.status = 'processing'",
				"effect.lease_owner = "+tt.leasePlaceholder,
				"effect.attempt_count = "+tt.attemptPlaceholder,
			)
		})
	}
}

func TestEffectDeliveryResetQueriesRequireDeadLetter(t *testing.T) {
	t.Parallel()

	for name, query := range map[string]string{
		"webhook":       resetWebhookEffectDelivery,
		"task callback": resetTaskCallbackEffectDelivery,
		"default":       resetRunEffectDelivery,
	} {
		t.Run(name, func(t *testing.T) {
			assertSQLContains(t, query,
				"effect_outbox_id = $1",
				"status = 'failed'",
				"FROM run_effect_outbox effect",
				"effect.id = $1",
				"effect.status = 'dead_letter'",
			)
		})
	}
}

func TestLegacyPendingDeliveryQueriesExcludeEffectOutboxRows(t *testing.T) {
	t.Parallel()

	for name, query := range map[string]string{
		"webhook":       listPendingDeliveries,
		"task callback": listPendingTaskCallbackDeliveries,
		"default":       listPendingRunDeliveries,
	} {
		t.Run(name, func(t *testing.T) {
			assertSQLContains(t, query,
				"status = 'pending'",
				"effect_outbox_id IS NULL",
			)
		})
	}
}

func TestRunEffectParentEventQueriesPreserveIdempotencyAndIdentity(t *testing.T) {
	t.Parallel()

	assertSQLContains(t, createRunEffectParentEvent,
		"WHERE r.id = $2::uuid",
		"$1, target_run.id, NULL",
		"'run.child.completed', $3",
		"ON CONFLICT (id) DO NOTHING",
	)
	if strings.Contains(createRunEffectParentEvent, "DO UPDATE") {
		t.Fatal("CreateRunEffectParentEvent must not mutate an existing immutable run event")
	}

	assertSQLContains(t, getMatchingRunEffectParentEvent,
		"WHERE id = $1",
		"run_id = $2",
		"parent_run_id IS NULL",
		"event_type = 'run.child.completed'",
		"payload = $3",
	)
}

func assertSQLContains(t *testing.T, query string, fragments ...string) {
	t.Helper()

	for _, fragment := range fragments {
		if !strings.Contains(query, fragment) {
			t.Errorf("query does not contain %q:\n%s", fragment, query)
		}
	}
}
