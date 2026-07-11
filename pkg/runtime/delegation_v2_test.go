package runtime

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRuntimeV2DelegationRequiresMatchingDomainCapabilities(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	contextValue, token, err := signer.Issue(capability)
	require.NoError(t, err)

	verified, err := verifyRuntimeV2DelegationCapabilityPair(
		signer, contextValue, token, capability.IssuedAt.Add(10),
	)
	require.NoError(t, err)
	require.True(t, runtimeV2InvocationCapabilitiesEqual(capability, verified))

	other := capability
	other.AttemptID = uuid.New()
	otherContext, _, err := signer.Issue(other)
	require.NoError(t, err)
	_, err = verifyRuntimeV2DelegationCapabilityPair(
		signer, otherContext, token, capability.IssuedAt.Add(10),
	)
	require.ErrorIs(t, err, ErrInvalidRuntimeInvocation)

	_, err = verifyRuntimeV2DelegationCapabilityPair(
		signer, contextValue, token, capability.ExpiresAt,
	)
	require.ErrorIs(t, err, ErrExpiredRuntimeInvocation)
}

func TestRuntimeV2DelegationAuthorizationBindsExactRequest(t *testing.T) {
	signer, err := NewRuntimeInvocationSigner(runtimeInvocationTestSecret)
	require.NoError(t, err)
	capability := runtimeInvocationCapabilityFixture()
	contextValue, token, err := signer.Issue(capability)
	require.NoError(t, err)
	body := []byte(`{"target_agent_id":"` + uuid.NewString() + `","input":{"q":"hi"}}`)
	proofRequest := RuntimeInvocationProofRequest{
		Method:         http.MethodPost,
		Path:           runtimeV2CallAgentPath,
		IdempotencyKey: "delegate-once",
		Context:        contextValue,
		Body:           body,
	}
	proof, err := BuildRuntimeInvocationProof(token, proofRequest)
	require.NoError(t, err)
	authorization := RuntimeV2DelegationAuthorization{
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
	require.True(t, validRuntimeV2DelegationAuthorization(authorization))
	require.NoError(t, VerifyRuntimeInvocationProof(token, proof, proofRequest))

	mutations := []func(*RuntimeV2DelegationAuthorization){
		func(value *RuntimeV2DelegationAuthorization) { value.ProofRequest.Path += "/other" },
		func(value *RuntimeV2DelegationAuthorization) { value.ProofRequest.Method = http.MethodPut },
		func(value *RuntimeV2DelegationAuthorization) { value.ProofRequest.Context += "x" },
		func(value *RuntimeV2DelegationAuthorization) { value.ProofRequest.IdempotencyKey += "x" },
		func(value *RuntimeV2DelegationAuthorization) { value.ProofRequest.Body = nil },
		func(value *RuntimeV2DelegationAuthorization) { value.Device.NodeID = uuid.Nil },
	}
	for _, mutate := range mutations {
		changed := authorization
		changed.ProofRequest.Body = append([]byte(nil), authorization.ProofRequest.Body...)
		mutate(&changed)
		require.False(t, validRuntimeV2DelegationAuthorization(changed))
	}
}
