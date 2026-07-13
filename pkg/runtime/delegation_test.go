package runtime

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRuntimeDelegationRequiresMatchingDomainCapabilities(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	contextValue, token, err := signer.Issue(capability)
	require.NoError(t, err)

	verified, err := verifyRuntimeDelegationCapabilityPair(
		signer, contextValue, token, capability.IssuedAt.Add(10),
	)
	require.NoError(t, err)
	require.True(t, runtimeInvocationCapabilitiesEqual(capability, verified))

	other := capability
	other.AttemptID = uuid.New()
	otherContext, _, err := signer.Issue(other)
	require.NoError(t, err)
	_, err = verifyRuntimeDelegationCapabilityPair(
		signer, otherContext, token, capability.IssuedAt.Add(10),
	)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)

	_, err = verifyRuntimeDelegationCapabilityPair(
		signer, contextValue, token, capability.ExpiresAt,
	)
	require.ErrorIs(t, err, ErrExpiredRuntimeInvocation)
}

func TestRuntimeDelegationAuthorizationBindsExactRequest(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	contextValue, token, err := signer.Issue(capability)
	require.NoError(t, err)
	body := []byte(`{"target_agent_id":"` + uuid.NewString() + `","input":{"q":"hi"}}`)
	proofRequest := RuntimeInvocationProofRequest{
		Method:         http.MethodPost,
		Path:           runtimeCallAgentPath,
		IdempotencyKey: "delegate-once",
		Context:        contextValue,
		Body:           body,
	}
	proof, err := BuildRuntimeInvocationProof(token, proofRequest)
	require.NoError(t, err)
	authorization := RuntimeDelegationAuthorization{
		Device: RuntimeDeviceIdentity{
			NodeID:                       capability.NodeID,
			CertificateSerial:            "abc",
			CertificateFingerprintSHA256: strings.Repeat("a", 64),
			PublicKeyThumbprintSHA256:    strings.Repeat("b", 64),
		},
		InvocationContext: contextValue,
		InvocationToken:   token,
		InvocationProof:   proof,
		IdempotencyKey:    proofRequest.IdempotencyKey,
		ProofRequest:      proofRequest,
	}
	require.True(t, validRuntimeDelegationAuthorization(authorization))
	require.NoError(t, VerifyRuntimeInvocationProof(token, proof, proofRequest))

	mutations := []func(*RuntimeDelegationAuthorization){
		func(value *RuntimeDelegationAuthorization) { value.ProofRequest.Path += "/other" },
		func(value *RuntimeDelegationAuthorization) { value.ProofRequest.Method = http.MethodPut },
		func(value *RuntimeDelegationAuthorization) { value.ProofRequest.Context += "x" },
		func(value *RuntimeDelegationAuthorization) { value.ProofRequest.IdempotencyKey += "x" },
		func(value *RuntimeDelegationAuthorization) { value.ProofRequest.Body = nil },
		func(value *RuntimeDelegationAuthorization) { value.Device.NodeID = uuid.Nil },
	}
	for _, mutate := range mutations {
		changed := authorization
		changed.ProofRequest.Body = append([]byte(nil), authorization.ProofRequest.Body...)
		mutate(&changed)
		require.False(t, validRuntimeDelegationAuthorization(changed))
	}
}
