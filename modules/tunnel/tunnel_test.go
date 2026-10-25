package tunnel_test

import (
	"strings"
	"testing"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/internal/testutil"
	"github.com/truvity/asg/modules/tunnel"
)

// connector stands in for what truvity/edge's instanceconnector renders.
func connector() bootstrap.UnitFragment {
	return bootstrap.UnitFragment{
		Install: "echo install-cloudflared\n",
		Unit: "[Unit]\nDescription=tunnel\n\n[Service]\nUser=app\nEnvironmentFile=/run/app/tunnel.env\n" +
			"ExecStart=/usr/local/bin/cloudflared --no-autoupdate tunnel run\n",
		UnitPath:      "/etc/systemd/system/cloudflared.service",
		LoadToken:     "echo load-token\n",
		Start:         "systemctl daemon-reload\nsystemctl start cloudflared.service\n",
		SSMParameters: []string{"/box/tunnel-token"},
	}
}

func TestInstallsEarlyButLoadsTheTokenAndStartsOnlyInService(t *testing.T) {
	c, err := tunnel.New(tunnel.Args{Connector: connector(), MetricsPort: 20241, Watchdog: true}).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	r := testutil.Render(t, c)
	testutil.CheckSyntax(t, r)

	if unit := r.Files["/etc/systemd/system/cloudflared.service"]; strings.Contains(unit, "[Install]") || !strings.Contains(unit, "tunnel run") {
		t.Errorf("unit:\n%s", unit)
	}

	run := strings.Join(r.RunCmd, "\n")
	if !strings.Contains(run, "echo install-cloudflared") {
		t.Error("the connector is not installed on the first boot")
	}

	// The token and the start are in the in-service step, nowhere else.
	step := r.Files["/usr/local/lib/asg/in-service.d/00-tunnel.sh"]
	if !strings.Contains(step, "echo load-token") || !strings.Contains(step, "systemctl start cloudflared.service") {
		t.Errorf("in-service step:\n%s", step)
	}

	if strings.Contains(run, "load-token") || strings.Contains(run, "start cloudflared") ||
		strings.Contains(r.Files["/usr/local/lib/bootstrap/secrets.sh"], "cloudflared") {
		t.Errorf("the token is loaded or the unit started outside the in-service step (a warm-pool instance would hold a connector):\n%s", run)
	}

	if d := r.Files["/etc/systemd/system/cloudflared.service.d/10-bootstrap.conf"]; d != "[Service]\nEnvironment=TUNNEL_METRICS=127.0.0.1:20241\n" {
		t.Errorf("drop-in %q", d)
	}

	if ready := r.Files["/usr/local/lib/asg/ready.d/00-tunnel.sh"]; !strings.Contains(ready, "http://127.0.0.1:20241/ready") {
		t.Errorf("ready check:\n%s", ready)
	}

	if watch := r.Files["/usr/local/lib/asg/watch.d/00-tunnel.sh"]; !strings.Contains(watch, "http://127.0.0.1:20241/ready") {
		t.Errorf("watch check:\n%s", watch)
	}

	if len(c.Scrape) != 1 || c.Scrape[0].Address != "127.0.0.1:20241" || c.Scrape[0].Path != "/metrics" {
		t.Errorf("scrape %+v", c.Scrape)
	}

	if len(c.SSMParameters) != 1 || c.SSMParameters[0] != "/box/tunnel-token" {
		t.Errorf("parameters %v", c.SSMParameters)
	}
}

func TestWithoutMetricsTheChecksAskTheUnit(t *testing.T) {
	c, err := tunnel.New(tunnel.Args{Connector: connector()}).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	active := "systemctl is-active --quiet cloudflared.service"
	if len(c.Watch) != 0 || len(c.Scrape) != 0 || len(c.ReadyChecks) != 1 || !strings.Contains(c.ReadyChecks[0].Script, active) {
		t.Errorf("contribution %+v", c)
	}

	c, _ = tunnel.New(tunnel.Args{Connector: connector(), SkipReadyCheck: true}).Contribute()
	if len(c.ReadyChecks) != 0 {
		t.Error("SkipReadyCheck kept the ready check")
	}
}

func TestRefusals(t *testing.T) {
	enableOnBoot := connector()
	enableOnBoot.Unit += "\n[Install]\nWantedBy=multi-user.target\n"

	badPath := connector()
	badPath.UnitPath = "/opt/cloudflared.service"

	noToken := connector()
	noToken.LoadToken = ""

	for name, a := range map[string]tunnel.Args{
		"install section": {Connector: enableOnBoot},
		"unit path":       {Connector: badPath},
		"no token":        {Connector: noToken},
		"port":            {Connector: connector(), MetricsPort: 80},
		"empty":           {},
	} {
		if _, err := tunnel.New(a).Contribute(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
