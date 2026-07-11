package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
)

func TestHashIdempotencyKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"a", " ", "run:create:01JTEST", strings.Repeat("x", 255)} {
		key := key
		t.Run("valid_"+strings.ReplaceAll(key[:1], " ", "space"), func(t *testing.T) {
			got, err := HashIdempotencyKey(key)
			if err != nil {
				t.Fatalf("HashIdempotencyKey() error = %v", err)
			}
			want := sha256.Sum256([]byte(key))
			if got != want {
				t.Fatalf("HashIdempotencyKey() = %x, want %x", got, want)
			}
		})
	}

	tests := []struct {
		name      string
		key       string
		wantClass IdempotencyErrorClass
	}{
		{name: "missing", key: "", wantClass: IdempotencyErrorKeyRequired},
		{name: "too long", key: strings.Repeat("x", 256), wantClass: IdempotencyErrorKeyInvalid},
		{name: "nul", key: "secret\x00key", wantClass: IdempotencyErrorKeyInvalid},
		{name: "unit separator", key: "secret\x1fkey", wantClass: IdempotencyErrorKeyInvalid},
		{name: "tab", key: "secret\tkey", wantClass: IdempotencyErrorKeyInvalid},
		{name: "newline", key: "secret\nkey", wantClass: IdempotencyErrorKeyInvalid},
		{name: "del", key: "secret\x7fkey", wantClass: IdempotencyErrorKeyInvalid},
		{name: "non ascii", key: "secret-密钥", wantClass: IdempotencyErrorKeyInvalid},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := HashIdempotencyKey(tt.key)
			if err == nil {
				t.Fatal("HashIdempotencyKey() error = nil")
			}
			class, ok := IdempotencyErrorClassOf(err)
			if !ok || class != tt.wantClass {
				t.Fatalf("IdempotencyErrorClassOf() = %q, %v; want %q, true", class, ok, tt.wantClass)
			}
			if tt.key != "" && strings.Contains(err.Error(), tt.key) {
				t.Fatalf("error leaked raw idempotency key: %q", err)
			}
		})
	}
}

func TestIdempotencyKeyReusedErrorIsStableAndSafe(t *testing.T) {
	t.Parallel()

	err := &IdempotencyError{Class: IdempotencyErrorKeyReused}
	class, ok := IdempotencyErrorClassOf(err)
	if !ok || class != IdempotencyErrorKeyReused {
		t.Fatalf("IdempotencyErrorClassOf() = %q, %v; want %q, true", class, ok, IdempotencyErrorKeyReused)
	}
	if got := err.Error(); got != "idempotency key was already used for a different request" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestCanonicalizeRFC8785OfficialExample(t *testing.T) {
	t.Parallel()

	const source = `{
		"numbers": [333333333.33333329, 1E30, 4.50,
		            2e-3, 0.000000000000000000000000001],
		"string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
		"literals": [null, true, false]
	}`
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode RFC 8785 example: %v", err)
	}

	got, err := CanonicalizeRFC8785(value)
	if err != nil {
		t.Fatalf("CanonicalizeRFC8785() error = %v", err)
	}
	// RFC 8785 section 3.2.4 publishes these exact UTF-8 bytes.
	const wantHex = "7b226c69746572616c73223a5b6e756c6c2c747275652c66616c73655d2c226e756d62657273223a5b3333333333333333332e333333333333332c31652b33302c342e352c302e3030322c31652d32375d2c22737472696e67223a22e282ac245c75303030665c6e4127425c225c5c5c5c5c222f227d"
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatalf("decode expected RFC bytes: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("CanonicalizeRFC8785() = %q, want %q", got, want)
	}
}

func TestCanonicalizeRFC8785UsesUTF16PropertyOrder(t *testing.T) {
	t.Parallel()

	value := map[string]any{
		"€":      "Euro Sign",
		"\r":     "Carriage Return",
		"דּ":      "Hebrew Letter Dalet With Dagesh",
		"1":      "One",
		"😀":      "Emoji: Grinning Face",
		"\u0080": "Control",
		"ö":      "Latin Small Letter O With Diaeresis",
	}
	got, err := CanonicalizeRFC8785(value)
	if err != nil {
		t.Fatalf("CanonicalizeRFC8785() error = %v", err)
	}
	want := "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"ö\":\"Latin Small Letter O With Diaeresis\",\"€\":\"Euro Sign\",\"😀\":\"Emoji: Grinning Face\",\"דּ\":\"Hebrew Letter Dalet With Dagesh\"}"
	if string(got) != want {
		t.Fatalf("CanonicalizeRFC8785() = %q, want %q", got, want)
	}

	// UTF-8 byte ordering would put U+E000 before U+10000. JCS compares the
	// latter's UTF-16 high surrogate (D800) and therefore puts it first.
	got, err = CanonicalizeRFC8785(map[string]any{"\ue000": 2, "\U00010000": 1})
	if err != nil {
		t.Fatalf("CanonicalizeRFC8785() supplementary key error = %v", err)
	}
	if want := "{\"\U00010000\":1,\"\ue000\":2}"; string(got) != want {
		t.Fatalf("supplementary key order = %q, want %q", got, want)
	}
}

func TestCanonicalizeRFC8785OfficialNumberVectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		bits uint64
		want string
	}{
		{bits: 0x0000000000000000, want: "0"},
		{bits: 0x8000000000000000, want: "0"},
		{bits: 0x0000000000000001, want: "5e-324"},
		{bits: 0x8000000000000001, want: "-5e-324"},
		{bits: 0x7fefffffffffffff, want: "1.7976931348623157e+308"},
		{bits: 0xffefffffffffffff, want: "-1.7976931348623157e+308"},
		{bits: 0x4340000000000000, want: "9007199254740992"},
		{bits: 0xc340000000000000, want: "-9007199254740992"},
		{bits: 0x4430000000000000, want: "295147905179352830000"},
		{bits: 0x44b52d02c7e14af5, want: "9.999999999999997e+22"},
		{bits: 0x44b52d02c7e14af6, want: "1e+23"},
		{bits: 0x44b52d02c7e14af7, want: "1.0000000000000001e+23"},
		{bits: 0x444b1ae4d6e2ef4e, want: "999999999999999700000"},
		{bits: 0x444b1ae4d6e2ef4f, want: "999999999999999900000"},
		{bits: 0x444b1ae4d6e2ef50, want: "1e+21"},
		{bits: 0x3eb0c6f7a0b5ed8c, want: "9.999999999999997e-7"},
		{bits: 0x3eb0c6f7a0b5ed8d, want: "0.000001"},
		{bits: 0x41b3de4355555553, want: "333333333.3333332"},
		{bits: 0x41b3de4355555554, want: "333333333.33333325"},
		{bits: 0x41b3de4355555555, want: "333333333.3333333"},
		{bits: 0x41b3de4355555556, want: "333333333.3333334"},
		{bits: 0x41b3de4355555557, want: "333333333.33333343"},
		{bits: 0xbecbf647612f3696, want: "-0.0000033333333333333333"},
		{bits: 0x43143ff3c1cb0959, want: "1424953923781206.2"},
	}
	for _, tt := range tests {
		got, err := CanonicalizeRFC8785(math.Float64frombits(tt.bits))
		if err != nil {
			t.Fatalf("bits %016x: CanonicalizeRFC8785() error = %v", tt.bits, err)
		}
		if string(got) != tt.want {
			t.Fatalf("bits %016x: CanonicalizeRFC8785() = %q, want %q", tt.bits, got, tt.want)
		}
	}
}

func TestCanonicalizeRFC8785MapOrderIsDeterministic(t *testing.T) {
	t.Parallel()

	left := map[string]any{
		"z": map[string]any{"b": 2, "a": 1},
		"a": []any{map[string]any{"d": 4, "c": 3}},
	}
	right := map[string]any{}
	right["a"] = []any{map[string]any{"c": 3, "d": 4}}
	right["z"] = map[string]any{"a": 1, "b": 2}

	leftCanonical, err := CanonicalizeRFC8785(left)
	if err != nil {
		t.Fatalf("canonicalize left: %v", err)
	}
	rightCanonical, err := CanonicalizeRFC8785(right)
	if err != nil {
		t.Fatalf("canonicalize right: %v", err)
	}
	if string(leftCanonical) != string(rightCanonical) {
		t.Fatalf("map order changed canonical JSON: %q != %q", leftCanonical, rightCanonical)
	}
}

func TestFingerprintRunCreationCoversSemanticFields(t *testing.T) {
	t.Parallel()

	base := baseRunFingerprintInput()
	want, err := FingerprintRunCreation(base)
	if err != nil {
		t.Fatalf("FingerprintRunCreation(base) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RunFingerprintInput)
	}{
		{name: "target agent", mutate: func(v *RunFingerprintInput) { v.Target.AgentID = "agent-2" }},
		{name: "target release", mutate: func(v *RunFingerprintInput) { v.Target.ReleaseID = "release-2" }},
		{name: "input", mutate: func(v *RunFingerprintInput) { v.Input["query"] = "different" }},
		{name: "metadata", mutate: func(v *RunFingerprintInput) { v.Metadata["locale"] = "en" }},
		{name: "callback", mutate: func(v *RunFingerprintInput) { v.Callback.(map[string]any)["url"] = "https://other.example/callback" }},
		{name: "delivery", mutate: func(v *RunFingerprintInput) { v.Delivery.(map[string]any)["target_id"] = "delivery-2" }},
		{name: "source protocol", mutate: func(v *RunFingerprintInput) { v.Source.Protocol = "mcp" }},
		{name: "source method", mutate: func(v *RunFingerprintInput) { v.Source.Method = "tools/call" }},
		{name: "parent run", mutate: func(v *RunFingerprintInput) { v.ParentRunID = "run-parent-2" }},
		{name: "caller agent", mutate: func(v *RunFingerprintInput) { v.CallerAgentID = "agent-caller-2" }},
		{name: "delegation reason", mutate: func(v *RunFingerprintInput) { v.Delegation.Reason = "different" }},
		{name: "delegation mode", mutate: func(v *RunFingerprintInput) { v.Delegation.Mode = "paid" }},
		{name: "delegation depth", mutate: func(v *RunFingerprintInput) { v.Delegation.Depth++ }},
		{name: "delegation options", mutate: func(v *RunFingerprintInput) { v.Delegation.Options["policy"] = "strict" }},
		{name: "a2a message", mutate: func(v *RunFingerprintInput) { v.A2A.MessageID = "message-2" }},
		{name: "a2a context", mutate: func(v *RunFingerprintInput) { v.A2A.ProtocolContextID = "context-2" }},
		{name: "a2a reference", mutate: func(v *RunFingerprintInput) { v.A2A.ReferenceTaskIDs[0] = "reference-2" }},
		{name: "a2a source", mutate: func(v *RunFingerprintInput) { v.A2A.Source = "agent_delegation" }},
		{name: "a2a visibility", mutate: func(v *RunFingerprintInput) { v.A2A.Visibility = "private" }},
		{name: "a2a output mode", mutate: func(v *RunFingerprintInput) { v.A2A.AcceptedOutputModes[0] = "text/plain" }},
		{name: "a2a extension", mutate: func(v *RunFingerprintInput) { v.A2A.Extensions[0] = "urn:other" }},
		{name: "a2a options", mutate: func(v *RunFingerprintInput) { v.A2A.Options["artifact_mode"] = "inline" }},
		{name: "visibility", mutate: func(v *RunFingerprintInput) { v.Visibility = "private" }},
		{name: "options", mutate: func(v *RunFingerprintInput) { v.Options.(map[string]any)["result_mode"] = "summary" }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			candidate := baseRunFingerprintInput()
			tt.mutate(&candidate)
			got, err := FingerprintRunCreation(candidate)
			if err != nil {
				t.Fatalf("FingerprintRunCreation() error = %v", err)
			}
			if got == want {
				t.Fatalf("semantic change %q did not change fingerprint", tt.name)
			}
		})
	}
}

func TestFingerprintRunCreationExcludesTransportFieldsOnly(t *testing.T) {
	t.Parallel()

	left := baseRunFingerprintInput()
	left.Transport = RunFingerprintTransport{
		RequestID:      "request-1",
		TraceID:        "trace-1",
		TraceParent:    "00-trace-1-span-1-01",
		RetryCount:     1,
		IdempotencyKey: "never-hash-me-1",
	}
	left.A2A.TraceID = "a2a-trace-1"
	right := baseRunFingerprintInput()
	right.Transport = RunFingerprintTransport{
		RequestID:      "request-2",
		TraceID:        "trace-2",
		TraceParent:    "00-trace-2-span-2-01",
		RetryCount:     99,
		IdempotencyKey: "never-hash-me-2",
	}
	right.A2A.TraceID = "a2a-trace-2"

	leftFingerprint, err := FingerprintRunCreation(left)
	if err != nil {
		t.Fatalf("FingerprintRunCreation(left) error = %v", err)
	}
	rightFingerprint, err := FingerprintRunCreation(right)
	if err != nil {
		t.Fatalf("FingerprintRunCreation(right) error = %v", err)
	}
	if leftFingerprint != rightFingerprint {
		t.Fatal("request/trace/retry/key transport data changed the fingerprint")
	}

	// Same-named values inside user-controlled semantic objects are not
	// recursively stripped; changing them must change the fingerprint.
	right = baseRunFingerprintInput()
	right.Input["trace_id"] = "business-trace-2"
	rightFingerprint, err = FingerprintRunCreation(right)
	if err != nil {
		t.Fatalf("FingerprintRunCreation(user trace) error = %v", err)
	}
	baseFingerprint, err := FingerprintRunCreation(baseRunFingerprintInput())
	if err != nil {
		t.Fatalf("FingerprintRunCreation(base) error = %v", err)
	}
	if rightFingerprint == baseFingerprint {
		t.Fatal("semantic input trace_id was incorrectly excluded")
	}
}

func TestFingerprintRunCreationIsConcurrentAndPure(t *testing.T) {
	input := baseRunFingerprintInput()
	want, err := FingerprintRunCreation(input)
	if err != nil {
		t.Fatalf("FingerprintRunCreation(base) error = %v", err)
	}

	const workers = 100
	start := make(chan struct{})
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wait.Done()
			<-start
			got, err := FingerprintRunCreation(input)
			if err != nil {
				errors <- err
				return
			}
			if got != want {
				errors <- &fingerprintMismatchError{got: got, want: want}
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}

	if input.Input["query"] != "hello" || input.A2A.ReferenceTaskIDs[0] != "reference-1" {
		t.Fatal("FingerprintRunCreation mutated its input")
	}
}

func TestCanonicalizeRFC8785RejectsInvalidIJSON(t *testing.T) {
	t.Parallel()

	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	tests := []struct {
		name  string
		value any
	}{
		{name: "nan", value: math.NaN()},
		{name: "positive infinity", value: math.Inf(1)},
		{name: "negative infinity", value: math.Inf(-1)},
		{name: "json number overflow", value: json.Number("1e9999")},
		{name: "invalid json number", value: json.Number("01")},
		{name: "non representable integer", value: int64(9007199254740993)},
		{name: "invalid unicode", value: string([]byte{0xff})},
		{name: "non string object key", value: map[int]any{1: "one"}},
		{name: "struct", value: struct{ Value string }{Value: "hidden serializer"}},
		{name: "cycle", value: cyclic},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := CanonicalizeRFC8785(tt.value)
			if err == nil {
				t.Fatal("CanonicalizeRFC8785() error = nil")
			}
			class, ok := IdempotencyErrorClassOf(err)
			if !ok || class != IdempotencyErrorInputInvalid {
				t.Fatalf("IdempotencyErrorClassOf() = %q, %v; want %q, true", class, ok, IdempotencyErrorInputInvalid)
			}
		})
	}
}

func TestFingerprintRunCreationNormalizesAbsentMetadata(t *testing.T) {
	t.Parallel()

	left := baseRunFingerprintInput()
	left.Metadata = nil
	right := baseRunFingerprintInput()
	right.Metadata = map[string]any{}
	leftFingerprint, err := FingerprintRunCreation(left)
	if err != nil {
		t.Fatalf("FingerprintRunCreation(nil metadata) error = %v", err)
	}
	rightFingerprint, err := FingerprintRunCreation(right)
	if err != nil {
		t.Fatalf("FingerprintRunCreation(empty metadata) error = %v", err)
	}
	if leftFingerprint != rightFingerprint {
		t.Fatal("nil and empty metadata must describe the same normalized request")
	}
}

type fingerprintMismatchError struct {
	got  [sha256.Size]byte
	want [sha256.Size]byte
}

func (e *fingerprintMismatchError) Error() string {
	return "concurrent fingerprint mismatch: got " + hex.EncodeToString(e.got[:]) + ", want " + hex.EncodeToString(e.want[:])
}

func baseRunFingerprintInput() RunFingerprintInput {
	return RunFingerprintInput{
		Target: RunFingerprintTarget{
			AgentID:   "agent-1",
			ReleaseID: "release-1",
		},
		Input: map[string]any{
			"query":           "hello",
			"trace_id":        "business-trace-1",
			"request_id":      "business-request-1",
			"retry_count":     7,
			"idempotency_key": "business-key-1",
		},
		Metadata: map[string]any{"locale": "zh-CN"},
		Callback: map[string]any{
			"event_types": []string{"run.completed"},
			"url":         "https://caller.example/callback",
		},
		Delivery: map[string]any{"target_id": "delivery-1"},
		Source: RunFingerprintSource{
			Protocol: "rest",
			Method:   "create-run",
		},
		ParentRunID:   "run-parent-1",
		CallerAgentID: "agent-caller-1",
		Delegation: &RunFingerprintDelegation{
			Reason:  "research",
			Mode:    "free",
			Depth:   1,
			Options: map[string]any{"policy": "default"},
		},
		A2A: &RunFingerprintA2A{
			MessageID:           "message-1",
			ProtocolContextID:   "context-1",
			ProtocolTaskID:      "task-1",
			RootContextID:       "root-1",
			ParentContextID:     "parent-context-1",
			ParentTaskID:        "parent-task-1",
			ReferenceTaskIDs:    []string{"reference-1"},
			Source:              "a2a_protocol",
			Visibility:          "shared",
			AcceptedOutputModes: []string{"application/json"},
			Extensions:          []string{"urn:openlinker:test"},
			Options:             map[string]any{"artifact_mode": "reference"},
		},
		Visibility: "shared",
		Options:    map[string]any{"result_mode": "full"},
	}
}
