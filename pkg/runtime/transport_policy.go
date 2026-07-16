package runtime

import "time"

const (
	RuntimeTransportWebSocket RuntimeTransport = "websocket"
	RuntimeTransportLongPoll  RuntimeTransport = "long_poll"
	RuntimeTransportUnknown   RuntimeTransport = "unknown"

	RuntimeTransportReasonExplicit             RuntimeTransportReason = "explicit"
	RuntimeTransportReasonWebSocketUnavailable RuntimeTransportReason = "websocket_unavailable"
	RuntimeTransportReasonPolicyForced         RuntimeTransportReason = "policy_forced"
	RuntimeTransportReasonRecovery             RuntimeTransportReason = "recovery"

	RuntimeHeartbeatInterval = 20 * time.Second
	RuntimeSessionStaleAfter = 45 * time.Second
	RuntimePresenceTTL       = 60 * time.Second

	RuntimeTransportPolicyVersion        = 1
	RuntimeRetryMinimum                  = 250 * time.Millisecond
	RuntimeRetryMaximum                  = 15 * time.Second
	RuntimeWebSocketProbeInterval        = 15 * time.Second
	RuntimeWebSocketProbeTimeout         = 10 * time.Second
	RuntimeDefaultTransportPolicy string = "auto"

	// RuntimeTransportForbiddenSignal and RuntimePolicyChangedSignal are
	// canonical, wire-compatible message values carried with the existing
	// FORBIDDEN error code. They deliberately are not new stable error enum
	// members: current and previous SDKs can distinguish the recovery action
	// without changing the current contract bytes or digest.
	RuntimeTransportForbiddenSignal = "RUNTIME_TRANSPORT_FORBIDDEN"
	RuntimePolicyChangedSignal      = "RUNTIME_POLICY_CHANGED"
)

// RuntimeTransport is the actual Server-observed transport of one attachment.
// unknown is reserved for detached history created before transport metadata
// became durable; a live attachment must use websocket or long_poll.
type RuntimeTransport string

func (transport RuntimeTransport) IsLive() bool {
	return transport == RuntimeTransportWebSocket || transport == RuntimeTransportLongPoll
}

// RuntimeTransportReason is bounded, safe-to-display evidence for why a new
// attachment uses its observed transport. Arbitrary client/network text never
// enters durable attachment or advisory presence state.
type RuntimeTransportReason string

func (reason RuntimeTransportReason) IsValid() bool {
	switch reason {
	case RuntimeTransportReasonExplicit,
		RuntimeTransportReasonWebSocketUnavailable,
		RuntimeTransportReasonPolicyForced,
		RuntimeTransportReasonRecovery:
		return true
	default:
		return false
	}
}

type RuntimeLivenessPolicy struct {
	HeartbeatInterval time.Duration
	SessionStaleAfter time.Duration
	PresenceTTL       time.Duration
}

// CurrentRuntimeLivenessPolicy is the single Core-owned definition used by
// Session transports, PostgreSQL readiness views and Redis advisory presence.
func CurrentRuntimeLivenessPolicy() RuntimeLivenessPolicy {
	return RuntimeLivenessPolicy{
		HeartbeatInterval: RuntimeHeartbeatInterval,
		SessionStaleAfter: RuntimeSessionStaleAfter,
		PresenceTTL:       RuntimePresenceTTL,
	}
}

type RuntimeTransportPolicy struct {
	Version                int
	OrderedTransports      []RuntimeTransport
	DefaultTransport       string
	RetryMinimum           time.Duration
	RetryMaximum           time.Duration
	WebSocketProbeInterval time.Duration
	WebSocketProbeTimeout  time.Duration
}

// RuntimeTransportPolicyProvider supplies the Server-owned policy at request
// time. Production leaves this nil and uses CurrentRuntimeTransportPolicy;
// tests and future dynamic policy storage may inject a concurrency-safe
// provider without moving admission decisions into an SDK or AgentNode.
type RuntimeTransportPolicyProvider func() RuntimeTransportPolicy

// CurrentRuntimeTransportPolicy is copied into discovery responses so callers
// cannot mutate Core's allowlist. The order is authoritative for auto mode.
func CurrentRuntimeTransportPolicy() RuntimeTransportPolicy {
	return RuntimeTransportPolicy{
		Version: RuntimeTransportPolicyVersion,
		OrderedTransports: []RuntimeTransport{
			RuntimeTransportWebSocket,
			RuntimeTransportLongPoll,
		},
		DefaultTransport:       RuntimeDefaultTransportPolicy,
		RetryMinimum:           RuntimeRetryMinimum,
		RetryMaximum:           RuntimeRetryMaximum,
		WebSocketProbeInterval: RuntimeWebSocketProbeInterval,
		WebSocketProbeTimeout:  RuntimeWebSocketProbeTimeout,
	}
}

func runtimeTransportAllowed(policy RuntimeTransportPolicy, transport RuntimeTransport) bool {
	if !transport.IsLive() {
		return false
	}
	for _, allowed := range policy.OrderedTransports {
		if allowed == transport {
			return true
		}
	}
	return false
}

func effectiveRuntimeTransportPolicy(policy RuntimeTransportPolicy) RuntimeTransportPolicy {
	if policy.Version == 0 && len(policy.OrderedTransports) == 0 && policy.DefaultTransport == "" {
		policy = CurrentRuntimeTransportPolicy()
	}
	policy.OrderedTransports = append([]RuntimeTransport(nil), policy.OrderedTransports...)
	return policy
}

func runtimePolicySelectedTransport(policy RuntimeTransportPolicy) RuntimeTransport {
	policy = effectiveRuntimeTransportPolicy(policy)
	configured := RuntimeTransport(policy.DefaultTransport)
	if configured.IsLive() && runtimeTransportAllowed(policy, configured) {
		return configured
	}
	for _, transport := range policy.OrderedTransports {
		if transport.IsLive() {
			return transport
		}
	}
	return ""
}

func runtimeTransportReasonForAttachment(
	transport RuntimeTransport,
	previous RuntimeTransport,
) RuntimeTransportReason {
	if previous == "" || previous == RuntimeTransportUnknown || previous == transport {
		return RuntimeTransportReasonExplicit
	}
	if previous == RuntimeTransportWebSocket && transport == RuntimeTransportLongPoll {
		return RuntimeTransportReasonWebSocketUnavailable
	}
	if previous == RuntimeTransportLongPoll && transport == RuntimeTransportWebSocket {
		return RuntimeTransportReasonRecovery
	}
	return RuntimeTransportReasonPolicyForced
}

// resolveRuntimeTransportReason treats the SDK value as an untrusted bounded
// hint. Core owns the observed HTTP/WS entry, prior durable attachment and
// current allowlist, and persists the hint only when all three agree.
func resolveRuntimeTransportReason(
	transport RuntimeTransport,
	previous RuntimeTransport,
	reported RuntimeTransportReason,
	policy RuntimeTransportPolicy,
) (RuntimeTransportReason, bool) {
	policy = effectiveRuntimeTransportPolicy(policy)
	if !runtimeTransportAllowed(policy, transport) ||
		(previous != "" && previous != RuntimeTransportUnknown && !previous.IsLive()) {
		return "", false
	}
	if reported == "" {
		return runtimeTransportReasonForAttachment(transport, previous), true
	}
	if !reported.IsValid() {
		return "", false
	}

	switch reported {
	case RuntimeTransportReasonExplicit:
		return reported, true
	case RuntimeTransportReasonWebSocketUnavailable:
		if transport == RuntimeTransportLongPoll &&
			runtimeTransportAllowed(policy, RuntimeTransportWebSocket) {
			return reported, true
		}
	case RuntimeTransportReasonPolicyForced:
		if transport == runtimePolicySelectedTransport(policy) {
			return reported, true
		}
	case RuntimeTransportReasonRecovery:
		if transport == RuntimeTransportWebSocket && previous == RuntimeTransportLongPoll {
			return reported, true
		}
	}
	return "", false
}
