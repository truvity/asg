// Package wakeconfig is the configuration file of cmd/wake-proxy. It is a
// package of its own so that the Pulumi-side module can render the same
// structure that the binary reads, with the same validation, without
// importing the AWS SDK.
//
// The file is JSON: the standard library reads it, and a module renders it
// with encoding/json. Unknown fields are refused, so a typo is an error at
// start, not a default nobody chose.
package wakeconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"
)

// Defaults.
const (
	DefaultIdleTimeout   = 20 * time.Minute
	DefaultWakeTimeout   = 180 * time.Second
	DefaultShutdownGrace = 30 * time.Second
	DefaultActivityTag   = "wake-proxy/last-active"
)

// Duration is a time.Duration that reads and writes a Go duration string such
// as "20m" or "180s".
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("want a duration string such as \"20m\": %w", err)
	}

	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}

	*d = Duration(v)

	return nil
}

// Selector picks the target instance by one tag.
type Selector struct {
	TagKey   string `json:"tagKey"`
	TagValue string `json:"tagValue"`
}

// Route is one listening port and the instance it wakes.
type Route struct {
	// Name labels logs and metrics: lower-case letters, digits and -.
	Name string `json:"name"`
	// ListenPort is the TCP port the proxy listens on.
	ListenPort int `json:"listenPort"`
	// Selector must match exactly one instance, running or stopped.
	Selector Selector `json:"selector"`
	// TargetPort is the port on the instance.
	TargetPort int `json:"targetPort"`
}

// Config is the whole file.
type Config struct {
	Region string  `json:"region"`
	Routes []Route `json:"routes"`
	// IdleTimeout is how long a target may go without a connection before
	// it is stopped. Default 20m.
	IdleTimeout Duration `json:"idleTimeout,omitempty"`
	// WakeTimeout is how long a client is held while the target wakes.
	// Default 180s.
	WakeTimeout Duration `json:"wakeTimeout,omitempty"`
	// ShutdownGrace is how long existing streams may finish after SIGTERM.
	// Default 30s.
	ShutdownGrace Duration `json:"shutdownGrace,omitempty"`
	// HealthPort serves GET /healthz: 200 whenever the proxy serves,
	// whether or not a target is up.
	HealthPort int `json:"healthPort"`
	// MetricsAddress, when set (host:port), serves Prometheus text at
	// /metrics. Empty: no metrics listener.
	MetricsAddress string `json:"metricsAddress,omitempty"`
	// ActivityTag is the tag key on the target that carries the last
	// activity time. Default wake-proxy/last-active.
	ActivityTag string `json:"activityTag,omitempty"`
}

// WithDefaults fills the zero values.
func (c Config) WithDefaults() Config {
	if c.IdleTimeout == 0 {
		c.IdleTimeout = Duration(DefaultIdleTimeout)
	}

	if c.WakeTimeout == 0 {
		c.WakeTimeout = Duration(DefaultWakeTimeout)
	}

	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = Duration(DefaultShutdownGrace)
	}

	if c.ActivityTag == "" {
		c.ActivityTag = DefaultActivityTag
	}

	return c
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	tagKeyRe = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,128}$`)
	tagValRe = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,256}$`)
	regionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

func portOK(p int) bool { return p >= 1 && p <= 65535 }

// Validate checks a config that has had its defaults applied.
func (c Config) Validate() error {
	var errs []error

	if !regionRe.MatchString(c.Region) {
		errs = append(errs, fmt.Errorf("region %q: want a region such as eu-west-1", c.Region))
	}

	if len(c.Routes) == 0 {
		errs = append(errs, errors.New("routes: at least one route is required"))
	}

	if c.IdleTimeout <= 0 || c.WakeTimeout <= 0 || c.ShutdownGrace <= 0 {
		errs = append(errs, errors.New("idleTimeout, wakeTimeout and shutdownGrace must be positive"))
	}

	if !portOK(c.HealthPort) {
		errs = append(errs, fmt.Errorf("healthPort %d: want 1..65535", c.HealthPort))
	}

	if !tagKeyRe.MatchString(c.ActivityTag) {
		errs = append(errs, fmt.Errorf("activityTag %q: not a valid tag key", c.ActivityTag))
	}

	names := map[string]bool{}
	ports := map[int]string{c.HealthPort: "healthPort"}

	for i, r := range c.Routes {
		where := fmt.Sprintf("routes[%d]", i)

		switch {
		case !nameRe.MatchString(r.Name):
			errs = append(errs, fmt.Errorf("%s: name %q: want lower-case letters, digits and -", where, r.Name))
		case names[r.Name]:
			errs = append(errs, fmt.Errorf("%s: name %q is used twice", where, r.Name))
		}

		names[r.Name] = true

		if !portOK(r.ListenPort) || !portOK(r.TargetPort) {
			errs = append(errs, fmt.Errorf("%s: listenPort %d and targetPort %d: want 1..65535", where, r.ListenPort, r.TargetPort))
		}

		if other, dup := ports[r.ListenPort]; dup {
			errs = append(errs, fmt.Errorf("%s: listenPort %d is already used by %s", where, r.ListenPort, other))
		}

		ports[r.ListenPort] = where

		if !tagKeyRe.MatchString(r.Selector.TagKey) || !tagValRe.MatchString(r.Selector.TagValue) {
			errs = append(errs, fmt.Errorf("%s: selector wants a tag key and a tag value", where))
		}

		if r.Selector.TagKey == c.ActivityTag {
			errs = append(errs, fmt.Errorf("%s: the selector tag %q is the activity tag", where, c.ActivityTag))
		}
	}

	return errors.Join(errs...)
}

// Parse reads and validates a config file's bytes.
func Parse(b []byte) (Config, error) {
	var c Config

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}

	if dec.More() {
		return Config{}, errors.New("config: trailing data after the JSON object")
	}

	c = c.WithDefaults()

	return c, c.Validate()
}

// Load reads and validates the config file at path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}

	return Parse(b)
}
