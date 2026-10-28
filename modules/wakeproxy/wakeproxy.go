// Package wakeproxy is a module that installs the wake proxy
// (cmd/wake-proxy, docs/decisions/0006-wake-proxy.md) on an appliance.
//
// The proxy is a small always-on TCP pass-through that starts a stopped
// instance when a client connects, holds the client while it wakes, pipes the
// bytes and stops the instance after a period without connections. The
// module:
//
//   - downloads the release build for the box's CPU architecture, pinned by
//     URL and sha256 (bootstrap.Binary), on the first boot, warm pool
//     included;
//   - writes the proxy's JSON configuration, which holds no secret;
//   - installs a hardened systemd unit (a dynamic user, no capabilities
//     unless a listen port is below 1024, Restart=always) with no [Install]
//     section, and starts it only on a boot into service, so a warm-pool
//     instance never answers for builders it is not in front of;
//   - adds a ready check and a watchdog check on the health port, which
//     reports on the proxy and never on a target, because a sleeping target
//     is the normal state;
//   - with Account set, contributes the role statements of [Statements].
//
// The box is an ordinary appliance in the Auto Scaling group mode behind a
// front NLB; the module needs the launch hook (it has in-service steps).
package wakeproxy

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/internal/wakeconfig"
	"github.com/truvity/asg/module"
)

// Defaults and names.
const (
	DefaultActivityTag = wakeconfig.DefaultActivityTag
	DefaultHealthPort  = 8080

	// StartOrder is the order of the in-service step that starts the proxy.
	StartOrder = 100

	// UnitName is the proxy's service.
	UnitName = "wake-proxy.service"

	// BinaryPath and ConfigPath are where the module installs them.
	BinaryPath = "/usr/local/bin/wake-proxy"
	ConfigPath = "/etc/wake-proxy/config.json"
)

// Route is one listening port and the instance it wakes.
type Route struct {
	// Name labels logs and metrics: lower-case letters, digits and -.
	Name string
	// ListenPort is the port the NLB's target group sends to.
	ListenPort int
	// Target is the tag that selects exactly one instance, running or
	// stopped. The proxy refuses a connection when it matches none or
	// several.
	Target Selector
	// TargetPort is the port on that instance (for example buildkitd's).
	TargetPort int
}

// Args configure the module.
type Args struct {
	// Binary is the pinned release build per CPU architecture ("amd64" and
	// "arm64"; give the one the box runs, or both). Use [ReleaseURL] for the
	// URL and the release's checksums.txt for the digest. Required.
	Binary map[string]bootstrap.Source
	// Region of the targets. Required.
	Region string
	// Routes, one per target. Required.
	Routes []Route
	// HealthPort serves GET /healthz for the NLB health check. Default 8080.
	HealthPort int
	// IdleTimeout is how long a target may go without a connection before
	// it is stopped. Default 20m.
	IdleTimeout time.Duration
	// WakeTimeout is how long a client is held while its target wakes.
	// Default 180s.
	WakeTimeout time.Duration
	// ShutdownGrace is how long open streams may finish on SIGTERM. Default
	// 30s.
	ShutdownGrace time.Duration
	// MetricsAddress, when set (host:port, such as 127.0.0.1:9102), serves
	// Prometheus text at /metrics and is recorded as a scrape target.
	MetricsAddress string
	// ActivityTag is the tag the proxies write on the target. Default
	// wake-proxy/last-active.
	ActivityTag string

	// Account, Partition and KMSKeyARNs, when Account is set, make the
	// module contribute [Statements] for the Routes' targets. Partition
	// defaults to aws. Leave Account empty to attach the statements
	// yourself.
	Account    string
	Partition  string
	KMSKeyARNs []string
}

type mod struct{ a Args }

// New returns the wake proxy module.
func New(a Args) module.Module { return mod{a: a} }

func (mod) Name() string { return "wakeproxy" }

// ReleaseURL is the URL of the proxy build of a release of this repository
// for a CPU architecture ("amd64" or "arm64"). version is the tag, such as
// v0.6.0.
func ReleaseURL(version, arch string) string {
	return fmt.Sprintf("https://github.com/truvity/asg/releases/download/%s/wake-proxy_%s_linux_%s", version, trimV(version), arch)
}

func trimV(v string) string {
	if len(v) > 0 && v[0] == 'v' {
		return v[1:]
	}

	return v
}

// Config returns the file the proxy reads, validated.
func (a Args) Config() (wakeconfig.Config, error) {
	c := wakeconfig.Config{
		Region:         a.Region,
		HealthPort:     a.HealthPort,
		IdleTimeout:    wakeconfig.Duration(a.IdleTimeout),
		WakeTimeout:    wakeconfig.Duration(a.WakeTimeout),
		ShutdownGrace:  wakeconfig.Duration(a.ShutdownGrace),
		MetricsAddress: a.MetricsAddress,
		ActivityTag:    a.ActivityTag,
	}

	if c.HealthPort == 0 {
		c.HealthPort = DefaultHealthPort
	}

	for _, r := range a.Routes {
		c.Routes = append(c.Routes, wakeconfig.Route{
			Name: r.Name, ListenPort: r.ListenPort, TargetPort: r.TargetPort,
			Selector: wakeconfig.Selector{TagKey: r.Target.TagKey, TagValue: r.Target.TagValue},
		})
	}

	c = c.WithDefaults()

	return c, c.Validate()
}

func (m mod) Contribute() (module.Contribution, error) {
	a := m.a

	cfg, err := a.Config()
	if err != nil {
		return module.Contribution{}, err
	}

	if len(a.Binary) == 0 {
		return module.Contribution{}, errors.New("binary: the pinned release build is required (see ReleaseURL)")
	}

	// The file as written is the file as validated: defaults are in it, so
	// a later change of a default in this library cannot change a box.
	body, err := jsonIndent(cfg)
	if err != nil {
		return module.Contribution{}, err
	}

	privileged := cfg.HealthPort < 1024

	for _, r := range cfg.Routes {
		privileged = privileged || r.ListenPort < 1024
	}

	health := "http://127.0.0.1:" + strconv.Itoa(cfg.HealthPort) + "/healthz"
	check := "curl -fsS -m 3 -o /dev/null " + health + "\n"

	c := module.Contribution{
		Fragments: []bootstrap.Fragment{bootstrap.Compose("wakeproxy",
			bootstrap.Binary{Name: "wake-proxy", Dest: BinaryPath, PerArch: a.Binary},
			bootstrap.File{Path: ConfigPath, Mode: "0644", Delivery: bootstrap.DeliveryInline, Content: body},
			// No [Install]: only a boot into service starts it.
			bootstrap.Unit{Name: UnitName, Content: unit(cfg, privileged)},
		)},
		InService:   []module.Step{{Name: "wake-proxy", Order: StartOrder, Script: "systemctl start " + UnitName + "\n"}},
		ReadyChecks: []module.ReadyCheck{{Name: "wake-proxy", Script: check}},
		Watch:       []module.WatchCheck{{Name: "wake-proxy", Script: check}},
	}

	if cfg.MetricsAddress != "" {
		c.Scrape = []module.ScrapeTarget{{Name: "wake-proxy", Address: cfg.MetricsAddress, Path: "/metrics"}}
	}

	if a.Account != "" {
		in := PolicyInput{Partition: a.Partition, Region: a.Region, Account: a.Account, ActivityTag: cfg.ActivityTag, KMSKeyARNs: a.KMSKeyARNs}
		if in.Partition == "" {
			in.Partition = "aws"
		}

		for _, r := range a.Routes {
			in.Targets = append(in.Targets, r.Target)
		}

		st, err := Statements(in)
		if err != nil {
			return module.Contribution{}, err
		}

		c.Statements = st
	}

	return c, nil
}

func unit(cfg wakeconfig.Config, privileged bool) string {
	caps := "CapabilityBoundingSet=\n"
	if privileged {
		caps = "CapabilityBoundingSet=CAP_NET_BIND_SERVICE\nAmbientCapabilities=CAP_NET_BIND_SERVICE\n"
	}

	return "[Unit]\nDescription=Wake proxy: starts stopped instances when a client connects\n" +
		"Wants=network-online.target\nAfter=network-online.target\n\n" +
		"[Service]\nExecStart=" + BinaryPath + " -config " + ConfigPath + "\n" +
		"Restart=always\nRestartSec=2\n" +
		// SIGTERM, then the grace plus a margin before SIGKILL.
		"KillSignal=SIGTERM\nTimeoutStopSec=" + strconv.Itoa(int(cfg.ShutdownGrace.D().Seconds())+15) + "\n" +
		"DynamicUser=yes\nUMask=0077\n" + caps +
		"NoNewPrivileges=yes\nProtectSystem=strict\nProtectHome=yes\nPrivateTmp=yes\nPrivateDevices=yes\n" +
		"ProtectKernelTunables=yes\nProtectKernelModules=yes\nProtectKernelLogs=yes\nProtectControlGroups=yes\nProtectClock=yes\nProtectHostname=yes\n" +
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX\nRestrictNamespaces=yes\nRestrictRealtime=yes\nRestrictSUIDSGID=yes\n" +
		"LockPersonality=yes\nMemoryDenyWriteExecute=yes\nSystemCallArchitectures=native\nSystemCallFilter=@system-service\n"
}
