package runtime

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRuntimeCredentialRevocationScopesAreUniqueAndOrdered(t *testing.T) {
	nodeA, nodeB := uuid.New(), uuid.New()
	coreA, coreB := uuid.New(), uuid.New()
	if nodeA.String() > nodeB.String() {
		nodeA, nodeB = nodeB, nodeA
	}
	if coreA.String() > coreB.String() {
		coreA, coreB = coreB, coreA
	}
	sessions := []lockedRuntimeCredentialSession{
		{runtimeSessionID: uuid.New(), nodeID: nodeB, coreInstanceID: &coreB},
		{runtimeSessionID: uuid.New(), nodeID: nodeA, coreInstanceID: &coreA},
		{runtimeSessionID: uuid.New(), nodeID: nodeB, coreInstanceID: &coreB},
		{runtimeSessionID: uuid.New(), nodeID: nodeA},
	}

	require.Equal(t, []uuid.UUID{nodeA, nodeB}, runtimeCredentialNodeIDs(sessions))
	require.Equal(t, []uuid.UUID{coreA, coreB}, runtimeCredentialCoreIDs(sessions))
}
