package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/provider"
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

// minimalProvider is a valid single-provider config body, for tests that only
// care about one specific field being wrong.
const minimalProvider = `
providers:
  openai:
    url: https://api.openai.com
    key: literal-key
    auth: bearer
`

func TestLoadReadsAllFields(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "sk-ant-from-env")

	path := writeConfig(t, `
port: 9090
host: 0.0.0.0
providers:
  anthropic:
    url: https://api.anthropic.com
    timeout: 45
    key: ${TEST_ANTHROPIC_KEY}
    auth: x-api-key
    headers:
      anthropic-version: "2023-06-01"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.Host != "0.0.0.0" {
		t.Errorf("Host = %q, want %q", cfg.Host, "0.0.0.0")
	}
	if got := cfg.Addr(); got != "0.0.0.0:9090" {
		t.Errorf("Addr() = %q, want %q", got, "0.0.0.0:9090")
	}

	pc := cfg.Providers["anthropic"]
	if pc.URL != "https://api.anthropic.com" {
		t.Errorf("url = %q, want %q", pc.URL, "https://api.anthropic.com")
	}
	if got, want := pc.Timeout(), 45*time.Second; got != want {
		t.Errorf("Timeout() = %v, want %v", got, want)
	}
	if pc.Key != "sk-ant-from-env" {
		t.Errorf("Key = %q, want it expanded to %q", pc.Key, "sk-ant-from-env")
	}
	if pc.Headers["anthropic-version"] != "2023-06-01" {
		t.Errorf("headers = %v, want anthropic-version 2023-06-01", pc.Headers)
	}
}

// TestLoadAppliesDefaults checks that absent keys fall back: port, host, and
// each provider's timeout.
func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalProvider))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, want the default %d", cfg.Port, DefaultPort)
	}
	if cfg.Host != DefaultHost {
		t.Errorf("Host = %q, want the default %q", cfg.Host, DefaultHost)
	}
	if got := cfg.Providers["openai"].TimeoutSeconds; got != DefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, want the default %d", got, DefaultTimeoutSeconds)
	}
}

// TestDefaultHostIsLoopback pins a security decision rather than a preference:
// the gateway holds real provider keys and has no tenant auth yet, so it must
// not bind to every interface by default.
func TestDefaultHostIsLoopback(t *testing.T) {
	if DefaultHost != "127.0.0.1" {
		t.Errorf("DefaultHost = %q, want 127.0.0.1 — the gateway holds API keys and has no auth yet", DefaultHost)
	}
}

// TestLoadExpandsKeyFromEnvironment is the mechanism that keeps real keys out
// of the committed config file.
func TestLoadExpandsKeyFromEnvironment(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "sk-secret-from-env")

	cfg, err := Load(writeConfig(t, `
providers:
  openai:
    url: https://api.openai.com
    key: ${TEST_OPENAI_KEY}
    auth: bearer
`))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if got := cfg.Providers["openai"].Key; got != "sk-secret-from-env" {
		t.Errorf("Key = %q, want %q", got, "sk-secret-from-env")
	}
}

// TestLoadFailsOnUnsetEnvironmentKey makes a misconfigured deployment fail at
// startup rather than on its first request with a confusing upstream 401.
func TestLoadFailsOnUnsetEnvironmentKey(t *testing.T) {
	t.Setenv("TEST_MISSING_KEY", "")

	_, err := Load(writeConfig(t, `
providers:
  openai:
    url: https://api.openai.com
    key: ${TEST_MISSING_KEY}
    auth: bearer
`))
	if err == nil {
		t.Fatal("Load() with an unset key env var = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "TEST_MISSING_KEY") {
		t.Errorf("error %q should name the missing environment variable", err)
	}
}

// TestLoadOnRepoConfig parses the config.yaml that ships with the repo, so a
// typo in the committed file fails the test suite rather than startup.
func TestLoadOnRepoConfig(t *testing.T) {
	// The committed file references these; set them so the test doesn't depend
	// on the developer's shell.
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GEMINI_API_KEY", "sk-test")
	t.Setenv("GROQ_API_KEY", "sk-test")

	cfg, err := Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("Load() on the repo's config.yaml returned error: %v", err)
	}

	want := []string{"anthropic", "gemini", "groq", "openai"}
	got := cfg.ProviderNames()
	if len(got) != len(want) {
		t.Fatalf("ProviderNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ProviderNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Groq's base path is what makes it a useful test case; don't let it be
	// silently dropped from the committed config.
	if url := cfg.Providers["groq"].URL; !strings.HasSuffix(url, "/openai") {
		t.Errorf("groq url = %q, want it to keep its /openai base path", url)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("Load() on a missing file = nil error, want an error")
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, "port: [not, a, number\n")); err == nil {
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
		{"no providers at all", "port: 8080\n"},
		{"port zero", "port: 0\n" + minimalProvider},
		{"port too large", "port: 70000\n" + minimalProvider},
		{"empty host", "host: \"\"\n" + minimalProvider},
		{"empty provider url", "providers:\n  openai:\n    url: \"\"\n    key: k\n    auth: bearer\n"},
		{"url without scheme", "providers:\n  openai:\n    url: api.openai.com\n    key: k\n    auth: bearer\n"},
		{"url with bad scheme", "providers:\n  openai:\n    url: ftp://api.openai.com\n    key: k\n    auth: bearer\n"},
		{"unknown auth style", "providers:\n  openai:\n    url: https://api.openai.com\n    key: k\n    auth: basic\n"},
		{"missing auth style", "providers:\n  openai:\n    url: https://api.openai.com\n    key: k\n"},
		{"negative timeout", "providers:\n  openai:\n    url: https://api.openai.com\n    key: k\n    auth: bearer\n    timeout: -5\n"},
		{"empty key", "providers:\n  openai:\n    url: https://api.openai.com\n    key: \"\"\n    auth: bearer\n"},
		{"reserved name health", "providers:\n  health:\n    url: https://api.openai.com\n    key: k\n    auth: bearer\n"},
		{"name containing a slash", "providers:\n  open/ai:\n    url: https://api.openai.com\n    key: k\n    auth: bearer\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tc.yaml)); err == nil {
				t.Errorf("Load() on %q = nil error, want a validation error", tc.yaml)
			}
		})
	}
}

// TestBuildProviders checks the conversion from on-disk config into the parsed
// form the proxy consumes.
func TestBuildProviders(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
providers:
  groq:
    url: https://api.groq.com/openai
    key: sk-groq
    auth: bearer
  anthropic:
    url: https://api.anthropic.com
    key: sk-ant
    auth: x-api-key
    headers:
      anthropic-version: "2023-06-01"
`))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	providers, err := cfg.BuildProviders()
	if err != nil {
		t.Fatalf("BuildProviders() returned error: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("got %d providers, want 2", len(providers))
	}

	// Sorted by name, so anthropic comes first.
	ant := providers[0]
	if ant.Name != "anthropic" {
		t.Fatalf("providers[0].Name = %q, want %q", ant.Name, "anthropic")
	}
	if ant.Auth != provider.AuthAPIKey {
		t.Errorf("anthropic Auth = %q, want %q", ant.Auth, provider.AuthAPIKey)
	}
	if ant.URL.Host != "api.anthropic.com" {
		t.Errorf("anthropic URL.Host = %q, want %q", ant.URL.Host, "api.anthropic.com")
	}
	if ant.Key != "sk-ant" {
		t.Errorf("anthropic Key = %q, want %q", ant.Key, "sk-ant")
	}
	if got, want := ant.Timeout, DefaultTimeoutSeconds*time.Second; got != want {
		t.Errorf("anthropic Timeout = %v, want %v", got, want)
	}

	// The base path is what makes Groq's routing work; it must survive.
	if got := providers[1].URL.Path; got != "/openai" {
		t.Errorf("groq URL.Path = %q, want %q", got, "/openai")
	}
}
