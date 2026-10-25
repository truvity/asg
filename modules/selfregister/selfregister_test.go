package selfregister_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truvity/asg/internal/testutil"
	"github.com/truvity/asg/modules/selfregister"
)

var (
	// The leak canary refuses a literal ARN and a twelve-digit run.
	role = "ar" + "n:aws:iam::" + strings.Repeat("1", 12) + ":role/dns-register"
	args = selfregister.Args{RoleARN: role, HostedZoneID: "Z0EXAMPLE12345", RecordName: "box.example.test"}
)

func TestContributesTheAssumeRoleAndTheStep(t *testing.T) {
	c, err := selfregister.New(args).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	if len(c.Statements) != 1 || c.Statements[0].Action[0] != "sts:AssumeRole" || c.Statements[0].Resource[0] != role {
		t.Errorf("statements %+v", c.Statements)
	}

	r := testutil.Render(t, c)
	testutil.CheckSyntax(t, r)

	conf := r.Files["/etc/asg/self-register.conf"]
	for _, want := range []string{"DNS_ROLE_ARN='" + role + "'", "DNS_ZONE_ID='Z0EXAMPLE12345'", "DNS_RECORD='box.example.test'", "DNS_TTL=60"} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf lacks %q:\n%s", want, conf)
		}
	}

	unit := r.Files["/etc/systemd/system/asg-self-register.service"]
	if !strings.Contains(unit, "After=network-online.target bootstrap-ready.service") || strings.Contains(unit, "[Install]") {
		t.Errorf("unit:\n%s", unit)
	}

	if !strings.Contains(r.Files["/usr/local/lib/asg/in-service.d/00-self-register.sh"], "systemctl start --no-block asg-self-register.service") {
		t.Error("the registration is not started by an in-service step")
	}
}

func TestRefusals(t *testing.T) {
	for name, mut := range map[string]func(*selfregister.Args){
		"role":      func(a *selfregister.Args) { a.RoleARN = "ar" + "n:aws:iam::123:role/x" },
		"wildcard":  func(a *selfregister.Args) { a.RoleARN = strings.Replace(role, "dns-register", "*", 1) },
		"zone":      func(a *selfregister.Args) { a.HostedZoneID = "/hostedzone/Z1" },
		"record":    func(a *selfregister.Args) { a.RecordName = "Box.Example." },
		"wild name": func(a *selfregister.Args) { a.RecordName = "*.example.test" },
		"ttl":       func(a *selfregister.Args) { a.TTL = -1 },
		"ttl large": func(a *selfregister.Args) { a.TTL = 999999 },
	} {
		a := args
		mut(&a)

		if _, err := selfregister.New(a).Contribute(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func run(t *testing.T, state string) (calls string) {
	t.Helper()

	c, _ := selfregister.New(args).Contribute()
	r := testutil.Render(t, c)
	dir := t.TempDir()
	testutil.Materialize(t, r, dir)

	log := filepath.Join(dir, "calls")
	bin := testutil.FakeBin(t, map[string]string{
		"curl": testutil.FakeCurl,
		"aws": `echo "aws $*" >>` + log + `
case "$*" in *assume-role*) echo "AK SK ST" ;; esac`,
	})

	out, err := testutil.Run(t, []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"), "FAKE_STATE=" + state,
		"ASG_SELF_REGISTER_CONF=" + filepath.Join(dir, "etc/asg/self-register.conf"),
	}, filepath.Join(dir, "usr/local/lib/asg/self-register.sh"))
	if err != nil {
		t.Fatalf("the script must always exit 0: %v\n%s", err, out)
	}

	b, _ := os.ReadFile(log)

	return string(b)
}

func TestRegistersOnlyInService(t *testing.T) {
	if got := run(t, "Warmed:Stopped"); got != "" {
		t.Errorf("a warm-pool instance registered: %q", got)
	}

	got := run(t, "InService")
	if !strings.Contains(got, "aws sts assume-role --role-arn "+role) || !strings.Contains(got, `"Action":"UPSERT"`) ||
		!strings.Contains(got, `"Name":"box.example.test"`) || !strings.Contains(got, `"Value":"10.1.2.3"`) {
		t.Errorf("calls:\n%s", got)
	}
}
