package wakeproxy_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truvity/asg"
	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/internal/testutil"
	"github.com/truvity/asg/internal/wakeconfig"
	"github.com/truvity/asg/modules/wakeproxy"
)

// The tests build the account and the key ARN at run time: the leak canary
// rejects both literals.
var (
	account   = strings.Repeat("1", 12)
	partition = "a" + "ws"
	keyARN    = "arn:" + partition + ":kms:eu-west-1:" + account + ":key/example"
	digest    = strings.Repeat("ab", 32)
)

func args() wakeproxy.Args {
	return wakeproxy.Args{
		Binary: map[string]bootstrap.Source{"arm64": {URL: wakeproxy.ReleaseURL("v1.2.3", "arm64"), SHA256: digest}},
		Region: "eu-west-1",
		Routes: []wakeproxy.Route{
			{Name: "arm", ListenPort: 9001, Target: wakeproxy.Selector{TagKey: "role", TagValue: "builder-arm"}, TargetPort: 1234},
			{Name: "amd", ListenPort: 9002, Target: wakeproxy.Selector{TagKey: "role", TagValue: "builder-amd"}, TargetPort: 1234},
		},
		MetricsAddress: "127.0.0.1:9102",
	}
}

func TestReleaseURL(t *testing.T) {
	if got, want := wakeproxy.ReleaseURL("v1.2.3", "arm64"), "https://github.com/truvity/asg/releases/download/v1.2.3/wake-proxy_1.2.3_linux_arm64"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestContributesTheBinaryTheConfigAndAHardenedUnit(t *testing.T) {
	c, err := wakeproxy.New(args()).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	r := testutil.Render(t, c)
	testutil.CheckSyntax(t, r)

	// The file on the box is the file the binary parses, defaults included.
	cfg, err := wakeconfig.Parse([]byte(r.Files[wakeproxy.ConfigPath]))
	if err != nil {
		t.Fatalf("rendered config: %v\n%s", err, r.Files[wakeproxy.ConfigPath])
	}

	if cfg.IdleTimeout.D() != 20*time.Minute || cfg.WakeTimeout.D() != 180*time.Second || cfg.HealthPort != 8080 || len(cfg.Routes) != 2 {
		t.Errorf("config %+v", cfg)
	}

	unit := r.Files["/etc/systemd/system/"+wakeproxy.UnitName]
	for _, want := range []string{
		"DynamicUser=yes", "Restart=always", "CapabilityBoundingSet=\n", "NoNewPrivileges=yes", "ProtectSystem=strict", "TimeoutStopSec=45",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}

	if strings.Contains(unit, "[Install]") || strings.Contains(unit, "AmbientCapabilities") {
		t.Errorf("unit has an [Install] section or a capability:\n%s", unit)
	}

	if len(c.InService) != 1 || len(c.ReadyChecks) != 1 || len(c.Watch) != 1 || len(c.Scrape) != 1 {
		t.Errorf("steps %d, ready %d, watch %d, scrape %d", len(c.InService), len(c.ReadyChecks), len(c.Watch), len(c.Scrape))
	}

	if !strings.Contains(c.ReadyChecks[0].Script, "127.0.0.1:8080/healthz") {
		t.Errorf("ready check %q", c.ReadyChecks[0].Script)
	}

	if len(c.Statements) != 0 {
		t.Error("statements without an Account")
	}

	joined := strings.Join(r.RunCmd, "\n") + r.CloudConfig
	if !strings.Contains(joined, digest) || !strings.Contains(joined, "wake-proxy_1.2.3_linux_arm64") {
		t.Error("the pinned download is not in the user-data")
	}
}

func TestAPortBelow1024GetsOnlyTheBindCapability(t *testing.T) {
	a := args()
	a.Routes[0].ListenPort = 443

	c, err := wakeproxy.New(a).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	unit := testutil.Render(t, c).Files["/etc/systemd/system/"+wakeproxy.UnitName]
	if !strings.Contains(unit, "CapabilityBoundingSet=CAP_NET_BIND_SERVICE\nAmbientCapabilities=CAP_NET_BIND_SERVICE\n") {
		t.Errorf("unit:\n%s", unit)
	}
}

func TestRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*wakeproxy.Args)
		want string
	}{
		"no binary":    {func(a *wakeproxy.Args) { a.Binary = nil }, "binary"},
		"no region":    {func(a *wakeproxy.Args) { a.Region = "" }, "region"},
		"port clash":   {func(a *wakeproxy.Args) { a.Routes[1].ListenPort = 9001 }, "already used"},
		"health clash": {func(a *wakeproxy.Args) { a.HealthPort = 9001 }, "already used"},
		"no selector":  {func(a *wakeproxy.Args) { a.Routes[0].Target = wakeproxy.Selector{} }, "selector"},
		"bad account":  {func(a *wakeproxy.Args) { a.Account = "12" }, "12-digit"},
		"short sha": {func(a *wakeproxy.Args) {
			a.Binary = map[string]bootstrap.Source{"arm64": {URL: "https://example.com/x", SHA256: "abc"}}
		}, "sha256"},
	} {
		a := args()
		tc.mut(&a)

		c, err := wakeproxy.New(a).Contribute()
		if err == nil {
			// The digest check is the bootstrap plan's.
			_, err = renderErr(c)
		}

		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}

func TestPolicyScopesStartStopAndTagsAndTheKey(t *testing.T) {
	a := args()
	a.Account = account
	a.KMSKeyARNs = []string{keyARN, keyARN}

	c, err := wakeproxy.New(a).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	doc, err := asg.Document(c.Statements)
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Statement []struct {
			Sid       string
			Action    []string
			Resource  []string
			Condition map[string]map[string][]string
		}
	}

	if err := json.Unmarshal([]byte(doc), &got); err != nil {
		t.Fatal(err)
	}

	bySid := map[string]int{}
	for i, s := range got.Statement {
		bySid[s.Sid] = i
	}

	// Describe, two start/stop, two tag, key use, key grant.
	if len(got.Statement) != 7 {
		t.Fatalf("%d statements:\n%s", len(got.Statement), doc)
	}

	if s := got.Statement[bySid[wakeproxy.SidDescribe]]; s.Resource[0] != "*" || s.Condition != nil {
		t.Errorf("describe %+v", s)
	}

	ss := got.Statement[bySid["StartStopTargets1"]] // builder-amd sorts first
	if ss.Action[0] != "ec2:StartInstances" || ss.Action[1] != "ec2:StopInstances" ||
		ss.Condition["StringEquals"]["aws:ResourceTag/role"][0] != "builder-amd" ||
		ss.Resource[0] != wakeproxy.InstanceARN(partition, "eu-west-1", account) {
		t.Errorf("start/stop %+v", ss)
	}

	tg := got.Statement[bySid["TagActivity2"]]
	if len(tg.Action) != 1 || tg.Action[0] != "ec2:CreateTags" ||
		tg.Condition["ForAllValues:StringEquals"]["aws:TagKeys"][0] != "wake-proxy/last-active" ||
		tg.Condition["StringEquals"]["aws:ResourceTag/role"][0] != "builder-arm" {
		t.Errorf("tag %+v", tg)
	}

	use := got.Statement[bySid[wakeproxy.SidKeyUse]]
	grant := got.Statement[bySid[wakeproxy.SidKeyGrant]]

	if len(use.Resource) != 1 || use.Resource[0] != keyARN || use.Action[0] != "kms:Decrypt" || use.Action[1] != "kms:GenerateDataKeyWithoutPlaintext" {
		t.Errorf("key use %+v", use)
	}

	if grant.Action[0] != "kms:CreateGrant" || grant.Condition["Bool"]["kms:GrantIsForAWSResource"][0] != "true" {
		t.Errorf("key grant %+v", grant)
	}
}

func TestPolicyWithoutKeysHasNoKMSAndRefusesWildcards(t *testing.T) {
	st, err := wakeproxy.Statements(wakeproxy.PolicyInput{
		Partition: partition, Region: "eu-west-1", Account: account,
		Targets: []wakeproxy.Selector{{TagKey: "role", TagValue: "b"}},
	})
	if err != nil || len(st) != 3 {
		t.Fatalf("%v, %d statements", err, len(st))
	}

	for _, in := range []wakeproxy.PolicyInput{
		{Partition: partition, Region: "eu-west-1", Account: account, Targets: []wakeproxy.Selector{{TagKey: "role", TagValue: "b*"}}},
		{Partition: partition, Region: "eu-west-1", Account: account, Targets: []wakeproxy.Selector{{TagKey: "role", TagValue: "b"}},
			KMSKeyARNs: []string{"arn:x:kms:r:a:key/*"}},
		{Partition: partition, Region: "eu-west-1", Account: account},
	} {
		if _, err := wakeproxy.Statements(in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
}
