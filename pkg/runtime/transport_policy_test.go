package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCurrentRuntimeLivenessPolicyCoversTwoDatabaseHeartbeats(t *testing.T) {
	policy := CurrentRuntimeLivenessPolicy()

	require.Equal(t, 20*time.Second, policy.HeartbeatInterval)
	require.Equal(t, 45*time.Second, policy.SessionStaleAfter)
	require.Equal(t, 60*time.Second, policy.PresenceTTL)
	require.Greater(t, policy.SessionStaleAfter, 2*policy.HeartbeatInterval)
	require.Greater(t, policy.PresenceTTL, policy.SessionStaleAfter)
}

func TestCurrentRuntimeTransportPolicyPreservesWebSocketFallbackDefaults(t *testing.T) {
	policy := CurrentRuntimeTransportPolicy()

	require.Equal(t, 1, policy.Version)
	require.Equal(t, []RuntimeTransport{RuntimeTransportWebSocket, RuntimeTransportLongPoll}, policy.OrderedTransports)
	require.Equal(t, "auto", policy.DefaultTransport)
	require.Equal(t, 250*time.Millisecond, policy.RetryMinimum)
	require.Equal(t, 15*time.Second, policy.RetryMaximum)
	require.Equal(t, 15*time.Second, policy.WebSocketProbeInterval)
	require.Equal(t, 10*time.Second, policy.WebSocketProbeTimeout)
}

func TestRuntimeTransportReasonUsesOnlyServerObservedTransition(t *testing.T) {
	tests := []struct {
		name      string
		transport RuntimeTransport
		previous  RuntimeTransport
		want      RuntimeTransportReason
	}{
		{name: "first websocket", transport: RuntimeTransportWebSocket, want: RuntimeTransportReasonExplicit},
		{name: "first long poll", transport: RuntimeTransportLongPoll, want: RuntimeTransportReasonExplicit},
		{name: "websocket unavailable", transport: RuntimeTransportLongPoll, previous: RuntimeTransportWebSocket, want: RuntimeTransportReasonWebSocketUnavailable},
		{name: "websocket recovered", transport: RuntimeTransportWebSocket, previous: RuntimeTransportLongPoll, want: RuntimeTransportReasonRecovery},
		{name: "same transport reattach", transport: RuntimeTransportWebSocket, previous: RuntimeTransportWebSocket, want: RuntimeTransportReasonExplicit},
		{name: "legacy history", transport: RuntimeTransportWebSocket, previous: RuntimeTransportUnknown, want: RuntimeTransportReasonExplicit},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, runtimeTransportReasonForAttachment(test.transport, test.previous))
		})
	}
}

func TestResolveRuntimeTransportReasonValidatesSDKHintAgainstServerTruth(t *testing.T) {
	current := CurrentRuntimeTransportPolicy()
	longPollOnly := current
	longPollOnly.OrderedTransports = []RuntimeTransport{RuntimeTransportLongPoll}

	tests := []struct {
		name      string
		transport RuntimeTransport
		previous  RuntimeTransport
		reported  RuntimeTransportReason
		policy    RuntimeTransportPolicy
		want      RuntimeTransportReason
		valid     bool
	}{
		{name: "explicit actual websocket", transport: RuntimeTransportWebSocket, reported: RuntimeTransportReasonExplicit, policy: current, want: RuntimeTransportReasonExplicit, valid: true},
		{name: "auto follows current primary", transport: RuntimeTransportWebSocket, reported: RuntimeTransportReasonPolicyForced, policy: current, want: RuntimeTransportReasonPolicyForced, valid: true},
		{name: "auto follows restricted primary", transport: RuntimeTransportLongPoll, reported: RuntimeTransportReasonPolicyForced, policy: longPollOnly, want: RuntimeTransportReasonPolicyForced, valid: true},
		{name: "policy reason cannot select secondary", transport: RuntimeTransportLongPoll, reported: RuntimeTransportReasonPolicyForced, policy: current},
		{name: "initial websocket fallback", transport: RuntimeTransportLongPoll, reported: RuntimeTransportReasonWebSocketUnavailable, policy: current, want: RuntimeTransportReasonWebSocketUnavailable, valid: true},
		{name: "websocket fallback transition", transport: RuntimeTransportLongPoll, previous: RuntimeTransportWebSocket, reported: RuntimeTransportReasonWebSocketUnavailable, policy: current, want: RuntimeTransportReasonWebSocketUnavailable, valid: true},
		{name: "continued websocket fallback", transport: RuntimeTransportLongPoll, previous: RuntimeTransportLongPoll, reported: RuntimeTransportReasonWebSocketUnavailable, policy: current, want: RuntimeTransportReasonWebSocketUnavailable, valid: true},
		{name: "unavailable reason requires allowed websocket", transport: RuntimeTransportLongPoll, reported: RuntimeTransportReasonWebSocketUnavailable, policy: longPollOnly},
		{name: "recovery requires long poll history", transport: RuntimeTransportWebSocket, previous: RuntimeTransportLongPoll, reported: RuntimeTransportReasonRecovery, policy: current, want: RuntimeTransportReasonRecovery, valid: true},
		{name: "initial recovery rejected", transport: RuntimeTransportWebSocket, reported: RuntimeTransportReasonRecovery, policy: current},
		{name: "same websocket recovery rejected", transport: RuntimeTransportWebSocket, previous: RuntimeTransportWebSocket, reported: RuntimeTransportReasonRecovery, policy: current},
		{name: "arbitrary reason rejected", transport: RuntimeTransportWebSocket, reported: RuntimeTransportReason("dial tcp private.example"), policy: current},
		{name: "actual entry must be allowed", transport: RuntimeTransportWebSocket, reported: RuntimeTransportReasonExplicit, policy: longPollOnly},
		{name: "invalid prior transport rejected", transport: RuntimeTransportWebSocket, previous: RuntimeTransport("proxy"), reported: RuntimeTransportReasonExplicit, policy: current},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, valid := resolveRuntimeTransportReason(test.transport, test.previous, test.reported, test.policy)
			require.Equal(t, test.valid, valid)
			require.Equal(t, test.want, got)
		})
	}
}
