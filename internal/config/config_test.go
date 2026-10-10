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
// care about one specific field being wrong. Its key needs TEST_KEY set; see
// setTestKey.
const minimalProvider = `
providers:
  openai:
    url: https://api.openai.com
    key: ${TEST_KEY}
    auth: bearer
    format: openai
`

// setTestKey sets the TEST_KEY variable that minimalProvider and the other
// inline configs reference.
func setTestKey(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_KEY", "sk-test")
}

func TestLoadReadsAllFields(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "sk-ant-from-env")

	path := writeConfig(t, `
port: 9090
host: 0.0.0.0
max_request_bytes: 1048576
providers:
  anthropic:
    url: https://api.anthropic.com
    timeout: 45
    key: ${TEST_ANTHROPIC_KEY}
    auth: x-api-key
    format: anthropic
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
	if cfg.MaxRequestBytes != 1048576 {
		t.Errorf("MaxRequestBytes = %d, want 1048576", cfg.MaxRequestBytes)
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
	setTestKey(t)

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
	if cfg.MaxRequestBytes != DefaultMaxRequestBytes {
		t.Errorf("MaxRequestBytes = %d, want the default %d", cfg.MaxRequestBytes, DefaultMaxRequestBytes)
	}
	if got := cfg.Providers["openai"].TimeoutSeconds; got != DefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, want the default %d", got, DefaultTimeoutSeconds)
	}
}

// TestDefaultHostIsLoopback pins a security decision rather than a preference:
// the gateway holds real provider keys, so exposing it beyond this machine
// must be an explicit choice, never the default.
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
    format: openai
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
    format: openai
`))
	if err == nil {
		t.Fatal("Load() with an unset key env var = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "TEST_MISSING_KEY") {
		t.Errorf("error %q should name the missing environment variable", err)
	}
}

// unsetenv removes name from the environment until the test ends.
//
// Go note: t.Setenv restores the variable's old value when the test ends, so
// calling it first makes the Unsetenv temporary too.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

// TestLoadExpandsFieldReferences: url and timeout may be written as ${VAR} or
// ${VAR:-default}, which as in the shell takes the default when VAR is unset
// or empty.
func TestLoadExpandsFieldReferences(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string // TEST_URL and TEST_TIMEOUT are unset unless set here
		url, timeout string            // as written in the file
		wantURL      envString
		wantTimeout  envInt
		wantErr      string // a substring of Load's error, for a case that fails
	}{
		{name: "set", env: map[string]string{"TEST_URL": "http://10.0.0.5:11434", "TEST_TIMEOUT": "45"},
			url: "${TEST_URL:-http://127.0.0.1:11434}", timeout: "${TEST_TIMEOUT:-120}",
			wantURL: "http://10.0.0.5:11434", wantTimeout: 45},
		{name: "unset takes the default",
			url: "${TEST_URL:-http://127.0.0.1:11434}", timeout: "${TEST_TIMEOUT:-120}",
			wantURL: "http://127.0.0.1:11434", wantTimeout: 120},
		{name: "empty takes the default", env: map[string]string{"TEST_URL": "", "TEST_TIMEOUT": ""},
			url: "${TEST_URL:-http://127.0.0.1:11434}", timeout: "${TEST_TIMEOUT:-120}",
			wantURL: "http://127.0.0.1:11434", wantTimeout: 120},
		{name: "no default, set", env: map[string]string{"TEST_URL": "http://10.0.0.5:11434", "TEST_TIMEOUT": "45"},
			url: "${TEST_URL}", timeout: "${TEST_TIMEOUT}",
			wantURL: "http://10.0.0.5:11434", wantTimeout: 45},
		// Quoting a reference is natural YAML; it doesn't make the int a string.
		{name: "quoted", env: map[string]string{"TEST_TIMEOUT": "45"},
			url: "https://api.openai.com", timeout: `"${TEST_TIMEOUT:-120}"`,
			wantURL: "https://api.openai.com", wantTimeout: 45},
		// Only a whole-value reference is expanded.
		{name: "partial reference stays literal", env: map[string]string{"TEST_URL": "v1"},
			url: "https://api.openai.com/${TEST_URL}", timeout: "30",
			wantURL: "https://api.openai.com/${TEST_URL}", wantTimeout: 30},
		// Both errors carry the timeout's line, 4.
		{name: "unset, no default", url: "https://api.openai.com", timeout: "${TEST_TIMEOUT}",
			wantErr: "line 4: environment variable TEST_TIMEOUT is unset or empty"},
		{name: "not an integer", env: map[string]string{"TEST_TIMEOUT": "2m"},
			url: "https://api.openai.com", timeout: "${TEST_TIMEOUT:-120}",
			wantErr: "line 4: cannot unmarshal"},
	}

	setTestKey(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unsetenv(t, "TEST_URL")
			unsetenv(t, "TEST_TIMEOUT")
			for name, value := range tc.env {
				t.Setenv(name, value)
			}

			cfg, err := Load(writeConfig(t, "providers:\n  openai:\n    url: "+tc.url+"\n    timeout: "+tc.timeout+
				"\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() = %v, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}

			pc := cfg.Providers["openai"]
			if pc.URL != tc.wantURL || pc.TimeoutSeconds != tc.wantTimeout {
				t.Errorf("url, timeout = %q, %d; want %q, %d", pc.URL, pc.TimeoutSeconds, tc.wantURL, tc.wantTimeout)
			}
		})
	}
}

// TestLoadAuthNoneNeedsNoKey: a keyless upstream such as a local Ollama is
// configured with auth none and no key line at all.
func TestLoadAuthNoneNeedsNoKey(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
providers:
  ollama:
    url: http://127.0.0.1:11434
    auth: none
    format: openai
`))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	pc := cfg.Providers["ollama"]
	if pc.Key != "" || provider.AuthStyle(pc.Auth) != provider.AuthNone {
		t.Errorf("key, auth = %q, %q; want \"\", none", pc.Key, pc.Auth)
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
	// Unset, Ollama's url and timeout take the defaults written in the file.
	unsetenv(t, "OLLAMA_BASE_URL")
	unsetenv(t, "OLLAMA_TIMEOUT_SECONDS")

	path := filepath.Join("..", "..", "config.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() on the repo's config.yaml returned error: %v", err)
	}

	want := []string{"anthropic", "gemini", "groq", "ollama", "openai"}
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
	if url := string(cfg.Providers["groq"].URL); !strings.HasSuffix(url, "/openai") {
		t.Errorf("groq url = %q, want it to keep its /openai base path", url)
	}

	ollama := cfg.Providers["ollama"]
	if ollama.URL != "http://127.0.0.1:11434" || ollama.TimeoutSeconds != 120 {
		t.Errorf("ollama url, timeout = %q, %d; want the defaults http://127.0.0.1:11434, 120", ollama.URL, ollama.TimeoutSeconds)
	}

	t.Setenv("OLLAMA_BASE_URL", "http://10.0.0.5:11434")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load() with OLLAMA_BASE_URL set returned error: %v", err)
	}
	if got := cfg.Providers["ollama"].URL; got != "http://10.0.0.5:11434" {
		t.Errorf("ollama url = %q, want OLLAMA_BASE_URL's %q", got, "http://10.0.0.5:11434")
	}
}

// TestLoadLiteralKeyErrorHidesKey checks the rejection message for a literal
// key doesn't repeat it: it may be a real secret, and startup errors end up in
// logs.
func TestLoadLiteralKeyErrorHidesKey(t *testing.T) {
	_, err := Load(writeConfig(t, "providers:\n  openai:\n    url: https://api.openai.com\n    key: sk-REAL-SECRET\n    auth: bearer\n    format: openai\n"))
	if err == nil {
		t.Fatal("Load() with a literal key = nil error, want an error")
	}
	if strings.Contains(err.Error(), "sk-REAL-SECRET") {
		t.Errorf("error %q repeats the literal key", err)
	}
}

// TestLoadEmptyFile keeps an empty file a validation error that says what's
// missing, rather than a bare decoder EOF.
func TestLoadEmptyFile(t *testing.T) {
	_, err := Load(writeConfig(t, ""))
	if err == nil || !strings.Contains(err.Error(), "at least one provider") {
		t.Errorf("Load() on an empty file = %v, want the missing-providers error", err)
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
	// Every case has a valid key reference unless the key is what's under
	// test, so each fails for the reason its name gives.
	setTestKey(t)

	cases := []struct {
		name string
		yaml string
	}{
		{"no providers at all", "port: 8080\n"},
		{"port zero", "port: 0\n" + minimalProvider},
		{"port too large", "port: 70000\n" + minimalProvider},
		{"empty host", "host: \"\"\n" + minimalProvider},
		{"zero max_request_bytes", "max_request_bytes: 0\n" + minimalProvider},
		{"empty provider url", "providers:\n  openai:\n    url: \"\"\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"url without scheme", "providers:\n  openai:\n    url: api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"url with bad scheme", "providers:\n  openai:\n    url: ftp://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"unknown auth style", "providers:\n  openai:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: basic\n    format: openai\n"},
		{"missing format", "providers:\n  openai:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n"},
		{"unknown format", "providers:\n  openai:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: groq\n"},
		{"missing auth style", "providers:\n  openai:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    format: openai\n"},
		{"negative timeout", "providers:\n  openai:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n    timeout: -5\n"},
		{"empty key", "providers:\n  openai:\n    url: https://api.openai.com\n    key: \"\"\n    auth: bearer\n    format: openai\n"},
		{"reserved name health", "providers:\n  health:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"reserved name admin", "providers:\n  admin:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"reserved name metrics", "providers:\n  metrics:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"reserved name in another case", "providers:\n  Admin:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"reserved name ready", "providers:\n  ready:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"name containing a slash", "providers:\n  open/ai:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		// A space or brace would change the ServeMux pattern and panic.
		{"name containing a space", "providers:\n  \"open ai\":\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"name containing a brace", "providers:\n  \"{openai}\":\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"empty name", "providers:\n  \"\":\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n"},
		{"literal key", "providers:\n  openai:\n    url: https://api.openai.com\n    key: sk-literal\n    auth: bearer\n    format: openai\n"},
		{"key with text around the reference", "providers:\n  openai:\n    url: https://api.openai.com\n    key: ${TEST_KEY}-suffix\n    auth: bearer\n    format: openai\n"},
		{"key as bare $VAR", "providers:\n  openai:\n    url: https://api.openai.com\n    key: $TEST_KEY\n    auth: bearer\n    format: openai\n"},
		// Unlike url and timeout: the default would be a literal key in git.
		{"key with a default", "providers:\n  openai:\n    url: https://api.openai.com\n    key: ${TEST_KEY:-sk-literal}\n    auth: bearer\n    format: openai\n"},
		// auth none would silently ignore the key.
		{"key with auth none", "providers:\n  ollama:\n    url: http://127.0.0.1:11434\n    key: ${TEST_KEY}\n    auth: none\n    format: openai\n"},
		// Unknown keys are typos; ignoring them would apply a default silently.
		{"misspelled provider field", "providers:\n  openai:\n    url: https://api.openai.com\n    key: ${TEST_KEY}\n    auth: bearer\n    format: openai\n    timout: 60\n"},
		{"misspelled top-level field", "prot: 9090\n" + minimalProvider},
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
	t.Setenv("TEST_GROQ_KEY", "sk-groq")
	t.Setenv("TEST_ANT_KEY", "sk-ant")

	cfg, err := Load(writeConfig(t, `
providers:
  groq:
    url: https://api.groq.com/openai
    key: ${TEST_GROQ_KEY}
    auth: bearer
    format: openai
  anthropic:
    url: https://api.anthropic.com
    key: ${TEST_ANT_KEY}
    auth: x-api-key
    format: anthropic
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
	if ant.Format != provider.FormatAnthropic {
		t.Errorf("anthropic Format = %q, want %q", ant.Format, provider.FormatAnthropic)
	}
	// Groq speaks the OpenAI format; the format names a wire shape, not a
	// company.
	if got := providers[1].Format; got != provider.FormatOpenAI {
		t.Errorf("groq Format = %q, want %q", got, provider.FormatOpenAI)
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
