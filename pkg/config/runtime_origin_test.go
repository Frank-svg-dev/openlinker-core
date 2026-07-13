package config

import "testing"

func TestNormalizeRuntimePublicOrigin(t *testing.T) {
	for _, value := range []string{
		"https://runtime.example",
		"https://runtime.example:1",
		"https://runtime.example:443",
		"https://runtime.example:65535",
		"https://[::1]:8443",
	} {
		t.Run("valid_"+value, func(t *testing.T) {
			got, err := NormalizeRuntimePublicOrigin("  " + value + "  ")
			if err != nil {
				t.Fatalf("NormalizeRuntimePublicOrigin(%q): %v", value, err)
			}
			if got != value {
				t.Fatalf("normalized origin = %q, want %q", got, value)
			}
		})
	}

	for _, value := range []string{
		"",
		"runtime.example",
		"http://runtime.example:8443",
		"https://:8443",
		"https://user:secret@runtime.example:8443",
		"https://runtime.example/",
		"https://runtime.example/api/v1/agent-runtime",
		"https://runtime.example/%2F",
		"https://runtime.example?token=secret",
		"https://runtime.example?",
		"https://runtime.example#runtime",
		"https://runtime.example#",
		"https://runtime.example:",
		"https://runtime.example:0",
		"https://runtime.example:65536",
		"https://runtime.example:https",
		"ftp://runtime.example",
	} {
		t.Run("invalid_"+value, func(t *testing.T) {
			if got, err := NormalizeRuntimePublicOrigin(value); err == nil {
				t.Fatalf("invalid origin accepted as %q", got)
			}
		})
	}
}
