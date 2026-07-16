package runtime

const runtimePreviousContractDigest = "fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53"

// This is a published support commitment, not a rolling process clock. A new
// release may move the fixed date deliberately; discovery must never derive it
// from time.Now because that would make compatibility promises drift.
const runtimePreviousSupportedUntil = "2027-01-31T00:00:00Z"

type runtimeWireGeneration struct {
	digest                       string
	requiresAttachmentGeneration bool
}

// runtimeSupportedWireGenerations is the bounded Server compatibility ring.
// It is deliberately internal: public discovery keeps one canonical Runtime
// name and URL while Core accepts only the current and immediately preceding
// wire generations. Database schema generations are tracked independently.
var runtimeSupportedWireGenerations = [...]runtimeWireGeneration{
	{digest: RuntimeContractDigest, requiresAttachmentGeneration: true},
	{digest: runtimePreviousContractDigest, requiresAttachmentGeneration: false},
}

// RuntimeWireCompatibilitySnapshot is a read-only copy of Core's bounded wire
// compatibility ring for public discovery. Adapter details remain internal.
type RuntimeWireCompatibilitySnapshot struct {
	CurrentContractDigest     string
	SupportedContractDigests  []string
	PreviousSupportedUntilRFC string
}

func CurrentRuntimeWireCompatibility() RuntimeWireCompatibilitySnapshot {
	digests := make([]string, len(runtimeSupportedWireGenerations))
	for index, generation := range runtimeSupportedWireGenerations {
		digests[index] = generation.digest
	}
	return RuntimeWireCompatibilitySnapshot{
		CurrentContractDigest:     RuntimeContractDigest,
		SupportedContractDigests:  digests,
		PreviousSupportedUntilRFC: runtimePreviousSupportedUntil,
	}
}

func runtimeWireGenerationForDigest(digest string) (runtimeWireGeneration, bool) {
	for _, generation := range runtimeSupportedWireGenerations {
		if constantTimeStringEqual(generation.digest, digest) {
			return generation, true
		}
	}
	return runtimeWireGeneration{}, false
}

func runtimeWireContractSupported(digest string) bool {
	_, ok := runtimeWireGenerationForDigest(digest)
	return ok
}

func runtimeWireContractAllowsMissingAttachment(digest string) bool {
	generation, ok := runtimeWireGenerationForDigest(digest)
	return ok && !generation.requiresAttachmentGeneration
}
