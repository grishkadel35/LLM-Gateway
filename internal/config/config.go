// Package config loads the gateway's settings from a YAML file.
//
// Go note: every file starts with a `package` clause. A directory is a package,
// and the directory name and package name are the same by convention. Anything
// under internal/ can only be imported by code inside this module — the Go
// toolchain enforces that, so it's a good place for implementation details.
package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Default values used when a key is absent from config.yaml.
const (
	DefaultPort           = 8080
	DefaultUpstreamURL    = "https://api.openai.com"
	DefaultTimeoutSeconds = 30
)

// Config is the top-level shape of config.yaml.
//
// Go note: the strings in backticks after each field are "struct tags" —
// metadata that libraries read at runtime via reflection. The YAML parser uses
// them to map file keys onto fields. Coming from Python, think of it as being
// explicit about the wire format instead of relying on attribute names.
//
// Go note: capitalization is visibility. A name starting with an uppercase
// letter is exported (other packages can see it); lowercase is private to this
// package. The YAML parser lives in another package, so these fields must be
// exported for it to fill them in.
type Config struct {
	Port     int      `yaml:"port"`
	Upstream Upstream `yaml:"upstream"`
}

// Upstream describes the LLM provider we forward requests to.
type Upstream struct {
	URL string `yaml:"url"`
	// TimeoutSeconds is stored as a plain int because that's what the YAML
	// file holds. Timeout() converts it to the type the net/http package
	// actually wants.
	TimeoutSeconds int `yaml:"timeout"`
}

// Timeout returns the configured timeout as a time.Duration.
//
// Go note: this is a "method with a value receiver" — `u Upstream` before the
// name. It's roughly Python's `self`, except you choose whether the method gets
// a copy (value receiver, like here) or a pointer it can mutate (*Upstream).
// Durations are a distinct type, not a number of seconds, so the conversion is
// explicit: Go won't silently mix an int with a Duration.
func (u Upstream) Timeout() time.Duration {
	return time.Duration(u.TimeoutSeconds) * time.Second
}

// Default returns a Config populated with the built-in defaults.
//
// Go note: returning *Config (a pointer) is the common choice for structs that
// get passed around and filled in. There's no `new` keyword ceremony — &Config{}
// allocates and yields a pointer, and Go's garbage collector handles the rest.
func Default() *Config {
	return &Config{
		Port: DefaultPort,
		Upstream: Upstream{
			URL:            DefaultUpstreamURL,
			TimeoutSeconds: DefaultTimeoutSeconds,
		},
	}
}

// Load reads config.yaml from path and returns the resulting Config.
//
// Keys missing from the file keep their default values, because we unmarshal
// *into* an already-populated struct and the YAML parser only touches fields it
// finds in the document.
//
// Go note: errors are values that get returned, not exceptions that get raised.
// The `(T, error)` return pair is the standard shape; the caller decides what to
// do. `%w` in fmt.Errorf wraps the original error so callers can still inspect
// it with errors.Is / errors.As while we add context to the message.
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config file %q: %w", path, err)
	}

	return cfg, nil
}

// Validate checks that the config makes sense before we try to serve traffic.
// Failing loudly at startup beats a confusing error on the first request.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}

	if c.Upstream.URL == "" {
		return fmt.Errorf("upstream.url must not be empty")
	}

	u, err := url.Parse(c.Upstream.URL)
	if err != nil {
		return fmt.Errorf("upstream.url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("upstream.url must start with http:// or https://, got %q", c.Upstream.URL)
	}
	if u.Host == "" {
		return fmt.Errorf("upstream.url must include a host, got %q", c.Upstream.URL)
	}

	if c.Upstream.TimeoutSeconds <= 0 {
		return fmt.Errorf("upstream.timeout must be greater than 0, got %d", c.Upstream.TimeoutSeconds)
	}

	return nil
}

// Addr returns the listen address in the form net/http expects, e.g. ":8080".
func (c *Config) Addr() string {
	return fmt.Sprintf(":%d", c.Port)
}
