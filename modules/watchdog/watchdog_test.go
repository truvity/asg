package watchdog_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/truvity/asg/internal/testutil"
	"github.com/truvity/asg/module"
	"github.com/truvity/asg/modules/watchdog"
)

func contribution(t *testing.T, a watchdog.Args, extra ...module.Module) module.Contribution {
	t.Helper()

	c, err := module.Collect(append([]module.Module{watchdog.New(a)}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

type other struct{ c module.Contribution }

func (other) Name() string                               { return "other" }
func (o other) Contribute() (module.Contribution, error) { return o.c, nil }

func TestRendersUnitsAndStartsTheTimerOnlyInService(t *testing.T) {
	r := testutil.Render(t, contribution(t, watchdog.Args{Checks: []module.WatchCheck{{Name: "app", Script: "true"}}}))
	testutil.CheckSyntax(t, r)

	if timer := r.Files["/etc/systemd/system/asg-watchdog.timer"]; strings.Contains(timer, "[Install]") ||
		!strings.Contains(timer, "OnBootSec=120s") || !strings.Contains(timer, "OnUnitActiveSec=60s") {
		t.Errorf("timer:\n%s", timer)
	}

	if !strings.Contains(r.Files["/usr/local/lib/asg/in-service.d/00-watchdog.sh"], "systemctl start asg-watchdog.timer") {
		t.Error("the timer is not started by the in-service step")
	}

	if strings.Contains(strings.Join(r.RunCmd, "\n"), "enable asg-watchdog.timer") {
		t.Error("the timer must not be enabled: a warm-pool boot would start it")
	}

	if !strings.Contains(r.Files["/etc/asg/watchdog.conf"], "FAIL_SECONDS=300\n") {
		t.Errorf("conf:\n%s", r.Files["/etc/asg/watchdog.conf"])
	}

	if _, ok := r.Files["/usr/local/lib/asg/watch.d/00-app.sh"]; !ok {
		t.Error("the check is not written")
	}
}

func TestCollectsChecksFromOtherModules(t *testing.T) {
	c := contribution(t, watchdog.Args{}, other{module.Contribution{Watch: []module.WatchCheck{{Name: "tunnel", Script: "true"}}}})
	if len(c.Watch) != 1 || c.Watch[0].Name != "tunnel" {
		t.Errorf("watch %+v", c.Watch)
	}
}

func TestRefusals(t *testing.T) {
	for name, a := range map[string]watchdog.Args{
		"interval":     {IntervalSeconds: 5},
		"unit":         {RestartUnits: []string{"x; reboot"}, RestartAttempts: 1},
		"attempts":     {RestartAttempts: 1},
		"negative":     {RestartAttempts: -1},
		"fail minutes": {FailMinutes: -2},
	} {
		if _, err := watchdog.New(a).Contribute(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

type env struct {
	dir, state string
	vars       []string
	calls      string
}

// setup materialises the watchdog with one failing check and fake commands.
func setup(t *testing.T, a watchdog.Args) env {
	t.Helper()

	a.Checks = []module.WatchCheck{{Name: "app", Script: "[ -e \"$HEALTHY\" ]"}}
	r := testutil.Render(t, contribution(t, a))
	dir := t.TempDir()
	testutil.Materialize(t, r, dir)

	calls := filepath.Join(dir, "calls")
	bin := testutil.FakeBin(t, map[string]string{
		"curl":      testutil.FakeCurl,
		"aws":       "echo \"aws $*\" >>" + calls,
		"systemctl": "echo \"systemctl $*\" >>" + calls,
	})

	e := env{dir: dir, state: filepath.Join(dir, "state"), calls: calls}
	e.vars = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"ASG_LIB=" + filepath.Join(dir, "usr/local/lib/asg"),
		"ASG_WATCHDOG_STATE=" + e.state,
		"ASG_WATCHDOG_CONF=" + filepath.Join(dir, "etc/asg/watchdog.conf"),
		"HEALTHY=" + filepath.Join(dir, "healthy"),
	}

	return e
}

func (e env) run(t *testing.T) {
	t.Helper()

	if out, err := testutil.Run(t, e.vars, filepath.Join(e.dir, "usr/local/lib/asg/watchdog.sh")); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func (e env) seed(t *testing.T, name string, v string) {
	t.Helper()

	if err := os.MkdirAll(e.state, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(e.state, name), []byte(v+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e env) log() string { b, _ := os.ReadFile(e.calls); return string(b) }

func ago(d time.Duration) string { return strconv.FormatInt(time.Now().Add(-d).Unix(), 10) }

func TestMarksTheInstanceUnhealthyOnlyAfterTheFailureWindow(t *testing.T) {
	e := setup(t, watchdog.Args{})

	// Failing for 2 minutes of 5: nothing yet.
	e.seed(t, "unhealthy-since", ago(2*time.Minute))
	e.run(t)

	if e.log() != "" {
		t.Errorf("acted too early: %q", e.log())
	}

	// Failing for 6 minutes: the group is asked to replace the instance.
	e.seed(t, "unhealthy-since", ago(6*time.Minute))
	e.run(t)

	if want := "aws autoscaling set-instance-health --instance-id i-0abc --health-status Unhealthy --region eu-west-1"; !strings.Contains(e.log(), want) {
		t.Errorf("calls %q lack %q", e.log(), want)
	}

	// Healthy again: the clock is reset.
	if err := os.WriteFile(filepath.Join(e.dir, "healthy"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	e.run(t)

	if _, err := os.Stat(filepath.Join(e.state, "unhealthy-since")); err == nil {
		t.Error("the failure clock was not reset")
	}
}

func TestRestartsBeforeGivingUp(t *testing.T) {
	e := setup(t, watchdog.Args{RestartUnits: []string{"app.service"}, RestartAttempts: 1})

	e.seed(t, "unhealthy-since", ago(6*time.Minute))
	e.run(t)

	if e.log() != "systemctl restart app.service\n" {
		t.Errorf("first expiry: %q", e.log())
	}

	// The window starts again after a restart; the second expiry gives up.
	e.seed(t, "unhealthy-since", ago(6*time.Minute))
	e.run(t)

	if !strings.Contains(e.log(), "set-instance-health") || strings.Count(e.log(), "systemctl restart") != 1 {
		t.Errorf("second expiry: %q", e.log())
	}
}
