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
	"strings"
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
	// provider API keys; tenant keys guard them, but exposing the port beyond
	// this machine (no TLS, no rate limits yet) is a decision the operator has
	// to make explicitly.
	DefaultHost = "127.0.0.1"
	// DefaultMaxRequestBytes caps a request body at 32 MiB, Anthropic's own
	// request limit: room for base64 images and PDFs, while a runaway client
	// can't make the gateway buffer gigabytes.
	DefaultMaxRequestBytes = 32 << 20
	// DefaultQueueTimeoutSeconds bounds a request's wait for a concurrency
	// slot, on a provider with max_concurrency and no queue_timeout.
	DefaultQueueTimeoutSeconds = 30
)

// validName is what a provider name may contain. The name becomes a URL path
// segment and a ServeMux pattern, where a space or a brace changes the
// pattern's meaning and panics at startup instead of failing validation.
var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// reservedNames are provider names that would collide with the gateway's own
// routes: a provider called "health" would shadow GET /health, and one called
// "admin" would catch and proxy every /admin/ path the admin API doesn't
// serve. Checked case-insensitively, so "Admin" can't sit beside "admin".
var reservedNames = map[string]bool{
	"health":  true,
	"admin":   true,
	"metrics": true,
	"ready":   true,
}

// Config is the top-level shape of config.yaml.
//
// Go note: the strings in backticks after each field are "struct tags" —
// metadata that libraries read at runtime via reflection. The YAML parser uses
// them to map file keys onto fields.
type Config struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
	// MaxRequestBytes is the largest request body the gateway reads; larger
	// ones get 413. The gateway buffers each body to meter and rewrite it.
	MaxRequestBytes int64                     `yaml:"max_request_bytes"`
	Providers       map[string]ProviderConfig `yaml:"providers"`
}

// ProviderConfig is one entry under `providers:` in the YAML file.
//
// This is the on-disk shape. Provider() turns it into a provider.Provider,
// which is the parsed, ready-to-use form the rest of the gateway consumes.
type ProviderConfig struct {
	// URL may be written as an environment reference with a default, such as
	// ${OLLAMA_BASE_URL:-http://127.0.0.1:11434}; see decodeEnv.
	URL envString `yaml:"url"`
	// TimeoutSeconds is an int, not a Duration, because that's what the YAML
	// file holds. Like URL, it may be an environment reference.
	TimeoutSeconds envInt `yaml:"timeout"`
	// Key is written as an environment reference such as ${OPENAI_API_KEY}.
	// Load expands it. config.yaml is committed to git, so a literal key must
	// never be written here. With auth none there is no key, so it must be
	// absent.
	Key  string `yaml:"key"`
	Auth string `yaml:"auth"`
	// Format is the provider's API shape (openai, anthropic, gemini). It is
	// required: it decides how usage is read, and a guess would mis-meter.
	Format  string            `yaml:"format"`
	Headers map[string]string `yaml:"headers"`
	// Free marks a provider that costs nothing, such as a local Ollama: each
	// usage row is priced at exactly 0, with its tokens still recorded.
	Free bool `yaml:"free"`
	// MaxConcurrency caps the requests in flight to this provider; 0, the
	// default, means no cap. A request beyond it waits for a slot, for up to
	// QueueTimeoutSeconds, and then gets 503.
	MaxConcurrency      envInt `yaml:"max_concurrency"`
	QueueTimeoutSeconds envInt `yaml:"queue_timeout"`
	// HealthPath, when set, is a path on the provider that the gateway GETs
	// every 15 seconds to check it is up, such as Ollama's /api/tags.
	HealthPath string `yaml:"health_path"`
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
		Port:            DefaultPort,
		Host:            DefaultHost,
		MaxRequestBytes: DefaultMaxRequestBytes,
		Providers:       map[string]ProviderConfig{},
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
// Only named fields are expanded, never the whole file: keys here, and fields
// of the env types as they are decoded (decodeEnv). Running os.ExpandEnv over
// the raw YAML would also mangle any other value containing a `$`.
func (c *Config) resolve() error {
	for name, pc := range c.Providers {
		if pc.TimeoutSeconds == 0 {
			pc.TimeoutSeconds = DefaultTimeoutSeconds
		}
		if pc.MaxConcurrency > 0 && pc.QueueTimeoutSeconds == 0 {
			pc.QueueTimeoutSeconds = DefaultQueueTimeoutSeconds
		}

		if provider.AuthStyle(pc.Auth) == provider.AuthNone {
			// A key here would be silently ignored, so it's an error. Its
			// value stays out of the message: it may be a real key.
			if pc.Key != "" {
				return fmt.Errorf("provider %q: auth none sends no key, so key must be absent", name)
			}
		} else {
			key, err := expandKey(name, pc.Key)
			if err != nil {
				return err
			}
			pc.Key = key
		}

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

// fieldRef matches a value written as exactly one environment reference with
// an optional default, such as ${OLLAMA_TIMEOUT_SECONDS:-120}. It captures the
// variable name and, if there is one, ":-" and the default.
var fieldRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}$`)

// envString is a string field that may be written as an environment
// reference; see decodeEnv.
type envString string

// envInt is an int field that may be written as an environment reference;
// see decodeEnv.
type envInt int

// UnmarshalYAML decodes s with decodeEnv.
//
// Go note: implementing yaml.Unmarshaler hooks the decoding of each field of
// this type, while the Decoder's KnownFields check still covers the mapping
// around it. Decoding into (*string)(s) rather than s matters: s has this
// method, so decoding into it would call this method again, forever.
func (s *envString) UnmarshalYAML(n *yaml.Node) error { return decodeEnv(n, (*string)(s)) }

// UnmarshalYAML decodes i with decodeEnv.
func (i *envInt) UnmarshalYAML(n *yaml.Node) error { return decodeEnv(n, (*int)(i)) }

// decodeEnv decodes n into out, expanding it first if it is one whole
// environment reference: ${VAR}, or ${VAR:-default}, which as in the shell
// takes the default when VAR is unset or empty. ${VAR} with VAR unset or
// empty is an error, so a missing setting fails at startup.
//
// The expansion is decoded as if written in the file unquoted, even when the
// reference was quoted: ${OLLAMA_TIMEOUT_SECONDS:-120} is an int, and a bad
// value is the usual type error with its line number. Anything that isn't one
// whole reference, such as http://${HOST} or $VAR, is the literal it looks
// like.
//
// n.Decode runs a fresh decoder without KnownFields. That is harmless here:
// KnownFields only checks mappings, and neither field type is one.
func decodeEnv(n *yaml.Node, out any) error {
	if m := fieldRef.FindStringSubmatch(n.Value); m != nil {
		value := os.Getenv(m[1])
		if value == "" {
			def, ok := strings.CutPrefix(m[2], ":-")
			if !ok {
				return fmt.Errorf("line %d: environment variable %s is unset or empty, and %s has no default", n.Line, m[1], n.Value)
			}
			value = def
		}
		// A copy, so the decoder's own tree is untouched. With no Tag or
		// Style, the value's type is resolved from its text, as for a plain
		// scalar; a quoted style would force a string.
		plain := *n
		plain.Value, plain.Tag, plain.Style = value, "", 0
		n = &plain
	}
	return n.Decode(out)
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
	if c.MaxRequestBytes <= 0 {
		return fmt.Errorf("max_request_bytes must be greater than 0, got %d", c.MaxRequestBytes)
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
	if reservedNames[strings.ToLower(name)] {
		return fmt.Errorf("provider name %q is reserved: it collides with the gateway's own /%s routes", name, strings.ToLower(name))
	}

	if pc.URL == "" {
		return fmt.Errorf("provider %q: url must not be empty", name)
	}
	u, err := url.Parse(string(pc.URL))
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

	if !provider.Format(pc.Format).Valid() {
		return fmt.Errorf("provider %q: format must be one of %v, got %q (Groq and other OpenAI-compatible APIs are openai)", name, provider.Formats, pc.Format)
	}

	if pc.TimeoutSeconds <= 0 {
		return fmt.Errorf("provider %q: timeout must be greater than 0, got %d", name, pc.TimeoutSeconds)
	}

	if pc.MaxConcurrency < 0 {
		return fmt.Errorf("provider %q: max_concurrency must be 0 (no cap) or more, got %d", name, pc.MaxConcurrency)
	}
	if pc.QueueTimeoutSeconds < 0 {
		return fmt.Errorf("provider %q: queue_timeout must not be negative, got %d", name, pc.QueueTimeoutSeconds)
	}

	if pc.HealthPath != "" && !strings.HasPrefix(pc.HealthPath, "/") {
		return fmt.Errorf("provider %q: health_path must start with /, got %q", name, pc.HealthPath)
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

		u, err := url.Parse(string(pc.URL))
		if err != nil {
			return nil, fmt.Errorf("provider %q: url is not valid: %w", name, err)
		}

		providers = append(providers, provider.Provider{
			Name:           name,
			URL:            u,
			Timeout:        pc.Timeout(),
			Key:            pc.Key,
			Auth:           provider.AuthStyle(pc.Auth),
			Format:         provider.Format(pc.Format),
			Headers:        pc.Headers,
			MaxConcurrency: int(pc.MaxConcurrency),
			QueueTimeout:   time.Duration(pc.QueueTimeoutSeconds) * time.Second,
			HealthPath:     pc.HealthPath,
		})
	}

	return providers, nil
}

// Addr returns the listen address in the form net/http expects.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}
