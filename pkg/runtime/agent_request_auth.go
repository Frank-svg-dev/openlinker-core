package runtime

import "github.com/labstack/echo/v4"

// AuthenticateAgentRequest authenticates the Agent Token and device mTLS
// identity used by server-owned compatibility adapters. It deliberately
// exposes only the already validated Runtime principal; adapters must derive
// all user and Agent ownership data from that principal on Core.
func (h *RuntimeHTTPController) AuthenticateAgentRequest(c echo.Context) (AuthenticatedRuntimePrincipal, *RuntimeTransportError) {
	return h.authenticate(c)
}
