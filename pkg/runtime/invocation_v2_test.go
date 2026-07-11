package runtime

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRuntimeInvocationCapabilityIsDeterministicAndDomainSeparated(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner("test-runtime-signing-secret")
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()

	contextA, tokenA, err := signer.Issue(capability)
	require.NoError(t, err)
	contextB, tokenB, err := signer.Issue(capability)
	require.NoError(t, err)
	require.Equal(t, contextA, contextB)
	require.Equal(t, tokenA, tokenB)
	require.NotEqual(t, contextA, tokenA)

	databaseNow := capability.IssuedAt.Add(time.Second)
	gotContext, err := signer.VerifyNodeEnvelope(contextA, databaseNow)
	require.NoError(t, err)
	gotToken, err := signer.VerifyInvocationToken(tokenA, databaseNow)
	require.NoError(t, err)
	require.Equal(t, capability, gotContext)
	require.Equal(t, capability, gotToken)

	other, err := NewRuntimeInvocationSigner("different-runtime-signing-secret")
	require.NoError(t, err)
	_, err = other.VerifyInvocationToken(tokenA, databaseNow)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
}

func TestRuntimeInvocationCapabilityRejectsTamperAndDatabaseExpiry(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner("test-runtime-signing-secret")
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	_, token, err := signer.Issue(capability)
	require.NoError(t, err)

	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	tampered := strings.Join(parts, ".")
	_, err = signer.VerifyInvocationToken(tampered, capability.IssuedAt.Add(time.Second))
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
	_, err = signer.VerifyInvocationToken(token, capability.IssuedAt.Add(-time.Nanosecond))
	require.ErrorIs(t, err, ErrExpiredRuntimeInvocation)
	_, err = signer.VerifyInvocationToken(token, capability.ExpiresAt)
	require.ErrorIs(t, err, ErrExpiredRuntimeInvocation)
}

func TestRuntimeInvocationProofBindsContextMethodPathKeyAndBody(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner("test-runtime-signing-secret")
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	contextValue, token, err := signer.Issue(capability)
	require.NoError(t, err)

	request := RuntimeInvocationProofRequest{
		Method:         "POST",
		Path:           "/api/v1/agent-runtime/v2/call-agent",
		IdempotencyKey: "delegation-42",
		Context:        contextValue,
		Body:           []byte(`{"target_agent_id":"` + uuid.NewString() + `"}`),
	}
	proof, err := BuildRuntimeInvocationProof(token, request)
	require.NoError(t, err)
	require.NoError(t, VerifyRuntimeInvocationProof(token, proof, request))

	mutations := []func(*RuntimeInvocationProofRequest){
		func(req *RuntimeInvocationProofRequest) { req.Method = "PUT" },
		func(req *RuntimeInvocationProofRequest) { req.Path += "/other" },
		func(req *RuntimeInvocationProofRequest) { req.IdempotencyKey += "-other" },
		func(req *RuntimeInvocationProofRequest) { req.IdempotencyKey = " " + req.IdempotencyKey + " " },
		func(req *RuntimeInvocationProofRequest) { req.Context += "x" },
		func(req *RuntimeInvocationProofRequest) { req.Body = append(req.Body, ' ') },
	}
	for _, mutate := range mutations {
		changed := request
		changed.Body = append([]byte(nil), request.Body...)
		mutate(&changed)
		require.ErrorIs(t, VerifyRuntimeInvocationProof(token, proof, changed), ErrInvalidRuntimeInvocation)
	}
}

func TestRuntimeInvocationCapabilityValidation(t *testing.T) {
	_, err := NewRuntimeInvocationSigner("  ")
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
	signer, err := NewRuntimeInvocationSigner("test-runtime-signing-secret")
	require.NoError(t, err)
	invalid := runtimeInvocationCapabilityFixture()
	invalid.RunID = uuid.Nil
	_, _, err = signer.Issue(invalid)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)

	_, err = signer.VerifyInvocationToken("garbage", time.Now())
	require.True(t, errors.Is(err, ErrInvalidRuntimeInvocation))
}

func runtimeInvocationCapabilityFixture() RuntimeInvocationCapability {
	issuedAt := time.Date(2026, 7, 11, 4, 0, 0, 123456789, time.UTC)
	return RuntimeInvocationCapability{
		RunID:            uuid.New(),
		AttemptID:        uuid.New(),
		LeaseID:          uuid.New(),
		FencingToken:     7,
		AgentID:          uuid.New(),
		CredentialID:     uuid.New(),
		NodeID:           uuid.New(),
		WorkerID:         "worker-a",
		RuntimeSessionID: uuid.New(),
		InputSHA256:      sha256.Sum256([]byte(`{"q":"hello"}`)),
		IssuedAt:         issuedAt,
		ExpiresAt:        issuedAt.Add(5 * time.Minute),
	}
}
