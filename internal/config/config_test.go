package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeConfig drops a config file into the test's own temporary directory.
//
// Go note: t.TempDir() creates a directory that the testing package deletes
// automatically when the test finishes — no cleanup code to forget.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadReadsAllFields(t *testing.T) {
	path := writeConfig(t, `
port: 9090
upstream:
  url: https://api.anthropic.com
  timeout: 45
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.Upstream.URL != "https://api.anthropic.com" {
		t.Errorf("Upstream.URL = %q, want %q", cfg.Upstream.URL, "https://api.anthropic.com")
	}
	if got, want := cfg.Upstream.Timeout(), 45*time.Second; got != want {
		t.Errorf("Upstream.Timeout() = %v, want %v", got, want)
	}
	if got := cfg.Addr(); got != ":9090" {
		t.Errorf("Addr() = %q, want %q", got, ":9090")
	}
}

// TestLoadAppliesDefaults checks the "unmarshal into a pre-filled struct"
// trick: keys absent from the file keep their default values.
func TestLoadAppliesDefaults(t *testing.T) {
	path := writeConfig(t, "port: 3000\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Port != 3000 {
		t.Errorf("Port = %d, want 3000", cfg.Port)
	}
	if cfg.Upstream.URL != DefaultUpstreamURL {
		t.Errorf("Upstream.URL = %q, want the default %q", cfg.Upstream.URL, DefaultUpstreamURL)
	}
	if cfg.Upstream.TimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("Upstream.TimeoutSeconds = %d, want the default %d",
			cfg.Upstream.TimeoutSeconds, DefaultTimeoutSeconds)
	}
}

// TestLoadOnRepoConfig parses the config.yaml that ships with the repo, so a
// typo in the committed file fails the test suite rather than startup.
func TestLoadOnRepoConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("Load() on the repo's config.yaml returned error: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, DefaultPort)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("Load() on a missing file = nil error, want an error")
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	path := writeConfig(t, "port: [not, a, number\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() on malformed YAML = nil error, want an error")
	}
}

// TestValidateRejectsBadConfigs is table-driven: each case is a config that
// should fail validation, with a name explaining why.
func TestValidateRejectsBadConfigs(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"port zero", "port: 0\n"},
		{"port negative", "port: -1\n"},
		{"port too large", "port: 70000\n"},
		{"empty upstream url", "upstream:\n  url: \"\"\n"},
		{"upstream url without scheme", "upstream:\n  url: api.openai.com\n"},
		{"upstream url with bad scheme", "upstream:\n  url: ftp://api.openai.com\n"},
		{"zero timeout", "upstream:\n  timeout: 0\n"},
		{"negative timeout", "upstream:\n  timeout: -5\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)

			if _, err := Load(path); err == nil {
				t.Errorf("Load() on %q = nil error, want a validation error", tc.yaml)
			}
		})
	}
}

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Errorf("Default() config failed validation: %v", err)
	}
}
