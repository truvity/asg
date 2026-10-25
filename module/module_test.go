package module_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/internal/testutil"
	"github.com/truvity/asg/module"
)

type fake struct {
	name string
	c    module.Contribution
	err  error
}

func (f fake) Name() string                             { return f.name }
func (f fake) Contribute() (module.Contribution, error) { return f.c, f.err }

func TestCollectMergesInOrderAndNamesTheFailingModule(t *testing.T) {
	c, err := module.Collect(
		fake{name: "a", c: module.Contribution{SSMParameters: []string{"/a"}, InService: []module.Step{{Name: "one", Script: "true"}}}},
		fake{name: "b", c: module.Contribution{SSMParameters: []string{"/b"}, InService: []module.Step{{Name: "two", Script: "true"}}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(c.SSMParameters, ",") != "/a,/b" || c.InService[0].Name != "one" || c.InService[1].Name != "two" {
		t.Errorf("merged %+v", c)
	}

	_, err = module.Collect(fake{name: "bad", err: os.ErrInvalid}, nil)
	if err == nil || !strings.Contains(err.Error(), `module "bad"`) || !strings.Contains(err.Error(), "nil module") {
		t.Errorf("error %v", err)
	}
}

func plan(t *testing.T, lifecycle bool) *bootstrap.Plan {
	t.Helper()

	o := bootstrap.Options{}
	if lifecycle {
		o.Lifecycle = testutil.Lifecycle
	}

	p, err := bootstrap.New(o)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func TestAddToRefusals(t *testing.T) {
	step := []module.Step{{Name: "start", Script: "true"}}

	for name, tc := range map[string]struct {
		c         module.Contribution
		lifecycle bool
		want      string
	}{
		"no lifecycle":   {module.Contribution{InService: step}, false, "Options.Lifecycle"},
		"drain":          {module.Contribution{Drains: []module.Drain{{Name: "d", Script: "true"}}}, true, "terminate"},
		"bad name":       {module.Contribution{InService: []module.Step{{Name: "Bad Name", Script: "true"}}}, true, "lower-case"},
		"duplicate name": {module.Contribution{InService: append(append([]module.Step{}, step...), step...)}, true, "twice"},
		"empty script":   {module.Contribution{ReadyChecks: []module.ReadyCheck{{Name: "r"}}}, true, "ready check"},
		"empty watch":    {module.Contribution{Watch: []module.WatchCheck{{Name: "w"}}}, true, "watch check"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.c.AddTo(plan(t, tc.lifecycle))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEmptyContributionAddsNothing(t *testing.T) {
	p := plan(t, false)
	if err := (module.Contribution{}).AddTo(p); err != nil {
		t.Fatal(err)
	}

	r, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}

	if len(r.Objects) != 0 || strings.Contains(r.CloudConfig, "asg") {
		t.Errorf("an empty contribution left a trace:\n%s", r.CloudConfig)
	}
}

func rendered(t *testing.T) (testutil.Rendered, string) {
	t.Helper()

	c := module.Contribution{
		InService: []module.Step{
			{Name: "late", Order: 200, Script: "echo late >>\"$LOG\""},
			{Name: "early", Order: 10, Script: "echo early >>\"$LOG\""},
			{Name: "tie", Order: 10, Script: "echo tie >>\"$LOG\""},
		},
		ReadyChecks: []module.ReadyCheck{{Name: "up", Script: "[ -e \"$UP\" ]", TimeoutSeconds: 1}},
	}
	r := testutil.Render(t, c)
	testutil.CheckSyntax(t, r)

	dir := t.TempDir()
	testutil.Materialize(t, r, dir)

	return r, filepath.Join(dir, "usr/local/lib/asg")
}

func TestRunnerStartsServicesOnlyWhenGoingInService(t *testing.T) {
	_, lib := rendered(t)
	bin := testutil.FakeBin(t, map[string]string{"curl": testutil.FakeCurl})
	work := t.TempDir()
	log := filepath.Join(work, "log")
	up := filepath.Join(work, "up")

	env := func(state string) []string {
		return []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "ASG_LIB=" + lib, "FAKE_STATE=" + state, "LOG=" + log, "UP=" + up}
	}

	read := func() string { b, _ := os.ReadFile(log); return string(b) }

	if out, err := testutil.Run(t, env("Warmed:Stopped"), filepath.Join(lib, "lifecycle.sh")); err != nil || read() != "" {
		t.Errorf("warm pool: err %v, log %q\n%s", err, read(), out)
	}

	if out, err := testutil.Run(t, env("Terminating"), filepath.Join(lib, "lifecycle.sh")); err != nil || read() != "" {
		t.Errorf("other state: err %v, log %q\n%s", err, read(), out)
	}

	// In service, ready check failing: the steps ran in order, then it fails.
	if out, err := testutil.Run(t, env("InService"), filepath.Join(lib, "lifecycle.sh")); err == nil {
		t.Errorf("a failing ready check must fail the unit\n%s", out)
	}

	if got := read(); got != "early\ntie\nlate\n" {
		t.Errorf("steps ran as %q, want early, tie (registration order), late", got)
	}

	// The check passes: success.
	if err := os.WriteFile(up, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := testutil.Run(t, env("-"), filepath.Join(lib, "lifecycle.sh")); err != nil {
		t.Errorf("no group (cold start) with a passing check: %v\n%s", err, out)
	}
}

func TestAFailingStepFailsTheUnit(t *testing.T) {
	c := module.Contribution{InService: []module.Step{{Name: "boom", Script: "false\necho not-reached >>\"$LOG\""}}}
	r := testutil.Render(t, c)
	dir := t.TempDir()
	testutil.Materialize(t, r, dir)

	bin := testutil.FakeBin(t, map[string]string{"curl": testutil.FakeCurl})
	log := filepath.Join(dir, "log")

	lib := filepath.Join(dir, "usr/local/lib/asg")
	env := []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "ASG_LIB=" + lib, "FAKE_STATE=InService", "LOG=" + log}

	_, err := testutil.Run(t, env, filepath.Join(lib, "lifecycle.sh"))
	if err == nil {
		t.Error("a failing step must fail the unit")
	}

	if _, statErr := os.Stat(log); statErr == nil {
		t.Error("the step kept running after a failure")
	}
}

func TestRendersTheRunnerUnitAndReadyOrdering(t *testing.T) {
	r, _ := rendered(t)

	unit := r.Files["/etc/systemd/system/asg-in-service.service"]
	for _, want := range []string{"Type=oneshot", "RemainAfterExit=yes", "ExecStart=/usr/local/lib/asg/lifecycle.sh", "WantedBy=multi-user.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}

	// The ready service must wait for the in-service unit and check it is active.
	ready := r.Files["/etc/systemd/system/bootstrap-ready.service"]
	if !strings.Contains(ready, "asg-in-service.service") {
		t.Errorf("bootstrap-ready.service does not follow asg-in-service:\n%s", ready)
	}

	if !strings.Contains(r.Files["/usr/local/lib/bootstrap/ready.sh"], "asg-in-service.service") {
		t.Error("ready.sh does not check asg-in-service")
	}

	for _, p := range []string{"in-service.d/00-early.sh", "in-service.d/01-tie.sh", "in-service.d/02-late.sh", "ready.d/00-up.sh"} {
		if _, ok := r.Files["/usr/local/lib/asg/"+p]; !ok {
			t.Errorf("missing %s; have %v", p, keys(r.Files))
		}
	}

	if !strings.Contains(r.Files["/usr/local/lib/asg/ready.d/00-up.sh"], "# asg-timeout=1\n") {
		t.Error("the ready check lost its timeout")
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}

	return out
}
