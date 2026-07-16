package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestAuthenticateAgentRequestUsesCanonicalRuntimeAuthentication(t *testing.T) {
	fixture := newRuntimeHandlerFixture()
	controller := fixture.controller()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-runtime/a2a-proxy/agents/ignored", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer runtime-secret")
	c := e.NewContext(req, httptest.NewRecorder())

	principal, transportErr := controller.AuthenticateAgentRequest(c)

	require.Nil(t, transportErr)
	require.Equal(t, fixture.authenticated, principal)
	require.Equal(t, "runtime-secret", fixture.tokens.plaintext)
	require.Equal(t, []string{runtimeTokenScope}, fixture.tokens.scopes)
	require.Equal(t, 1, fixture.devices.calls)
}
