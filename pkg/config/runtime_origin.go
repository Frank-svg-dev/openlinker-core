package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// NormalizeRuntimePublicOrigin validates the public HTTPS origin that Core
// publishes for its dedicated mTLS Runtime listener. Keeping this validator in
// config makes startup validation and discovery serialization share exactly
// the same fail-closed boundary.
func NormalizeRuntimePublicOrigin(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("Runtime public URL must be an absolute HTTPS origin")
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("Runtime public URL must be an absolute HTTPS origin")
	}
	if parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.RawFragment != "" || strings.Contains(value, "#") {
		return "", fmt.Errorf("Runtime public URL must not include credentials, a path, query, or fragment")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("Runtime public URL has an invalid port")
	}
	if portText := parsed.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("Runtime public URL has an invalid port")
		}
	}
	return parsed.String(), nil
}
