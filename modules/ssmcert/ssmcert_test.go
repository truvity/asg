package ssmcert_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truvity/asg/internal/testutil"
	"github.com/truvity/asg/module"
	"github.com/truvity/asg/modules/ssmcert"
)

func args() ssmcert.Args {
	return ssmcert.Args{
		Name: "server", Cert: "/estate/tls/cert", Key: "/estate/tls/key", CA: "/estate/tls/ca",
		Owner: "", RestartUnits: []string{"app.service"}, ReadyCheck: "[ -e \"$READY_FLAG\" ]\n",
		DrainPort: 1234, Group: "box", Hook: "box-launch",
	}
}

func contribution(t *testing.T, a ssmcert.Args) module.Contribution {
	t.Helper()

	c, err := module.Collect(ssmcert.New(a))
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestContribution(t *testing.T) {
	c := contribution(t, args())

	if got := strings.Join(c.SSMParameters, " "); got != "/estate/tls/cert /estate/tls/key /estate/tls/ca" {
		t.Errorf("parameters %q", got)
	}

	for _, want := range []string{
		"autoscaling:EnterStandby", "autoscaling:ExitStandby", "autoscaling:SetDesiredCapacity",
		"autoscaling:TerminateInstanceInAutoScalingGroup",
	} {
		if !strings.Contains(strings.Join(c.GroupActions, " "), want) {
			t.Errorf("group actions %v lack %s", c.GroupActions, want)
		}
	}

	if len(c.Statements) != 1 || c.Statements[0].Action[0] != "autoscaling:DescribeAutoScalingGroups" {
		t.Errorf("statements %+v", c.Statements)
	}

	if len(c.InService) != 2 || c.InService[0].Order != ssmcert.LoadOrder || c.InService[1].Order != ssmcert.TimerOrder {
		t.Errorf("in-service steps %+v", c.InService)
	}

	r := testutil.Render(t, c)
	testutil.CheckSyntax(t, r)

	timer := r.Files["/etc/systemd/system/asg-ssmcert-server.timer"]
	if strings.Contains(timer, "[Install]") || !strings.Contains(timer, "OnUnitActiveSec=60min") {
		t.Errorf("timer:\n%s", timer)
	}

	conf := r.Files["/etc/asg/ssmcert-server.conf"]
	for _, want := range []string{
		"CERT_PARAM='/estate/tls/cert'", "DIR='/run/asg-ssmcert/server'", "STANDBY=1", "HOOK='box-launch'",
		"WINDOW_START=2", "WINDOW_END=5", "DRAIN_SECONDS=1800",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf lacks %s:\n%s", want, conf)
		}
	}

	cert, key, ca := ssmcert.Args{Name: "server"}.Files()
	if cert != "/run/asg-ssmcert/server/cert.pem" || key != "/run/asg-ssmcert/server/key.pem" || ca != "/run/asg-ssmcert/server/ca.pem" {
		t.Errorf("files %s %s %s", cert, key, ca)
	}
}

func TestInPlaceNeedsNoGroup(t *testing.T) {
	a := args()
	a.InPlace, a.Group, a.Hook = true, "", ""

	c := contribution(t, a)
	if len(c.GroupActions) != 0 || len(c.Statements) != 0 {
		t.Errorf("in place asks for group rights: %v %v", c.GroupActions, c.Statements)
	}
}

func TestRefusals(t *testing.T) {
	zero := 0

	for name, mut := range map[string]func(*ssmcert.Args){
		"cert":     func(a *ssmcert.Args) { a.Cert = "relative" },
		"same":     func(a *ssmcert.Args) { a.Key = a.Cert },
		"dir":      func(a *ssmcert.Args) { a.Dir = "/var/lib/tls" },
		"owner":    func(a *ssmcert.Args) { a.Owner = "Root;x" },
		"units":    func(a *ssmcert.Args) { a.RestartUnits = nil },
		"unit":     func(a *ssmcert.Args) { a.RestartUnits = []string{"app; reboot"} },
		"ready":    func(a *ssmcert.Args) { a.ReadyCheck = " " },
		"window":   func(a *ssmcert.Args) { a.WindowStartHour, a.WindowEndHour = &zero, &zero },
		"port":     func(a *ssmcert.Args) { a.DrainPort = 0 },
		"group":    func(a *ssmcert.Args) { a.Group = "" },
		"hook":     func(a *ssmcert.Args) { a.Hook = "" },
		"interval": func(a *ssmcert.Args) { a.CheckIntervalMinutes = 1 },
	} {
		a := args()
		mut(&a)

		if _, err := ssmcert.New(a).Contribute(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// fakeAWS answers the SSM and Auto Scaling calls from files under $FAKE:
// versions/<leaf> is a parameter's version; group/<n>.json are the answers
// to successive describe-auto-scaling-groups calls (the last one repeats).
// Every call is logged to $FAKE/calls without its values.
const fakeAWS = `name=""
prev=""
for a in "$@"; do
  [ "$prev" = --name ] && name="$a"
  prev="$a"
done
echo "aws $1 $2" >>"$FAKE/calls"
case "$1 $2" in
  "ssm get-parameter")
    v="$(cat "$FAKE/versions/$(basename "$name")")"
    case "$*" in
      *--with-decryption*) printf '{"Parameter":{"Name":"%s","Value":"SECRET-%s-v%s","Version":%s}}' "$name" "$(basename "$name")" "$v" "$v" ;;
      *) echo "$v" ;;
    esac
    ;;
  "autoscaling describe-auto-scaling-groups")
    n="$(cat "$FAKE/n" 2>/dev/null || echo 0)"
    f="$FAKE/group/$n.json"
    [ -f "$f" ] && echo $((n + 1)) >"$FAKE/n" || f="$(ls "$FAKE"/group/*.json | sort -V | tail -n 1)"
    cat "$f"
    ;;
  *) echo "aws $*" >>"$FAKE/actions" ;;
esac
`

type env struct {
	dir, fake, out string
	vars           []string
}

func setup(t *testing.T, a ssmcert.Args) env {
	t.Helper()

	r := testutil.Render(t, contribution(t, a))
	dir := t.TempDir()
	testutil.Materialize(t, r, dir)

	out := filepath.Join(dir, "run", "tls")
	conf := filepath.Join(dir, "etc/asg/ssmcert-server.conf")
	b, _ := os.ReadFile(conf)
	if err := os.WriteFile(conf, []byte(strings.ReplaceAll(string(b), "/run/asg-ssmcert/server", out)), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := filepath.Join(dir, "fake")
	for _, d := range []string{"versions", "group"} {
		if err := os.MkdirAll(filepath.Join(fake, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	bin := testutil.FakeBin(t, map[string]string{
		"curl":      testutil.FakeCurl,
		"aws":       fakeAWS,
		"systemctl": "echo \"systemctl $*\" >>\"$FAKE/actions\"",
		"ss":        "cat \"$FAKE/connections\" 2>/dev/null || true",
		"sleep":     "true",
	})

	e := env{dir: dir, fake: fake, out: out}
	e.vars = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"FAKE=" + fake,
		"ASG_SSMCERT_CONF=" + conf,
		"ASG_SSMCERT_READY=" + filepath.Join(dir, "etc/asg/ssmcert-server-ready.sh"),
		"ASG_WATCHDOG_STATE=" + filepath.Join(dir, "watchdog"),
		"ASG_SSMCERT_POLL=0",
		"READY_FLAG=" + filepath.Join(dir, "ready"),
	}

	e.write(t, "versions/cert", "1")
	e.write(t, "versions/key", "1")
	e.write(t, "versions/ca", "1")

	return e
}

func (e env) write(t *testing.T, rel, v string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(e.fake, rel), []byte(v+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e env) run(t *testing.T, hour string, cmd string) (string, error) {
	t.Helper()

	return testutil.Run(t, append(e.vars, "ASG_SSMCERT_HOUR="+hour), filepath.Join(e.dir, "usr/local/lib/asg/ssmcert-server.sh"), cmd)
}

func (e env) actions() string {
	b, _ := os.ReadFile(filepath.Join(e.fake, "actions"))
	return string(b)
}

func group(states ...string) string {
	var in []string
	for i, s := range states {
		id := "i-other" + string(rune('a'+i))
		if i == 0 {
			id = "i-0abc"
		}

		in = append(in, `{"InstanceId":"`+id+`","LifecycleState":"`+s+`"}`)
	}

	return `{"AutoScalingGroups":[{"DesiredCapacity":DESIRED,"MaxSize":3,"Instances":[` + strings.Join(in, ",") + `]}]}`
}

func (e env) groups(t *testing.T, desired []string, answers ...string) {
	t.Helper()

	for i, a := range answers {
		e.write(t, "group/"+string(rune('0'+i))+".json", strings.ReplaceAll(a, "DESIRED", desired[i]))
	}
}

func TestLoadWritesTheFilesAndNeverPrintsAValue(t *testing.T) {
	e := setup(t, args())

	out, err := e.run(t, "12", "load")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	if strings.Contains(out, "SECRET") {
		t.Errorf("a value reached the log:\n%s", out)
	}

	for file, want := range map[string]string{"cert.pem": "SECRET-cert-v1", "key.pem": "SECRET-key-v1", "ca.pem": "SECRET-ca-v1"} {
		b, err := os.ReadFile(filepath.Join(e.out, file))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q %v", file, b, err)
		}
	}

	if st, _ := os.Stat(filepath.Join(e.out, "key.pem")); st == nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", st)
	}

	v, _ := os.ReadFile(filepath.Join(e.out, "versions"))
	if string(v) != "/estate/tls/cert 1\n/estate/tls/key 1\n/estate/tls/ca 1\n" {
		t.Errorf("versions %q", v)
	}
}

func TestCheckDoesNothingWithoutANewVersionOrOutsideTheWindow(t *testing.T) {
	e := setup(t, args())
	if out, err := e.run(t, "12", "load"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	if out, err := e.run(t, "3", "check"); err != nil || e.actions() != "" {
		t.Fatalf("no change: %v %q\n%s", err, e.actions(), out)
	}

	e.write(t, "versions/cert", "2")

	out, err := e.run(t, "12", "check")
	if err != nil || e.actions() != "" || !strings.Contains(out, "waits for the window") {
		t.Fatalf("outside the window: %v %q\n%s", err, e.actions(), out)
	}
}

func TestStandbyRotation(t *testing.T) {
	e := setup(t, args())
	if out, err := e.run(t, "12", "load"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	e.write(t, "versions/key", "2")
	_ = os.WriteFile(filepath.Join(e.dir, "ready"), nil, 0o644)

	e.groups(t, []string{"1", "1", "1", "2", "2", "2"},
		group("InService"),            // before
		group("Standby", "Pending"),   // waiting for the replacement
		group("Standby", "InService"), // replacement in service
		group("Pending:Wait", "InService"),
		group("InService", "InService"),
		group("InService", "InService"),
	)

	out, err := e.run(t, "3", "check")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	acts := e.actions()
	order := []string{
		"enter-standby --region eu-west-1 --auto-scaling-group-name box --instance-ids i-0abc --no-should-decrement-desired-capacity",
		"systemctl restart app.service",
		"exit-standby --region eu-west-1 --auto-scaling-group-name box --instance-ids i-0abc",
		"complete-lifecycle-action --region eu-west-1 --auto-scaling-group-name box --lifecycle-hook-name box-launch " +
			"--instance-id i-0abc --lifecycle-action-result CONTINUE",
		"set-desired-capacity --region eu-west-1 --auto-scaling-group-name box --desired-capacity 1",
	}

	at := 0
	for _, want := range order {
		i := strings.Index(acts[at:], want)
		if i < 0 {
			t.Fatalf("actions lack, in order, %q:\n%s\n%s", want, acts, out)
		}

		at += i + len(want)
	}

	if b, _ := os.ReadFile(filepath.Join(e.out, "key.pem")); string(b) != "SECRET-key-v2" {
		t.Errorf("the new key was not loaded: %q", b)
	}

	if _, err := os.Stat(filepath.Join(e.dir, "watchdog", "paused-until")); err == nil {
		t.Error("the watchdog is still paused")
	}
}

func TestStandbyFailureTerminatesOnlyThisInstance(t *testing.T) {
	a := args()
	a.ReadyTimeoutSeconds = 10
	e := setup(t, a)

	if out, err := e.run(t, "12", "load"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	e.write(t, "versions/cert", "2")
	e.groups(t, []string{"1", "1"}, group("InService"), group("Standby", "InService"))

	// No ready flag: the restarted daemon never answers. bash's SECONDS
	// moves on its own, so the 10 s bound passes in real time.
	out, err := e.run(t, "3", "check")
	if err == nil {
		t.Fatalf("a failed restart exited 0\n%s", out)
	}

	acts := e.actions()
	if !strings.Contains(acts, "terminate-instance-in-auto-scaling-group --region eu-west-1 --instance-id i-0abc --no-should-decrement-desired-capacity") ||
		strings.Contains(acts, "exit-standby") || strings.Contains(acts, "set-desired-capacity") {
		t.Errorf("actions:\n%s", acts)
	}
}

func TestBusyGroupIsLeftAlone(t *testing.T) {
	e := setup(t, args())
	if out, err := e.run(t, "12", "load"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	e.write(t, "versions/ca", "5")
	// Another instance is still launching.
	e.groups(t, []string{"2"}, group("InService", "Pending"))

	out, err := e.run(t, "3", "check")
	if err != nil || e.actions() != "" || !strings.Contains(out, "group busy") {
		t.Fatalf("%v %q\n%s", err, e.actions(), out)
	}
}

func TestInPlaceRestartsWhenIdle(t *testing.T) {
	a := args()
	a.InPlace, a.Group, a.Hook = true, "", ""
	e := setup(t, a)

	if out, err := e.run(t, "12", "load"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	e.write(t, "versions/cert", "2")
	_ = os.WriteFile(filepath.Join(e.dir, "ready"), nil, 0o644)

	// POLL is 0, so the idle counter never grows: use a 1-second poll.
	vars := append([]string{}, e.vars...)
	vars = append(vars, "ASG_SSMCERT_POLL=60", "ASG_SSMCERT_HOUR=3")

	out, err := testutil.Run(t, vars, filepath.Join(e.dir, "usr/local/lib/asg/ssmcert-server.sh"), "check")
	if err != nil || e.actions() != "systemctl restart app.service\n" {
		t.Fatalf("%v %q\n%s", err, e.actions(), out)
	}
}
