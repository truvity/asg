// Package tunnel is a module that runs a Cloudflare tunnel connector
// (cloudflared) on the box, in the right phases: the binary and the unit are
// installed on the first boot, warm-pool instances included, but the token is
// read from SSM and the unit started only on a boot into service, never in
// the warm pool. A connector in the warm pool would take traffic that the
// instance cannot serve.
//
// The module takes the connector as a [bootstrap.UnitFragment], which the
// caller copies field by field from truvity/edge's cloudflare/instanceconnector
// (the seam of ADR 0001: neither module imports the other, and the cloudflared
// pin stays in edge). The tunnel, its ingress rules and its DNS records are
// resources and belong to the caller's stack; the token reaches the box as an
// SSM SecureString whose name is in the connector. No Cloudflare API token
// ever goes on a box.
//
// With MetricsPort the connector listens for metrics on the loopback and the
// module adds a ready check, so the launch hook is completed only once
// cloudflared holds an edge connection, and a watchdog check. Without it the
// checks only ask whether the unit is active, which a connector with no edge
// connection also is.
package tunnel

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"
)

// DefaultOrder is the order of the in-service step: after the origins the
// tunnel serves were started (a consumer's own step at 0).
const DefaultOrder = 100

// Args configure the tunnel connector.
type Args struct {
	// Connector is the rendered connector: Install, Unit, UnitPath, LoadToken,
	// Start and SSMParameters. Its UnitPath must be
	// /etc/systemd/system/<unit>.service and the unit must have no [Install]
	// section.
	Connector bootstrap.UnitFragment
	// Order of the in-service step that loads the token and starts the unit.
	// Default 100.
	Order int
	// MetricsPort, when set (1024..65535), makes cloudflared serve its metrics
	// on 127.0.0.1:<port> (TUNNEL_METRICS) and enables the /ready check.
	MetricsPort int
	// SkipReadyCheck leaves the launch hook alone: it completes whether or
	// not the tunnel connected.
	SkipReadyCheck bool
	// Watchdog adds the same check to the watchdog module's checks.
	Watchdog bool
}

type mod struct{ a Args }

// New returns the tunnel module.
func New(a Args) module.Module { return mod{a: a} }

func (mod) Name() string { return "tunnel" }

func (m mod) Contribute() (module.Contribution, error) {
	a := m.a
	if a.Order == 0 {
		a.Order = DefaultOrder
	}

	c := a.Connector

	var errs []error

	if c.Install == "" || c.Unit == "" || c.LoadToken == "" || c.Start == "" {
		errs = append(errs, errors.New("connector: Install, Unit, LoadToken and Start are required (copy them from instanceconnector.Bootstrap)"))
	}

	dir, file := path.Split(c.UnitPath)
	unit, isService := strings.CutSuffix(file, ".service")

	if dir != "/etc/systemd/system/" || !isService || unit == "" {
		errs = append(errs, fmt.Errorf("Connector.UnitPath %q: want /etc/systemd/system/<unit>.service", c.UnitPath))
	}

	if strings.Contains(c.Unit, "[Install]") {
		errs = append(errs, errors.New("Connector.Unit has an [Install] section: the unit would start in the warm pool; build the connector without EnableOnBoot"))
	}

	if a.MetricsPort != 0 && (a.MetricsPort < 1024 || a.MetricsPort > 65535) {
		errs = append(errs, fmt.Errorf("MetricsPort %d: want 0 or 1024..65535", a.MetricsPort))
	}

	if err := errors.Join(errs...); err != nil {
		return module.Contribution{}, err
	}

	// RunManual: install and the unit file on the first boot; the token and
	// the start belong to the in-service step.
	items := []bootstrap.Fragment{c.Fragment("tunnel", bootstrap.RunManual)}

	out := module.Contribution{
		SSMParameters: c.SSMParameters,
		InService:     []module.Step{{Name: "tunnel", Order: a.Order, Script: strings.TrimRight(c.LoadToken, "\n") + "\n" + c.Start}},
	}

	check := fmt.Sprintf("systemctl is-active --quiet %s.service\n", unit)

	if a.MetricsPort != 0 {
		addr := fmt.Sprintf("127.0.0.1:%d", a.MetricsPort)
		items = append(items, bootstrap.Compose("tunnel metrics",
			bootstrap.Unit{Name: unit + ".service", DropIn: true, Content: "[Service]\nEnvironment=TUNNEL_METRICS=" + addr + "\n"}))
		check = fmt.Sprintf("curl -fsS -m 3 -o /dev/null http://%s/ready\n", addr)
		out.Scrape = []module.ScrapeTarget{{Name: unit, Address: addr, Path: "/metrics"}}
	}

	out.Fragments = items

	if !a.SkipReadyCheck {
		out.ReadyChecks = []module.ReadyCheck{{Name: "tunnel", Script: check}}
	}

	if a.Watchdog {
		out.Watch = []module.WatchCheck{{Name: "tunnel", Script: check}}
	}

	return out, nil
}
