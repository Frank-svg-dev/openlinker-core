package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

const (
	DefaultRuntimeHTTPRequestsPerSecond      = 100
	DefaultRuntimeHTTPBurst                  = 200
	DefaultRuntimeWebSocketMessagesPerSecond = 200
	DefaultRuntimeWebSocketMessageBurst      = 400
	DefaultRuntimeWebSocketsPerIdentity      = 16

	defaultRuntimeAdmissionIdentityTTL = 10 * time.Minute
	defaultRuntimeAdmissionSweepPeriod = time.Minute
)

// RuntimeAdmissionIdentity contains only durable, non-secret identifiers.
// The limiter hashes this value before it is used as an internal key; Agent
// Token plaintext and certificate material are never accepted by this API.
type RuntimeAdmissionIdentity struct {
	AgentID uuid.UUID
	NodeID  uuid.UUID
}

// RuntimeAdmissionLimiter applies authenticated, per-principal admission to
// Runtime HTTP requests and WebSocket traffic. Implementations must not derive
// keys from request source IP because the raw mTLS edge intentionally preserves
// TLS bytes, not the downstream socket address.
type RuntimeAdmissionLimiter interface {
	AllowHTTP(RuntimeAdmissionIdentity) bool
	AcquireWebSocket(RuntimeAdmissionIdentity) (release func(), allowed bool)
	AllowWebSocketMessage(RuntimeAdmissionIdentity) bool
}

type RuntimeAdmissionLimitConfig struct {
	HTTPRequestsPerSecond      int
	HTTPBurst                  int
	WebSocketMessagesPerSecond int
	WebSocketMessageBurst      int
	MaxWebSocketsPerIdentity   int
}

type runtimeAdmissionLimiter struct {
	mu            sync.Mutex
	config        RuntimeAdmissionLimitConfig
	identities    map[string]*runtimeAdmissionState
	lastSweep     time.Time
	now           func() time.Time
	identityTTL   time.Duration
	sweepInterval time.Duration
}

type runtimeAdmissionState struct {
	http              *rate.Limiter
	webSocketMessages *rate.Limiter
	webSockets        int
	lastSeen          time.Time
}

func NewRuntimeAdmissionLimiter(config RuntimeAdmissionLimitConfig) RuntimeAdmissionLimiter {
	config = effectiveRuntimeAdmissionLimitConfig(config)
	return &runtimeAdmissionLimiter{
		config:        config,
		identities:    make(map[string]*runtimeAdmissionState),
		now:           time.Now,
		identityTTL:   defaultRuntimeAdmissionIdentityTTL,
		sweepInterval: defaultRuntimeAdmissionSweepPeriod,
	}
}

func effectiveRuntimeAdmissionLimitConfig(config RuntimeAdmissionLimitConfig) RuntimeAdmissionLimitConfig {
	if config.HTTPRequestsPerSecond <= 0 {
		config.HTTPRequestsPerSecond = DefaultRuntimeHTTPRequestsPerSecond
	}
	if config.HTTPBurst <= 0 {
		config.HTTPBurst = DefaultRuntimeHTTPBurst
	}
	if config.WebSocketMessagesPerSecond <= 0 {
		config.WebSocketMessagesPerSecond = DefaultRuntimeWebSocketMessagesPerSecond
	}
	if config.WebSocketMessageBurst <= 0 {
		config.WebSocketMessageBurst = DefaultRuntimeWebSocketMessageBurst
	}
	if config.MaxWebSocketsPerIdentity <= 0 {
		config.MaxWebSocketsPerIdentity = DefaultRuntimeWebSocketsPerIdentity
	}
	return config
}

func runtimeAdmissionIdentityFromPrincipal(principal AuthenticatedRuntimePrincipal) RuntimeAdmissionIdentity {
	return RuntimeAdmissionIdentity{
		AgentID: principal.AgentID,
		NodeID:  principal.Device.NodeID,
	}
}

func runtimeAdmissionIdentityFromDevice(device RuntimeDeviceIdentity) RuntimeAdmissionIdentity {
	return RuntimeAdmissionIdentity{NodeID: device.NodeID}
}

func (identity RuntimeAdmissionIdentity) valid() bool {
	// Delegation requests authenticate a Node plus short-lived capabilities and
	// therefore deliberately use a Node-only bucket. Agent Runtime requests add
	// the durable Agent ID. Agent Token rotation and Session churn cannot create
	// fresh buckets because neither credential nor request-body IDs are keys.
	return identity.NodeID != uuid.Nil
}

func runtimeAdmissionIdentityKey(identity RuntimeAdmissionIdentity) (string, bool) {
	if !identity.valid() {
		return "", false
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("openlinker-runtime-admission-v1\x00"))
	_, _ = hash.Write(identity.AgentID[:])
	_, _ = hash.Write(identity.NodeID[:])
	return hex.EncodeToString(hash.Sum(nil)), true
}

func (limiter *runtimeAdmissionLimiter) AllowHTTP(identity RuntimeAdmissionIdentity) bool {
	if limiter == nil {
		return false
	}
	key, ok := runtimeAdmissionIdentityKey(identity)
	if !ok {
		return false
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.currentTime()
	state := limiter.stateLocked(key, now)
	return state.http.AllowN(now, 1)
}

func (limiter *runtimeAdmissionLimiter) AcquireWebSocket(identity RuntimeAdmissionIdentity) (func(), bool) {
	if limiter == nil {
		return func() {}, false
	}
	key, ok := runtimeAdmissionIdentityKey(identity)
	if !ok {
		return func() {}, false
	}
	limiter.mu.Lock()
	now := limiter.currentTime()
	state := limiter.stateLocked(key, now)
	if state.webSockets >= limiter.config.MaxWebSocketsPerIdentity {
		limiter.mu.Unlock()
		return func() {}, false
	}
	state.webSockets++
	limiter.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			limiter.mu.Lock()
			if current := limiter.identities[key]; current != nil {
				if current.webSockets > 0 {
					current.webSockets--
				}
				current.lastSeen = limiter.currentTime()
			}
			limiter.mu.Unlock()
		})
	}, true
}

func (limiter *runtimeAdmissionLimiter) AllowWebSocketMessage(identity RuntimeAdmissionIdentity) bool {
	if limiter == nil {
		return false
	}
	key, ok := runtimeAdmissionIdentityKey(identity)
	if !ok {
		return false
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.currentTime()
	state := limiter.stateLocked(key, now)
	return state.webSocketMessages.AllowN(now, 1)
}

func (limiter *runtimeAdmissionLimiter) stateLocked(key string, now time.Time) *runtimeAdmissionState {
	limiter.sweepLocked(now)
	state := limiter.identities[key]
	if state == nil {
		state = &runtimeAdmissionState{
			http: rate.NewLimiter(
				rate.Limit(limiter.config.HTTPRequestsPerSecond), limiter.config.HTTPBurst,
			),
			webSocketMessages: rate.NewLimiter(
				rate.Limit(limiter.config.WebSocketMessagesPerSecond), limiter.config.WebSocketMessageBurst,
			),
		}
		limiter.identities[key] = state
	}
	state.lastSeen = now
	return state
}

func (limiter *runtimeAdmissionLimiter) sweepLocked(now time.Time) {
	if !limiter.lastSweep.IsZero() && now.Sub(limiter.lastSweep) < limiter.sweepInterval {
		return
	}
	limiter.lastSweep = now
	for key, state := range limiter.identities {
		if state.webSockets == 0 && now.Sub(state.lastSeen) >= limiter.identityTTL {
			delete(limiter.identities, key)
		}
	}
}

func (limiter *runtimeAdmissionLimiter) currentTime() time.Time {
	if limiter.now == nil {
		return time.Now()
	}
	return limiter.now()
}
