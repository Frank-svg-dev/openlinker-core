package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/OpenLinker-ai/openlinker-core/pkg/config"
	coreruntime "github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

func TestNewManifestUsesStablePublicEntrypoints(t *testing.T) {
	manifest := NewManifest(&config.Config{
		Env:                "test",
		APIURL:             "https://api.openlinker.test/",
		FrontendURL:        "https://openlinker.test/",
		RuntimeMTLSEnabled: true,
		RuntimeMTLSAPIURL:  "https://runtime.openlinker.test:8443",
	})

	require.Equal(t, "OpenLinker", manifest.Name)
	require.Equal(t, "v1", manifest.Version)
	require.Equal(t, "https://runtime.openlinker.test:8443", manifest.BaseURLs.Runtime)
	require.True(t, manifest.Runtime.Enabled)
	require.True(t, manifest.Runtime.MTLSRequired)
	require.Equal(t, []string{"websocket", "long_poll"}, manifest.Runtime.Transports)
	require.Equal(t, "auto", manifest.Runtime.DefaultTransport)
	require.Equal(t, coreruntime.RuntimeContractDigest, manifest.Runtime.CurrentContractDigest)
	require.Len(t, manifest.Runtime.SupportedContractDigests, 2)
	require.Equal(t, coreruntime.RuntimeContractDigest, manifest.Runtime.SupportedContractDigests[0])
	require.NotContains(t, manifest.Runtime.SupportedContractDigests, "857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f")
	_, err := time.Parse(time.RFC3339, manifest.Runtime.PreviousSupportedUntil)
	require.NoError(t, err)
	require.Equal(t, ManifestRuntimeTransportPolicy{
		Version: 1, HeartbeatIntervalSeconds: 20, SessionStaleAfterSeconds: 45,
		RetryMinimumMilliseconds: 250, RetryMaximumMilliseconds: 15000,
		WebSocketProbeIntervalMS: 15000, WebSocketProbeTimeoutMS: 10000,
	}, manifest.Runtime.TransportPolicy)
	require.Equal(t, "https://api.openlinker.test/skill/publish-agent", manifest.Docs.PublishAgent)
	require.Equal(t, "https://api.openlinker.test/skill/consume-agent", manifest.Docs.ConsumeAgent)
	require.Equal(t, "https://api.openlinker.test/api/v1/agents/{slug}/agent-card.json", manifest.Docs.AgentCard)
	require.Equal(t, "https://api.openlinker.test/api/v1/mcp/tools", manifest.Tools.MCPTools)
	require.Equal(t, "https://api.openlinker.test/api/v1/a2a/agents/{slug}", manifest.Protocols.A2A)
	require.Equal(t, "https://api.openlinker.test/api/v1/runs/{run_id}/events", manifest.Protocols.RunEvents)
	require.Contains(t, manifest.Tools.Names, "run_agent")
	require.Contains(t, manifest.Auth.APIScopes, "agents:run")
	require.Contains(t, manifest.Auth.APIScopes, "tasks:create")
	require.Contains(t, manifest.Auth.APIScopes, "tasks:read")
	require.Contains(t, manifest.Auth.APIScopes, "tasks:run")
	require.NotContains(t, manifest.Auth.APIScopes, "tasks:publish")
	require.NotContains(t, manifest.Auth.APIScopes, "tasks:work")
	require.NotContains(t, manifest.Auth.APIScopes, "tasks:review")
	require.NotContains(t, manifest.Auth.APIScopes, "tasks:write")
	require.Contains(t, manifest.Auth.AgentScopes, "agent:pull")
	require.Equal(t, "run public agents through REST, MCP, A2A, or delegated calls", manifest.TokenScopes["agents:run"])
	require.NotEmpty(t, manifest.TokenScopes["agent-tokens:issue"])
	require.Empty(t, manifest.TokenScopes["tasks:write"])
	require.Empty(t, manifest.TokenScopes["tasks:publish"])
	require.Empty(t, manifest.TokenScopes["tasks:work"])
	require.Empty(t, manifest.TokenScopes["tasks:review"])
	require.Equal(t, "no_pre_review", manifest.Policies["public_listing"])
	require.Equal(t, "not_enabled", manifest.Policies["payments"])
	require.Equal(t, "not_enabled", manifest.Policies["agent_autonomous_purchase"])
	require.Contains(t, manifest.States.Run, "success")
	require.Equal(t, []string{"needs_agent", "open", "matched", "completed"}, manifest.States.Task)
	require.Equal(t, "dag_async_agent_workflow_api", manifest.Workflows.Builder)
}

func TestNewManifestFallsBackToLocalPublicEntrypoints(t *testing.T) {
	manifest := NewManifest(&config.Config{})

	require.Equal(t, "http://localhost:8080", manifest.BaseURLs.API)
	require.Equal(t, "http://localhost:3000", manifest.BaseURLs.Web)
	require.Empty(t, manifest.BaseURLs.Runtime)
	require.False(t, manifest.Runtime.Enabled)
	require.True(t, manifest.Runtime.MTLSRequired)
	require.Equal(t, []string{"websocket", "long_poll"}, manifest.Runtime.Transports)
	require.Equal(t, "auto", manifest.Runtime.DefaultTransport)
	require.Equal(t, int64(45), manifest.Runtime.TransportPolicy.SessionStaleAfterSeconds)
	require.Equal(t, "http://localhost:8080/api/v1/a2a/agents/{slug}", manifest.Protocols.A2A)
	require.Equal(t, "http://localhost:3000/connect", manifest.Docs.Connect)
}

func TestNewManifestRuntimeDiscoveryFailsClosed(t *testing.T) {
	tests := []string{
		"",
		"http://runtime.openlinker.test:8443",
		"https://user:secret@runtime.openlinker.test:8443",
		"https://runtime.openlinker.test:8443/",
		"https://runtime.openlinker.test:8443/api/v1/agent-runtime",
		"https://runtime.openlinker.test:8443?token=secret",
		"https://runtime.openlinker.test:8443?",
		"https://runtime.openlinker.test:8443#runtime",
		"https://runtime.openlinker.test:8443#",
		"https://runtime.openlinker.test:",
		"https://runtime.openlinker.test:0",
		"https://runtime.openlinker.test:65536",
	}
	for _, runtimeURL := range tests {
		t.Run(runtimeURL, func(t *testing.T) {
			manifest := NewManifest(&config.Config{
				APIURL:             "https://api.openlinker.test",
				RuntimeMTLSEnabled: true,
				RuntimeMTLSAPIURL:  runtimeURL,
			})
			require.False(t, manifest.Runtime.Enabled)
			require.Empty(t, manifest.BaseURLs.Runtime)
			require.NotEqual(t, manifest.BaseURLs.API, manifest.BaseURLs.Runtime)
		})
	}
}

func TestNewManifestPublishesNoRuntimeCredentialsOrEndpointPath(t *testing.T) {
	manifest := NewManifest(&config.Config{
		APIURL:                                 "https://api.openlinker.test",
		FrontendURL:                            "https://openlinker.test",
		RuntimeMTLSEnabled:                     true,
		RuntimeMTLSAPIURL:                      "https://runtime.openlinker.test:8443",
		RuntimeMTLSCertFile:                    "/internal/pki/server-secret.pem",
		RuntimeMTLSKeyFile:                     "/internal/pki/server-key-secret.pem",
		RuntimeMTLSClientCAFile:                "/internal/pki/client-ca-secret.pem",
		RuntimeInvocationSigningSecret:         "never-publish-runtime-signing-secret",
		RuntimeInvocationSigningKeyID:          "private-key-id",
		RuntimeInvocationPreviousSigningSecret: "never-publish-previous-secret",
	})

	body, err := json.Marshal(manifest)
	require.NoError(t, err)
	serialized := string(body)
	require.Contains(t, serialized, `"runtime":"https://runtime.openlinker.test:8443"`)
	require.NotContains(t, serialized, "server-secret")
	require.NotContains(t, serialized, "server-key-secret")
	require.NotContains(t, serialized, "client-ca-secret")
	require.NotContains(t, serialized, "never-publish")
	require.NotContains(t, serialized, "private-key-id")
	require.NotContains(t, serialized, "/api/v1/agent-runtime")
	require.NotContains(t, serialized, coreruntime.RuntimeContractDigest+"/")
	require.NotContains(t, manifest.BaseURLs.Runtime, "/v2")
}

func TestServeOpenLinkerManifest(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/openlinker.json", nil)
	rec := httptest.NewRecorder()

	handler := ServeOpenLinkerManifest(&config.Config{
		Env:                "production",
		APIURL:             " https://api.openlinker.test/// ",
		FrontendURL:        " https://openlinker.test/// ",
		RuntimeMTLSEnabled: true,
		RuntimeMTLSAPIURL:  "https://runtime.openlinker.test:8443",
	})

	require.NoError(t, handler(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "public, max-age=300", rec.Header().Get(echo.HeaderCacheControl))

	var manifest OpenLinkerManifest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &manifest))
	require.Equal(t, "production", manifest.Environment)
	require.Equal(t, "https://api.openlinker.test", manifest.BaseURLs.API)
	require.Equal(t, "https://openlinker.test", manifest.BaseURLs.Web)
	require.Equal(t, "https://runtime.openlinker.test:8443", manifest.BaseURLs.Runtime)
	require.True(t, manifest.Runtime.Enabled)
	require.Equal(t, "https://api.openlinker.test/api/v1/mcp", manifest.Protocols.MCP)
}
