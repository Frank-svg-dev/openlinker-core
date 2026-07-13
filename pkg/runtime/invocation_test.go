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
	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
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

	other, err := NewRuntimeInvocationSigner("different-runtime-signing-secret-00000000")
	require.NoError(t, err)
	_, err = other.VerifyInvocationToken(tokenA, databaseNow)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
}

func TestRuntimeInvocationCapabilityRejectsTamperAndDatabaseExpiry(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	_, token, err := signer.Issue(capability)
	require.NoError(t, err)

	parts := strings.Split(token, ".")
	require.Len(t, parts, 4)
	if parts[3][0] == 'A' {
		parts[3] = "B" + parts[3][1:]
	} else {
		parts[3] = "A" + parts[3][1:]
	}
	tampered := strings.Join(parts, ".")
	_, err = signer.VerifyInvocationToken(tampered, capability.IssuedAt.Add(time.Second))
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
	_, err = signer.VerifyInvocationToken(token, capability.IssuedAt.Add(-time.Nanosecond))
	require.ErrorIs(t, err, ErrExpiredRuntimeInvocation)
	_, err = signer.VerifyInvocationToken(token, capability.ExpiresAt)
	require.ErrorIs(t, err, ErrExpiredRuntimeInvocation)

	payload, err := canonicalRuntimeInvocationCapability(capability)
	require.NoError(t, err)
	payload = []byte(strings.Replace(string(payload), runtimeInvocationAudience, "openlinker.runtime.v2/other", 1))
	key := signer.tokenKeys[signer.activeKeyID]
	wrongAudience := encodeSignedRuntimeCapability(runtimeInvocationTokenPrefix, signer.activeKeyID, payload, key[:])
	_, err = signer.VerifyInvocationToken(wrongAudience, capability.IssuedAt.Add(time.Second))
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
}

func TestRuntimeInvocationKeyRotationRetainsOnlyConfiguredPredecessors(t *testing.T) {
	t.Parallel()

	oldSecret := "old-runtime-signing-secret-0000000000"
	newSecret := "new-runtime-signing-secret-0000000000"
	oldSigner, err := NewRuntimeInvocationSignerKeyring("old", map[string]string{"old": oldSecret})
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	oldContext, oldToken, err := oldSigner.Issue(capability)
	require.NoError(t, err)

	rotating, err := NewRuntimeInvocationSignerKeyring("new", map[string]string{
		"new": newSecret,
		"old": oldSecret,
	})
	require.NoError(t, err)
	databaseNow := capability.IssuedAt.Add(time.Second)
	_, err = rotating.VerifyNodeEnvelope(oldContext, databaseNow)
	require.NoError(t, err)
	_, err = rotating.VerifyInvocationToken(oldToken, databaseNow)
	require.NoError(t, err)
	_, newToken, err := rotating.Issue(capability)
	require.NoError(t, err)
	require.Contains(t, newToken, ".new.")

	retired, err := NewRuntimeInvocationSignerKeyring("new", map[string]string{"new": newSecret})
	require.NoError(t, err)
	_, err = retired.VerifyInvocationToken(oldToken, databaseNow)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
	_, err = oldSigner.VerifyInvocationToken(newToken, databaseNow)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)

	configured, err := NewRuntimeInvocationSignerWithPrevious("new", newSecret, "old", oldSecret)
	require.NoError(t, err)
	_, err = configured.VerifyInvocationToken(oldToken, databaseNow)
	require.NoError(t, err)
	_, err = NewRuntimeInvocationSignerWithPrevious("new", newSecret, "old", "")
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
	_, err = NewRuntimeInvocationSignerWithPrevious("new", newSecret, "new", oldSecret)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
}

func TestRuntimeInvocationProofBindsContextMethodPathKeyAndBody(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	contextValue, token, err := signer.Issue(capability)
	require.NoError(t, err)

	request := RuntimeInvocationProofRequest{
		Method:         "POST",
		Path:           "/api/v1/agent-runtime/call-agent",
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
	_, err = NewRuntimeInvocationSigner("too-short")
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
	_, err = NewRuntimeInvocationSigner(" " + runtimeInvocationTestSecret)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
	_, err = NewRuntimeInvocationSignerKeyring("bad.key", map[string]string{"bad.key": runtimeInvocationTestSecret})
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
	require.NoError(t, err)
	invalid := runtimeInvocationCapabilityFixture()
	invalid.RunID = uuid.Nil
	_, _, err = signer.Issue(invalid)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)

	_, err = signer.VerifyInvocationToken("garbage", time.Now())
	require.True(t, errors.Is(err, ErrInvalidRuntimeInvocation))
}

func TestRuntimeInvocationWorkerLimitCountsRunes(t *testing.T) {
	t.Parallel()

	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	capability.WorkerID = strings.Repeat("节", 200)
	_, _, err = signer.Issue(capability)
	require.NoError(t, err)
	capability.WorkerID += "点"
	_, _, err = signer.Issue(capability)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)
}

const runtimeInvocationTestSecret = "test-runtime-signing-secret-00000000"

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
