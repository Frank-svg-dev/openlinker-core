package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

func TestNormalizeRunCreationCanonicalizesSourceAndCallbackAliases(t *testing.T) {
	t.Parallel()

	agentID := uuid.New()
	left := &RunRequest{
		AgentID:        strings.ToUpper(agentID.String()),
		IdempotencyKey: "normalize-left",
		Input: map[string]any{
			"z": map[string]any{"b": float64(2), "a": json.Number("1.0")},
			"a": []any{"first", true},
		},
		CreationProtocol: " REST ",
		CreationMethod:   " RUNS.CREATE ",
		TaskCallback: &TaskCallbackConfig{
			URL:             "  https://caller.example/callback  ",
			Token:           "callback-token",
			EventTypes:      []string{"run.failed", "run.completed", "run.failed"},
			Metadata:        map[string]any{"z": float64(2), "a": json.Number("1.0")},
			EventTypesAlias: []string{"run.created"}, // canonical field wins over its alias.
		},
	}
	right := &RunRequest{
		AgentID:        agentID.String(),
		IdempotencyKey: "normalize-right",
		Input: map[string]any{
			"a": []any{"first", true},
			"z": map[string]any{"a": 1, "b": 2},
		},
		Metadata:         map[string]any{},
		CreationProtocol: "rest",
		CreationMethod:   "runs.create",
		PushNotificationConfig: &TaskCallbackConfig{
			URL:             "https://caller.example/callback",
			Token:           "callback-token",
			EventTypesAlias: []string{"run.completed", "run.failed"},
			Metadata:        map[string]any{"a": 1, "z": 2},
		},
	}

	svc := &Service{}
	leftNormalized, err := svc.normalizeRunCreation(left, "web", createRunOptions{})
	if err != nil {
		t.Fatalf("normalize left: %v", err)
	}
	rightNormalized, err := svc.normalizeRunCreation(right, "web", createRunOptions{})
	if err != nil {
		t.Fatalf("normalize right: %v", err)
	}

	if leftNormalized.agentID != agentID || leftNormalized.request.AgentID != agentID.String() {
		t.Fatalf("agent ID was not canonicalized: id=%s request=%q", leftNormalized.agentID, leftNormalized.request.AgentID)
	}
	if leftNormalized.request.CreationProtocol != "rest" || leftNormalized.request.CreationMethod != "runs.create" {
		t.Fatalf("source was not normalized: %#v", leftNormalized.request)
	}
	if leftNormalized.idempotencyFingerprint == nil ||
		string(leftNormalized.idempotencyFingerprint) != string(rightNormalized.idempotencyFingerprint) {
		t.Fatalf("equivalent callback/source aliases produced different fingerprints: %x != %x",
			leftNormalized.idempotencyFingerprint, rightNormalized.idempotencyFingerprint)
	}
	if string(leftNormalized.idempotencyKeyHash) == string(rightNormalized.idempotencyKeyHash) {
		t.Fatal("different idempotency keys unexpectedly produced the same key hash")
	}

	// Normalization must operate on a copy: callers can safely reuse their
	// request when a transport retries it.
	if left.AgentID != strings.ToUpper(agentID.String()) || left.CreationProtocol != " REST " {
		t.Fatalf("normalizeRunCreation mutated caller request: %#v", left)
	}
	if got := left.TaskCallback.EventTypes; len(got) != 3 || got[0] != "run.failed" {
		t.Fatalf("callback event types were mutated in place: %#v", got)
	}
}

func TestNormalizeRunCreationFingerprintIncludesNormalizedSource(t *testing.T) {
	t.Parallel()

	agentID := uuid.NewString()
	makeRequest := func(protocol, method string) *RunRequest {
		return &RunRequest{
			AgentID:          agentID,
			Input:            map[string]any{"query": "same semantics otherwise"},
			IdempotencyKey:   "source-fingerprint",
			CreationProtocol: protocol,
			CreationMethod:   method,
		}
	}
	svc := &Service{}
	rest, err := svc.normalizeRunCreation(makeRequest("REST", "RUNS.CREATE"), "web", createRunOptions{})
	if err != nil {
		t.Fatalf("normalize REST: %v", err)
	}
	mcp, err := svc.normalizeRunCreation(makeRequest("mcp", "run_agent"), "web", createRunOptions{})
	if err != nil {
		t.Fatalf("normalize MCP: %v", err)
	}
	if string(rest.idempotencyFingerprint) == string(mcp.idempotencyFingerprint) {
		t.Fatal("changing the normalized protocol/method did not change the fingerprint")
	}

	tests := []struct {
		source       string
		wantProtocol string
		wantMethod   string
	}{
		{source: "", wantProtocol: "rest", wantMethod: "runs.create"},
		{source: "web", wantProtocol: "rest", wantMethod: "runs.create"},
		{source: "mcp", wantProtocol: "mcp", wantMethod: "run_agent"},
		{source: "api", wantProtocol: "api", wantMethod: "runs.create"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run("fallback_"+tt.wantProtocol, func(t *testing.T) {
			got, err := normalizeRunCreationSource("", "", tt.source)
			if err != nil {
				t.Fatalf("normalizeRunCreationSource(%q): %v", tt.source, err)
			}
			if got.Protocol != tt.wantProtocol || got.Method != tt.wantMethod {
				t.Fatalf("normalizeRunCreationSource(%q) = %#v, want %s/%s", tt.source, got, tt.wantProtocol, tt.wantMethod)
			}
		})
	}
}

func TestTaskCallbackAliasesHaveExplicitPrecedence(t *testing.T) {
	t.Parallel()

	canonical := &TaskCallbackConfig{URL: "https://canonical.example/callback"}
	pushSnake := &TaskCallbackConfig{URL: "https://push-snake.example/callback"}
	pushCamel := &TaskCallbackConfig{URL: "https://push-camel.example/callback"}
	pushConfig := &TaskCallbackConfig{URL: "https://push-config.example/callback"}

	tests := []struct {
		name string
		req  *RunRequest
		want *TaskCallbackConfig
	}{
		{name: "canonical", req: &RunRequest{TaskCallback: canonical}, want: canonical},
		{name: "push notification", req: &RunRequest{PushNotification: pushSnake}, want: pushSnake},
		{name: "camel alias", req: &RunRequest{PushNotificationAlias: pushCamel}, want: pushCamel},
		{name: "config alias", req: &RunRequest{PushNotificationConfig: pushConfig}, want: pushConfig},
		{
			name: "canonical wins",
			req: &RunRequest{
				TaskCallback:           canonical,
				PushNotification:       pushSnake,
				PushNotificationAlias:  pushCamel,
				PushNotificationConfig: pushConfig,
			},
			want: canonical,
		},
		{
			name: "snake push wins over camel aliases",
			req: &RunRequest{
				PushNotification:       pushSnake,
				PushNotificationAlias:  pushCamel,
				PushNotificationConfig: pushConfig,
			},
			want: pushSnake,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := taskCallbackConfigFromRunRequest(tt.req); got != tt.want {
				t.Fatalf("taskCallbackConfigFromRunRequest() = %p, want %p", got, tt.want)
			}
		})
	}
}

func TestRESTRunCreationRequiresSafeKeyAndReturnsReplaySemantics(t *testing.T) {
	t.Parallel()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	runID := uuid.NewString()
	body := `{"agent_id":"` + agentID + `","input":{"query":"hello"}}`

	assertIdempotencyError := func(t *testing.T, key string, wantCode IdempotencyErrorClass) {
		t.Helper()
		mock := &mockRuntimeService{}
		h := NewHandler(mock)
		headers := map[string]string{}
		if key != "" {
			headers["Idempotency-Key"] = key
		}
		ctx, _ := newRuntimeDispatchContext(&runtimeDispatchRequest{
			method:     http.MethodPost,
			target:     "/api/v1/run",
			body:       body,
			userID:     userID,
			authMethod: "jwt",
			headers:    headers,
		})
		err := h.PostRun(ctx)
		var httpErr *httpx.HTTPError
		if !errors.As(err, &httpErr) {
			t.Fatalf("PostRun() error = %T %v, want *httpx.HTTPError", err, err)
		}
		if httpErr.Status != http.StatusUnprocessableEntity || httpErr.Code != httpx.ErrorCode(wantCode) {
			t.Fatalf("PostRun() error = %#v, want 422/%s", httpErr, wantCode)
		}
		if key != "" && strings.Contains(httpErr.Error(), key) {
			t.Fatalf("raw idempotency key leaked in error: %q", httpErr.Error())
		}
		if mock.runReq != nil {
			t.Fatalf("service was called for invalid idempotency key: %#v", mock.runReq)
		}
	}

	t.Run("missing key", func(t *testing.T) {
		assertIdempotencyError(t, "", IdempotencyErrorKeyRequired)
	})
	t.Run("invalid key", func(t *testing.T) {
		assertIdempotencyError(t, strings.Repeat("private-key-material", 20), IdempotencyErrorKeyInvalid)
	})

	tests := []struct {
		name             string
		async            bool
		response         *RunResponse
		wantStatus       int
		wantReplayHeader string
	}{
		{
			name:       "new synchronous run",
			response:   &RunResponse{RunID: runID, Status: "success"},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "new asynchronous run",
			async:      true,
			response:   &RunResponse{RunID: runID, Status: "running"},
			wantStatus: http.StatusCreated,
		},
		{
			name:             "completed replay",
			response:         &RunResponse{RunID: runID, Status: "success", Replayed: true},
			wantStatus:       http.StatusOK,
			wantReplayHeader: "true",
		},
		{
			name:             "running replay",
			response:         &RunResponse{RunID: runID, Status: "running", Replayed: true},
			wantStatus:       http.StatusAccepted,
			wantReplayHeader: "true",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockRuntimeService{runResp: tt.response, startRunResp: tt.response}
			h := NewHandler(mock)
			ctx, rec := newRuntimeDispatchContext(&runtimeDispatchRequest{
				method:     http.MethodPost,
				target:     "/api/v1/run",
				body:       body,
				userID:     userID,
				authMethod: "jwt",
				headers:    map[string]string{"Idempotency-Key": "safe-rest-key"},
			})
			var err error
			if tt.async {
				err = h.PostRunAsync(ctx)
			} else {
				err = h.PostRun(ctx)
			}
			if err != nil {
				t.Fatalf("run handler error: %v", err)
			}
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got, want := rec.Header().Get("Location"), "/api/v1/runs/"+runID; got != want {
				t.Fatalf("Location = %q, want %q", got, want)
			}
			if got := rec.Header().Get("Idempotency-Replayed"); got != tt.wantReplayHeader {
				t.Fatalf("Idempotency-Replayed = %q, want %q", got, tt.wantReplayHeader)
			}
			var gotReq *RunRequest
			if tt.async {
				gotReq = mock.startRunReq
			} else {
				gotReq = mock.runReq
			}
			if gotReq == nil || gotReq.IdempotencyKey != "safe-rest-key" ||
				gotReq.CreationProtocol != "rest" || gotReq.CreationMethod != "runs.create" {
				t.Fatalf("handler did not pass normalized idempotency context: %#v", gotReq)
			}
		})
	}
}

func TestRESTRunCreationPreferWaitContract(t *testing.T) {
	t.Parallel()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	runID := uuid.NewString()
	body := `{"agent_id":"` + agentID + `","input":{"query":"hello"}}`

	mock := &mockRuntimeService{startRunResp: &RunResponse{RunID: runID, Status: "running"}}
	h := NewHandler(mock)
	ctx, rec := newRuntimeDispatchContext(&runtimeDispatchRequest{
		method:     http.MethodPost,
		target:     "/api/v1/runs",
		body:       body,
		userID:     userID,
		authMethod: "jwt",
		headers: map[string]string{
			"Idempotency-Key": "prefer-wait-zero",
			"Prefer":          "respond-async, wait=0",
		},
	})
	require.NoError(t, h.PostRunAsync(ctx))
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, "wait=0", rec.Header().Get("Preference-Applied"))
	require.Equal(t, "/api/v1/runs/"+runID, rec.Header().Get("Location"))

	for _, raw := range []string{"wait=-1", "wait=31", "wait=abc", "wait=1, wait=2"} {
		_, _, err := parseRunPreferWait(raw)
		var httpErr *httpx.HTTPError
		require.ErrorAs(t, err, &httpErr, raw)
		require.Equal(t, http.StatusBadRequest, httpErr.Status, raw)
	}

	wait, applied, err := parseRunPreferWait("respond-async, WAIT=30")
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, 30*time.Second, wait)
}
