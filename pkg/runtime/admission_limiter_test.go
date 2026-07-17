package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestRuntimeAdmissionLimiterUsesIndependentHashedPrincipalBuckets(t *testing.T) {
	limiter := NewRuntimeAdmissionLimiter(RuntimeAdmissionLimitConfig{
		HTTPRequestsPerSecond:      1,
		HTTPBurst:                  1,
		WebSocketMessagesPerSecond: 1,
		WebSocketMessageBurst:      1,
		MaxWebSocketsPerIdentity:   1,
	})
	first := RuntimeAdmissionIdentity{AgentID: uuid.New(), NodeID: uuid.New()}
	second := RuntimeAdmissionIdentity{AgentID: uuid.New(), NodeID: uuid.New()}

	require.True(t, limiter.AllowHTTP(first))
	require.False(t, limiter.AllowHTTP(first))
	require.True(t, limiter.AllowHTTP(second), "one principal must not consume another principal's bucket")
	require.True(t, limiter.AllowWebSocketMessage(first))
	require.False(t, limiter.AllowWebSocketMessage(first))

	release, allowed := limiter.AcquireWebSocket(first)
	require.True(t, allowed)
	_, allowed = limiter.AcquireWebSocket(first)
	require.False(t, allowed)
	release()
	release() // release is intentionally idempotent
	releaseAgain, allowed := limiter.AcquireWebSocket(first)
	require.True(t, allowed)
	releaseAgain()
}

func TestRuntimeAdmissionIdentityKeyIsStableOpaqueAndContainsNoCredentialMaterial(t *testing.T) {
	identity := RuntimeAdmissionIdentity{AgentID: uuid.New(), NodeID: uuid.New()}
	key, ok := runtimeAdmissionIdentityKey(identity)
	require.True(t, ok)
	require.Len(t, key, sha256HexLength)
	require.NotContains(t, key, identity.AgentID.String())
	require.NotContains(t, key, identity.NodeID.String())
	repeated, ok := runtimeAdmissionIdentityKey(identity)
	require.True(t, ok)
	require.Equal(t, key, repeated)

	nodeOnly, ok := runtimeAdmissionIdentityKey(RuntimeAdmissionIdentity{NodeID: identity.NodeID})
	require.True(t, ok)
	require.NotEqual(t, key, nodeOnly)
	_, ok = runtimeAdmissionIdentityKey(RuntimeAdmissionIdentity{AgentID: identity.AgentID})
	require.False(t, ok)
}

func TestRuntimeAuthenticatedLimiterCoversPullHeartbeatResultAndDelegationHTTP(t *testing.T) {
	fixture := newRuntimeHandlerFixture()
	limiter := &runtimeAdmissionLimiterFake{}
	controller := runtimeHTTPControllerWithAdmission(fixture, limiter)

	for _, test := range []struct {
		name   string
		method string
		target string
	}{
		{name: "pull", method: http.MethodPost, target: "/api/v1/agent-runtime/runs/claim"},
		{name: "heartbeat", method: http.MethodPost, target: "/api/v1/agent-runtime/sessions/" + fixture.acting.RuntimeSessionID.String() + "/heartbeat"},
		{name: "result", method: http.MethodPost, target: "/api/v1/agent-runtime/runs/" + uuid.NewString() + "/result"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := serveRuntimeRaw(t, controller, test.method, test.target, `{}`)
			require.Equal(t, http.StatusTooManyRequests, recorder.Code, recorder.Body.String())
			requireRuntimeResponseCode(t, recorder, RuntimeErrorRateLimited)
		})
	}
	require.Zero(t, fixture.leases.claimCalls)
	require.Zero(t, fixture.sessions.createCalls)
	require.Equal(t, 3, limiter.httpCount())
	for _, identity := range limiter.httpIdentities() {
		require.Equal(t, runtimeAdmissionIdentityFromPrincipal(fixture.authenticated), identity)
	}

	delegationLimiter := &runtimeAdmissionLimiterFake{}
	delegationController := runtimeHTTPControllerWithAdmission(fixture, delegationLimiter)
	recorder := serveRuntimeRaw(
		t, delegationController, http.MethodPost, runtimeCallAgentPath, `{}`,
	)
	require.Equal(t, http.StatusTooManyRequests, recorder.Code, recorder.Body.String())
	requireRuntimeResponseCode(t, recorder, RuntimeErrorRateLimited)
	require.Zero(t, fixture.delegation.calls)
	require.Equal(t, []RuntimeAdmissionIdentity{{NodeID: fixture.authenticated.Device.NodeID}}, delegationLimiter.httpIdentities())
}

func TestRuntimeWebSocketAdmissionLimitsConnectionsAndMessagesWithoutChangingWireErrors(t *testing.T) {
	fixture := newRuntimeWSTestFixture()
	connectionLimiter := &runtimeAdmissionLimiterFake{allowHTTP: true}
	connectionController := runtimeWSControllerWithAdmission(fixture, connectionLimiter)
	connectionServer, connectionTarget := runtimeWSServerForController(t, connectionController)
	defer connectionServer.Close()

	conn, response, err := websocket.DefaultDialer.Dial(connectionTarget, http.Header{
		echo.HeaderAuthorization: []string{"Bearer runtime-secret"},
	})
	require.Error(t, err)
	require.Nil(t, conn)
	require.NotNil(t, response)
	defer response.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode)
	body, readErr := io.ReadAll(response.Body)
	require.NoError(t, readErr)
	var runtimeErr RuntimeError
	require.NoError(t, json.Unmarshal(body, &runtimeErr), string(body))
	require.Equal(t, RuntimeErrorRateLimited, runtimeErr.Error.Code)
	require.Zero(t, fixture.sessions.createCount)

	messageLimiter := &runtimeAdmissionLimiterFake{
		allowHTTP:      true,
		allowWebSocket: true,
	}
	messageController := runtimeWSControllerWithAdmission(fixture, messageLimiter)
	messageServer, messageTarget := runtimeWSServerForController(t, messageController)
	defer messageServer.Close()
	messageConn := dialRuntimeWS(t, messageTarget)
	defer messageConn.Close()
	writeRuntimeWSHello(t, messageConn, fixture.hello)
	require.Equal(t, RuntimeMessageReady, readRuntimeWSEnvelope(t, messageConn).Type)

	deadline := fixture.now.Add(time.Minute)
	request, requestEnvelope, err := newRuntimeWSTypedMessage(
		RuntimeMessageDrain, nil, RuntimeDrainPayload{
			DeadlineAt: deadline,
			ReasonCode: "SDK_SHUTDOWN",
			Capacity:   0,
		},
	)
	require.NoError(t, err)
	require.NoError(t, messageConn.WriteJSON(request))
	reply := readRuntimeWSEnvelope(t, messageConn)
	require.Equal(t, RuntimeMessageError, reply.Type)
	require.NoError(t, ValidateRuntimeReplyCorrelation(requestEnvelope, reply))
	rateError, err := DecodeRuntimeMessagePayload[RuntimeErrorBody](reply, RuntimeMessageError)
	require.NoError(t, err)
	require.Equal(t, RuntimeErrorRateLimited, rateError.Code)
	require.True(t, rateError.Retryable)
	fixture.sessions.mu.Lock()
	require.Equal(t, uuid.Nil, fixture.sessions.drainRequest.RuntimeSessionID)
	fixture.sessions.mu.Unlock()

	// RATE_LIMITED is a normal correlated Runtime error, not a protocol close.
	messageLimiter.setAllowMessages(true)
	require.NoError(t, messageConn.WriteJSON(request))
	reply = readRuntimeWSEnvelope(t, messageConn)
	require.Equal(t, RuntimeMessageDrain, reply.Type)
	require.NoError(t, ValidateRuntimeReplyCorrelation(requestEnvelope, reply))
	require.Equal(t, 2, messageLimiter.messageCount())
}

const sha256HexLength = 64

func runtimeHTTPControllerWithAdmission(
	fixture *runtimeHandlerFixture,
	limiter RuntimeAdmissionLimiter,
) *RuntimeHTTPController {
	return NewRuntimeHTTPController(RuntimeHTTPDependencies{
		TokenValidator:      fixture.tokens,
		DeviceAuthenticator: fixture.devices,
		Sessions:            fixture.sessions,
		Leases:              fixture.leases,
		EventProjector:      fixture.events,
		Finalizer:           fixture.finalizer,
		Resume:              fixture.resume,
		Delegation:          fixture.delegation,
		Cancellations:       fixture.cancellations,
		AdmissionLimiter:    limiter,
	})
}

func runtimeWSControllerWithAdmission(
	fixture *runtimeWSTestFixture,
	limiter RuntimeAdmissionLimiter,
) *RuntimeHTTPController {
	return NewRuntimeHTTPController(RuntimeHTTPDependencies{
		TokenValidator:      fixture.tokens,
		DeviceAuthenticator: fixture.devices,
		TransportPolicy:     fixture.transportPolicy,
		Sessions:            fixture.sessions,
		Leases:              fixture.leases,
		EventProjector:      fixture.events,
		Finalizer:           fixture.finalizer,
		Resume:              fixture.resume,
		Cancellations:       fixture.cancellations,
		WakeHub:             fixture.wakeHub,
		AdmissionLimiter:    limiter,
	})
}

func runtimeWSServerForController(
	t *testing.T,
	controller *RuntimeHTTPController,
) (*httptest.Server, string) {
	t.Helper()
	e := echo.New()
	controller.Register(e.Group("/api/v1"))
	server := httptest.NewServer(e)
	return server, "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/agent-runtime/ws"
}

type runtimeAdmissionLimiterFake struct {
	mu             sync.Mutex
	allowHTTP      bool
	allowWebSocket bool
	allowMessages  bool
	http           []RuntimeAdmissionIdentity
	webSockets     []RuntimeAdmissionIdentity
	messages       []RuntimeAdmissionIdentity
	releases       int
}

func (limiter *runtimeAdmissionLimiterFake) AllowHTTP(identity RuntimeAdmissionIdentity) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.http = append(limiter.http, identity)
	return limiter.allowHTTP
}

func (limiter *runtimeAdmissionLimiterFake) AcquireWebSocket(identity RuntimeAdmissionIdentity) (func(), bool) {
	limiter.mu.Lock()
	limiter.webSockets = append(limiter.webSockets, identity)
	allowed := limiter.allowWebSocket
	limiter.mu.Unlock()
	if !allowed {
		return func() {}, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			limiter.mu.Lock()
			limiter.releases++
			limiter.mu.Unlock()
		})
	}, true
}

func (limiter *runtimeAdmissionLimiterFake) AllowWebSocketMessage(identity RuntimeAdmissionIdentity) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.messages = append(limiter.messages, identity)
	return limiter.allowMessages
}

func (limiter *runtimeAdmissionLimiterFake) setAllowMessages(allowed bool) {
	limiter.mu.Lock()
	limiter.allowMessages = allowed
	limiter.mu.Unlock()
}

func (limiter *runtimeAdmissionLimiterFake) httpCount() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return len(limiter.http)
}

func (limiter *runtimeAdmissionLimiterFake) httpIdentities() []RuntimeAdmissionIdentity {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return append([]RuntimeAdmissionIdentity(nil), limiter.http...)
}

func (limiter *runtimeAdmissionLimiterFake) messageCount() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return len(limiter.messages)
}

var _ RuntimeAdmissionLimiter = (*runtimeAdmissionLimiterFake)(nil)
