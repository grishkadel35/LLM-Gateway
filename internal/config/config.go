// Package config loads the gateway's settings from a YAML file.
//
// Go note: every file starts with a `package` clause. A directory is a package,
// and the directory name and package name are the same by convention. Anything
// under internal/ can only be imported by code inside this module — the Go
// toolchain enforces that, so it's a good place for implementation details.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// Default values used when a key is absent from config.yaml.
const (
	DefaultPort = 8080
	// DefaultTimeoutSeconds bounds the wait for a provider's response headers.
	// A non-streaming completion sends no headers until generation finishes,
	// which can take minutes, so this matches the official SDKs' own 10-minute
	// default: the gateway should never give up before the client would.
	DefaultTimeoutSeconds = 600
	// DefaultHost is deliberately loopback-only. The gateway holds real
	// provider API keys but has no tenant authentication yet, so anything that
	// can reach the port can spend them. Binding to 0.0.0.0 is a decision the
	// operator has to make explicitly.
	DefaultHost = "127.0.0.1"
)

// validName is what a provider name may contain. The name becomes a URL path
// segment and a ServeMux pattern, where a space or a brace changes the
// pattern's meaning and panics at startup instead of failing validation.
var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// reservedNames are provider names that would collide with the gateway's own
// routes. A provider called "health" would shadow GET /health.
var reservedNames = map[string]bool{
	"health": true,
}

// Config is the top-level shape of config.yaml.
//
// Go note: the strings in backticks after each field are "struct tags" —
// metadata that libraries read at runtime via reflection. The YAML parser uses
// them to map file keys onto fields.
type Config struct {
	Port      int                       `yaml:"port"`
	Host      string                    `yaml:"host"`
	Providers map[string]ProviderConfig `yaml:"providers"`
}

// ProviderConfig is one entry under `providers:` in the YAML file.
//
// This is the on-disk shape. Provider() turns it into a provider.Provider,
// which is the parsed, ready-to-use form the rest of the gateway consumes.
type ProviderConfig struct {
	URL string `yaml:"url"`
	// TimeoutSeconds is a plain int because that's what the YAML file holds.
	TimeoutSeconds int `yaml:"timeout"`
	// Key is written as an environment reference such as ${OPENAI_API_KEY}.
	// Load expands it. config.yaml is committed to git, so a literal key must
	// never be written here.
	Key     string            `yaml:"key"`
	Auth    string            `yaml:"auth"`
	Headers map[string]string `yaml:"headers"`
}

// Timeout returns the configured timeout as a time.Duration.
//
// Go note: Durations are a distinct type, not a number of seconds, so the
// conversion is explicit — Go won't silently mix an int with a Duration.
func (pc ProviderConfig) Timeout() time.Duration {
	return time.Duration(pc.TimeoutSeconds) * time.Second
}

// Default returns a Config with the built-in defaults and no providers.
func Default() *Config {
	return &Config{
		Port:      DefaultPort,
		Host:      DefaultHost,
		Providers: map[string]ProviderConfig{},
	}
}

// Load reads config.yaml from path and returns the resulting Config.
//
// Keys missing from the file keep their default values, because we unmarshal
// into an already-populated struct and the YAML parser only touches fields it
// finds in the document.
//
// Go note: errors are values that get returned, not exceptions that get raised.
// `%w` in fmt.Errorf wraps the original error so callers can still inspect it
// with errors.Is / errors.As while we add context to the message.
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	// KnownFields makes an unrecognised key an error. Without it a typo such as
	// "timout: 60" is silently ignored and the default applies instead. An
	// empty file decodes to io.EOF; it falls through to Validate, which
	// reports what's missing.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	if err := cfg.resolve(); err != nil {
		return nil, fmt.Errorf("in config file %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config file %q: %w", path, err)
	}

	return cfg, nil
}

// resolve fills in per-provider defaults and expands API keys from the
// environment.
//
// Only the key field is expanded, not the whole file: running os.ExpandEnv over
// the raw YAML would also mangle any other value containing a `$`.
func (c *Config) resolve() error {
	for name, pc := range c.Providers {
		if pc.TimeoutSeconds == 0 {
			pc.TimeoutSeconds = DefaultTimeoutSeconds
		}

		key, err := expandKey(name, pc.Key)
		if err != nil {
			return err
		}
		pc.Key = key

		// Go note: ranging over a map gives you a *copy* of each value, so
		// mutating pc above changes nothing until we write it back.
		c.Providers[name] = pc
	}
	return nil
}

// envRef matches a key written as exactly one environment reference, such as
// ${OPENAI_API_KEY}, and captures the variable name.
var envRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// expandKey resolves a ${VAR} reference against the environment.
//
// The whole value must be one reference. A literal key would end up committed
// to git, and a partial one ("${KEY}-suffix") would turn an unset variable
// into a plausible-looking wrong key rather than an error.
//
// An unset variable is a startup error rather than an empty key, so a
// misconfigured deployment fails immediately instead of on its first request
// with a confusing 401 from the provider.
func expandKey(name, raw string) (string, error) {
	m := envRef.FindStringSubmatch(raw)
	if m == nil {
		// raw is deliberately left out of the message: it may be a real key.
		return "", fmt.Errorf("provider %q: key must be a single environment reference such as ${PROVIDER_API_KEY}, never a literal", name)
	}

	value := os.Getenv(m[1])
	if value == "" {
		return "", fmt.Errorf("provider %q: key %q resolves to an empty value; is that environment variable set?", name, raw)
	}
	return value, nil
}

// Validate checks that the config makes sense before we try to serve traffic.
// Failing loudly at startup beats a confusing error on the first request.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if c.Host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider must be configured")
	}

	for _, name := range c.ProviderNames() {
		if err := c.Providers[name].validate(name); err != nil {
			return err
		}
	}
	return nil
}

// validate checks a single provider entry.
func (pc ProviderConfig) validate(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("provider name %q must be non-empty and contain only letters, digits, '-' and '_': it is used as a URL path prefix", name)
	}
	if reservedNames[name] {
		return fmt.Errorf("provider name %q is reserved: it would shadow the gateway's own /%s route", name, name)
	}

	if pc.URL == "" {
		return fmt.Errorf("provider %q: url must not be empty", name)
	}
	u, err := url.Parse(pc.URL)
	if err != nil {
		return fmt.Errorf("provider %q: url is not valid: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("provider %q: url must start with http:// or https://, got %q", name, pc.URL)
	}
	if u.Host == "" {
		return fmt.Errorf("provider %q: url must include a host, got %q", name, pc.URL)
	}

	if !provider.AuthStyle(pc.Auth).Valid() {
		return fmt.Errorf("provider %q: auth must be one of %v, got %q", name, provider.AuthStyles, pc.Auth)
	}

	if pc.TimeoutSeconds <= 0 {
		return fmt.Errorf("provider %q: timeout must be greater than 0, got %d", name, pc.TimeoutSeconds)
	}

	return nil
}

// ProviderNames returns the configured provider names in sorted order.
//
// Go note: iterating a Go map yields keys in a deliberately random order, so
// anything user-visible (log lines, error messages, route registration) sorts
// first to stay deterministic.
func (c *Config) ProviderNames() []string {
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BuildProviders converts the config into the parsed form the proxy consumes,
// sorted by name.
func (c *Config) BuildProviders() ([]provider.Provider, error) {
	names := c.ProviderNames()
	providers := make([]provider.Provider, 0, len(names))

	for _, name := range names {
		pc := c.Providers[name]

		u, err := url.Parse(pc.URL)
		if err != nil {
			return nil, fmt.Errorf("provider %q: url is not valid: %w", name, err)
		}

		providers = append(providers, provider.Provider{
			Name:    name,
			URL:     u,
			Timeout: pc.Timeout(),
			Key:     pc.Key,
			Auth:    provider.AuthStyle(pc.Auth),
			Headers: pc.Headers,
		})
	}

	return providers, nil
}

// Addr returns the listen address in the form net/http expects.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}
