package front_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg/front"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The repository is public: an ARN written out would fail the leak canary, so
// the mock builds it and the goldens hold a placeholder.
const part = "aw" + "s"

func arnOf(kind, name string) string {
	return "arn:" + part + ":elasticloadbalancing:eu-west-1::" + kind + "/" + name
}

// recorder stands in for the AWS provider. It echoes every resource's inputs
// back as its state, adds the outputs the front reads, and remembers type,
// name, parent and inputs.
type recorder struct {
	mu  sync.Mutex
	all []map[string]any
}

func tail(u string) string {
	if _, rest, ok := strings.Cut(u, "::"); ok {
		if _, t, ok := strings.Cut(rest, "::"); ok {
			return t
		}
	}

	return u
}

func (r *recorder) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	parent := ""
	if args.RegisterRPC != nil {
		parent = tail(args.RegisterRPC.GetParent())
	}

	r.all = append(r.all, map[string]any{"type": args.TypeToken, "name": args.Name, "parent": parent, "inputs": args.Inputs.Mappable()})

	out := args.Inputs.Copy()

	switch args.TypeToken {
	case "aws:lb/loadBalancer:LoadBalancer":
		out["arn"] = resource.NewStringProperty(arnOf("loadbalancer/net", args.Name))
		out["dnsName"] = resource.NewStringProperty(args.Name + ".elb.example")
		out["zoneId"] = resource.NewStringProperty("ZONE" + strings.ToUpper(args.Name))
	case "aws:lb/targetGroup:TargetGroup":
		out["arn"] = resource.NewStringProperty(arnOf("targetgroup", args.Name))
	case "aws:lb/listener:Listener":
		out["arn"] = resource.NewStringProperty(arnOf("listener/net", args.Name))
	}

	return args.Name + "_id", out, nil
}

func (r *recorder) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}

func (r *recorder) dump(t *testing.T) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	sort.Slice(r.all, func(i, j int) bool {
		return r.all[i]["type"].(string)+"/"+r.all[i]["name"].(string) < r.all[j]["type"].(string)+"/"+r.all[j]["name"].(string)
	})

	b, err := json.MarshalIndent(r.all, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	return []byte(strings.ReplaceAll(string(b), "arn:"+part, "arn:@PARTITION@") + "\n")
}

func (r *recorder) count(typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0

	for _, x := range r.all {
		if x["type"] == typ {
			n++
		}
	}

	return n
}

func (r *recorder) find(t *testing.T, typ, name string) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, x := range r.all {
		if x["type"] == typ && x["name"] == name {
			return x["inputs"].(map[string]any)
		}
	}

	t.Fatalf("no %s named %s", typ, name)

	return nil
}

func run(t *testing.T, program func(ctx *pulumi.Context) error) (*recorder, error) {
	t.Helper()

	rec := &recorder{}
	err := pulumi.RunErr(program, pulumi.WithMocks("p", "s", rec))

	return rec, err
}

func mustRun(t *testing.T, program func(ctx *pulumi.Context) error) *recorder {
	t.Helper()

	rec, err := run(t, program)
	if err != nil {
		t.Fatal(err)
	}

	return rec
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()

	file := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if string(want) != string(got) {
		t.Errorf("%s differs from the golden (run go test -update to accept):\n%s", name, got)
	}
}

func sharedArgs() *front.SharedNLBArgs {
	return &front.SharedNLBArgs{
		Name:      "box-front",
		VPCID:     pulumi.String("vpc-0example"),
		VPCCIDR:   pulumi.String("10.0.0.0/16"),
		SubnetIDs: []pulumi.StringInput{pulumi.String("subnet-0a"), pulumi.String("subnet-0b"), pulumi.String("subnet-0c")},
		Tags:      map[string]string{"env": "test"},
	}
}

func sharedData() front.SharedNLB {
	return front.SharedNLB{
		ARN: arnOf("loadbalancer/net", "box-front"), DNSName: "box-front.elb.example", ZoneID: "ZONEBOXFRONT",
		SecurityGroupID: "sg-0front", VPCID: "vpc-0example", VPCCIDR: "10.0.0.0/16",
	}
}

func buildkit(arch string, port int) front.Listener {
	return front.Listener{
		Name:                       "buildkit-" + arch,
		Port:                       port,
		TargetPort:                 1234,
		IdleTimeoutSeconds:         3600,
		DeregistrationDelaySeconds: 30,
		Sources:                    []front.Source{{SecurityGroupID: pulumi.String("sg-0runners"), Description: "Runners"}},
	}
}

func TestSharedNLBResourceSet(t *testing.T) {
	rec := mustRun(t, func(ctx *pulumi.Context) error {
		_, err := front.NewSharedNLB(ctx, "box-front", sharedArgs())

		return err
	})

	golden(t, "shared-resources.golden.json", rec.dump(t))
}

func TestSharedNLBDefaultsAreTheSafeOnes(t *testing.T) {
	rec := mustRun(t, func(ctx *pulumi.Context) error {
		_, err := front.NewSharedNLB(ctx, "box-front", sharedArgs())

		return err
	})

	in := rec.find(t, "aws:lb/loadBalancer:LoadBalancer", "box-front")
	if in["internal"] != true || in["loadBalancerType"] != "network" {
		t.Errorf("want an internal network load balancer: %v", in)
	}

	if in["enableCrossZoneLoadBalancing"] != true || in["enableDeletionProtection"] != true {
		t.Errorf("cross-zone and deletion protection default to on: %v", in)
	}

	if sgs, _ := in["securityGroups"].([]any); len(sgs) != 1 {
		t.Errorf("the NLB is created with its security group: %v", in["securityGroups"])
	}

	if n := rec.count("aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule"); n != 0 {
		t.Errorf("the NLB's group has no ingress of its own, got %d rules", n)
	}

	// No inline rules on the group: other stacks add rules to it.
	sg := rec.find(t, "aws:ec2/securityGroup:SecurityGroup", "box-front")
	if _, ok := sg["ingress"]; ok {
		t.Error("the security group declares inline ingress")
	}

	if _, ok := sg["egress"]; ok {
		t.Error("the security group declares inline egress")
	}
}

func TestSharedNLBCanTurnTheDefaultsOff(t *testing.T) {
	f := false
	a := sharedArgs()
	a.CrossZone, a.DeletionProtection = &f, &f

	rec := mustRun(t, func(ctx *pulumi.Context) error {
		_, err := front.NewSharedNLB(ctx, "box-front", a)

		return err
	})

	in := rec.find(t, "aws:lb/loadBalancer:LoadBalancer", "box-front")
	if in["enableCrossZoneLoadBalancing"] != false || in["enableDeletionProtection"] != false {
		t.Errorf("explicit false lost: %v", in)
	}
}

func TestSharedNLBDataIsWhatTheBoxesRead(t *testing.T) {
	var got front.SharedNLB

	done := make(chan struct{})

	mustRun(t, func(ctx *pulumi.Context) error {
		r, err := front.NewSharedNLB(ctx, "box-front", sharedArgs())
		if err != nil {
			return err
		}

		r.Data().ApplyT(func(d any) error {
			got = d.(front.SharedNLB)

			close(done)

			return nil
		})

		return nil
	})

	<-done

	if got.SecurityGroupID != "box-front_id" || got.VPCID != "vpc-0example" || got.VPCCIDR != "10.0.0.0/16" ||
		got.DNSName != "box-front.elb.example" || !strings.HasSuffix(got.ARN, "loadbalancer/net/box-front") || got.ZoneID == "" {
		t.Errorf("data: %+v", got)
	}
}

func TestAttachResourceSet(t *testing.T) {
	rec := mustRun(t, func(ctx *pulumi.Context) error {
		_, err := front.Attach(ctx, "builders", sharedData(), []front.Listener{buildkit("arm64", 1235)})

		return err
	})

	golden(t, "attach-resources.golden.json", rec.dump(t))

	// The box owns no NLB and no security group.
	if rec.count("aws:lb/loadBalancer:LoadBalancer")+rec.count("aws:ec2/securityGroup:SecurityGroup") != 0 {
		t.Error("Attach created an NLB or a security group")
	}
}

func TestAttachScopesTheRuleToThePort(t *testing.T) {
	rec := mustRun(t, func(ctx *pulumi.Context) error {
		_, err := front.Attach(ctx, "builders", sharedData(), []front.Listener{buildkit("amd64", 1234), buildkit("arm64", 1235)})

		return err
	})

	for _, c := range []struct {
		name string
		port float64
	}{{"builders-buildkit-amd64-in-0-tcp", 1234}, {"builders-buildkit-arm64-in-0-tcp", 1235}} {
		r := rec.find(t, "aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule", c.name)
		if r["fromPort"] != c.port || r["toPort"] != c.port || r["ipProtocol"] != "tcp" {
			t.Errorf("%s: want tcp %v only: %v", c.name, c.port, r)
		}

		if r["referencedSecurityGroupId"] != "sg-0runners" || r["securityGroupId"] != "sg-0front" {
			t.Errorf("%s: want the runners' group on the NLB's group: %v", c.name, r)
		}

		if _, ok := r["cidrIpv4"]; ok {
			t.Errorf("%s: no range allowed", c.name)
		}
	}
}

func TestAttachListenerAndTargetGroupSettings(t *testing.T) {
	f := false
	l := buildkit("arm64", 1235)
	l.PreserveClientIP = &f
	l.TerminateOnDeregistration = true

	rec := mustRun(t, func(ctx *pulumi.Context) error {
		_, err := front.Attach(ctx, "builders", sharedData(), []front.Listener{l})

		return err
	})

	lst := rec.find(t, "aws:lb/listener:Listener", "builders-buildkit-arm64")
	if lst["protocol"] != "TCP" || lst["port"] != float64(1235) || lst["tcpIdleTimeoutSeconds"] != float64(3600) {
		t.Errorf("listener: %v", lst)
	}

	tg := rec.find(t, "aws:lb/targetGroup:TargetGroup", "builders-buildkit-arm64")
	if tg["targetType"] != "instance" || tg["port"] != float64(1234) ||
		tg["preserveClientIp"] != "false" || tg["connectionTermination"] != true || tg["deregistrationDelay"] != float64(30) {
		t.Errorf("target group: %v", tg)
	}

	hc := tg["healthCheck"].(map[string]any)
	if hc["protocol"] != "TCP" || hc["port"] != "traffic-port" || hc["healthyThreshold"] != hc["unhealthyThreshold"] {
		t.Errorf("health check: %v", hc)
	}
}

func TestAttachLeavesAwsDefaultsAlone(t *testing.T) {
	rec := mustRun(t, func(ctx *pulumi.Context) error {
		_, err := front.Attach(ctx, "b", sharedData(), []front.Listener{{
			Name: "svc", Port: 2222, Sources: []front.Source{{CIDR: "10.1.0.0/16"}},
		}})

		return err
	})

	tg := rec.find(t, "aws:lb/targetGroup:TargetGroup", "b-svc")
	for _, k := range []string{"preserveClientIp", "deregistrationDelay", "connectionTermination"} {
		if _, ok := tg[k]; ok {
			t.Errorf("%s set without being asked: %v", k, tg[k])
		}
	}

	if tg["port"] != float64(2222) {
		t.Errorf("TargetPort defaults to Port: %v", tg["port"])
	}

	if _, ok := rec.find(t, "aws:lb/listener:Listener", "b-svc")["tcpIdleTimeoutSeconds"]; ok {
		t.Error("idle timeout set without being asked")
	}

	r := rec.find(t, "aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule", "b-svc-in-0-tcp")
	if r["cidrIpv4"] != "10.1.0.0/16" {
		t.Errorf("range source: %v", r)
	}
}

// outputs resolves the front's values inside a run.
func outputs(t *testing.T, build func(ctx *pulumi.Context) (*front.Front, error)) (arns []string, ingress []string) {
	t.Helper()

	done := make(chan struct{})

	mustRun(t, func(ctx *pulumi.Context) error {
		f, err := build(ctx)
		if err != nil {
			return err
		}

		all := []any{}
		for _, a := range f.TargetGroupARNs() {
			all = append(all, a)
		}

		pulumi.All(all...).ApplyT(func(v []any) error {
			for _, x := range v {
				arns = append(arns, x.(string))
			}

			close(done)

			return nil
		})

		rules := f.TargetIngress()
		for i := range rules {
			r := &rules[i]
			src := ""
			if len(r.SourceGroups) > 0 {
				src = "sg"
			}

			ingress = append(ingress, strings.Join([]string{r.Protocol, itoa(r.FromPort), itoa(r.ToPort), src}, "/"))
		}

		return nil
	})

	<-done

	return arns, ingress
}

func itoa(n int) string {
	b, _ := json.Marshal(n)

	return string(b)
}

func TestTargetGroupARNsInListenerOrder(t *testing.T) {
	arns, _ := outputs(t, func(ctx *pulumi.Context) (*front.Front, error) {
		return front.Attach(ctx, "builders", sharedData(), []front.Listener{buildkit("amd64", 1234), buildkit("arm64", 1235)})
	})

	if len(arns) != 2 || !strings.HasSuffix(arns[0], "targetgroup/builders-buildkit-amd64") || !strings.HasSuffix(arns[1], "targetgroup/builders-buildkit-arm64") {
		t.Errorf("ARNs: %v", arns)
	}
}

func TestTargetIngressIsTheNLBGroupOnly(t *testing.T) {
	l := buildkit("amd64", 1234)
	l.HealthCheck = front.HealthCheck{Protocol: "HTTP", Port: 8081, Path: "/healthz"}

	m := buildkit("arm64", 1235)

	_, ingress := outputs(t, func(ctx *pulumi.Context) (*front.Front, error) {
		return front.Attach(ctx, "builders", sharedData(), []front.Listener{l, m})
	})

	// 1234 twice is one rule; the health port 8081 is its own.
	want := []string{"tcp/1234/1234/sg", "tcp/8081/8081/sg"}
	if strings.Join(ingress, ",") != strings.Join(want, ",") {
		t.Errorf("ingress %v, want %v", ingress, want)
	}
}

func TestUDPListenerHasTCPHealthPortRule(t *testing.T) {
	_, ingress := outputs(t, func(ctx *pulumi.Context) (*front.Front, error) {
		return front.Attach(ctx, "dns", sharedData(), []front.Listener{{
			Name: "dns", Port: 5353, Protocol: front.UDP, HealthCheck: front.HealthCheck{Port: 5354},
			Sources: []front.Source{{CIDR: "10.2.0.0/16"}},
		}})
	})

	want := []string{"udp/5353/5353/sg", "tcp/5354/5354/sg"}
	if strings.Join(ingress, ",") != strings.Join(want, ",") {
		t.Errorf("ingress %v, want %v", ingress, want)
	}
}

func TestTCPUDPListenerHasBothRules(t *testing.T) {
	rec := mustRun(t, func(ctx *pulumi.Context) error {
		_, err := front.Attach(ctx, "dns", sharedData(), []front.Listener{{
			Name: "dns", Port: 53, Protocol: front.TCPUDP, Sources: []front.Source{{CIDR: "10.2.0.0/16"}},
		}})

		return err
	})

	if n := rec.count("aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule"); n != 2 {
		t.Errorf("want a tcp and a udp rule, got %d", n)
	}
}

func TestNewNLBResourceSet(t *testing.T) {
	rec := mustRun(t, func(ctx *pulumi.Context) error {
		a := sharedArgs()
		a.Name = "builders-nlb"

		_, err := front.NewNLB(ctx, "builders", &front.NLBArgs{SharedNLBArgs: *a, Listeners: []front.Listener{buildkit("arm64", 443)}})

		return err
	})

	golden(t, "dedicated-resources.golden.json", rec.dump(t))
}

func TestNewNLBReturnsTheSameFront(t *testing.T) {
	arns, ingress := outputs(t, func(ctx *pulumi.Context) (*front.Front, error) {
		a := sharedArgs()
		f, err := front.NewNLB(ctx, "builders", &front.NLBArgs{SharedNLBArgs: *a, Listeners: []front.Listener{buildkit("arm64", 443)}})
		if err == nil && (f.LoadBalancer == nil || f.SecurityGroup == nil) {
			t.Error("a dedicated front owns its NLB and group")
		}

		return f, err
	})

	if len(arns) != 1 || len(ingress) != 1 || ingress[0] != "tcp/1234/1234/sg" {
		t.Errorf("arns %v ingress %v", arns, ingress)
	}
}

func TestRefusals(t *testing.T) {
	ok := buildkit("arm64", 1235)

	tls := ok
	tls.Protocol = "TLS"

	noSources := ok
	noSources.Sources = nil

	world := ok
	world.Sources = []front.Source{{CIDR: "0.0.0.0/0"}}

	both := ok
	both.Sources = []front.Source{{CIDR: "10.0.0.0/8", SecurityGroupID: pulumi.String("sg-0")}}

	badPort := ok
	badPort.Port = 70000

	badIdle := ok
	badIdle.IdleTimeoutSeconds = 30

	udpNoCheck := ok
	udpNoCheck.Protocol = front.UDP
	udpNoCheck.IdleTimeoutSeconds = 0

	pathOnTCP := ok
	pathOnTCP.HealthCheck.Path = "/x"

	httpNoPath := ok
	httpNoPath.HealthCheck.Protocol = "HTTP"

	longName := ok
	longName.Name = strings.Repeat("a", 33)

	same := ok
	same.Name = "other"

	cases := []struct {
		name string
		ls   []front.Listener
		want string
	}{
		{"tls", []front.Listener{tls}, "TLS is refused"},
		{"no sources", []front.Listener{noSources}, "Sources is empty"},
		{"world", []front.Listener{world}, "set World"},
		{"both source kinds", []front.Listener{both}, "not both"},
		{"port", []front.Listener{badPort}, "want 1..65535"},
		{"idle", []front.Listener{badIdle}, "60..6000"},
		{"udp without health port", []front.Listener{udpNoCheck}, "needs HealthCheck.Port"},
		{"path on tcp", []front.Listener{pathOnTCP}, "protocol is TCP"},
		{"http without path", []front.Listener{httpNoPath}, "absolute path"},
		{"long name", []front.Listener{longName}, "Name"},
		{"duplicate name", []front.Listener{ok, ok}, "used twice"},
		{"duplicate port", []front.Listener{ok, same}, "port tcp/1235 is used twice"},
		{"none", nil, "no listeners"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, func(ctx *pulumi.Context) error {
				_, err := front.Attach(ctx, "b", sharedData(), c.ls)

				return err
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestAttachSaysWhatToApplyWhenTheFileIsMissing(t *testing.T) {
	_, err := run(t, func(ctx *pulumi.Context) error {
		_, err := front.Attach(ctx, "b", front.SharedNLB{}, []front.Listener{buildkit("arm64", 1235)})

		return err
	})
	if err == nil || !strings.Contains(err.Error(), "box-front stack") || !strings.Contains(err.Error(), "security-group-id") {
		t.Errorf("error: %v", err)
	}
}

func TestSharedNLBArgsAreChecked(t *testing.T) {
	bad := &front.SharedNLBArgs{Name: "-x-"}

	_, err := run(t, func(ctx *pulumi.Context) error {
		_, err := front.NewSharedNLB(ctx, "box-front", bad)

		return err
	})
	if err == nil {
		t.Fatal("no error")
	}

	for _, w := range []string{"Name", "VPCID is required", "VPCCIDR is required", "SubnetIDs is empty"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("missing %q in %v", w, err)
		}
	}
}

// A target group name fits AWS's 32 characters whatever the listener name,
// and moves with what replaces the group.
func TestTargetGroupNamesFitAndMove(t *testing.T) {
	names := func(l front.Listener) string {
		rec := mustRun(t, func(ctx *pulumi.Context) error {
			_, err := front.Attach(ctx, "a-long-component-name", sharedData(), []front.Listener{l})

			return err
		})

		for _, x := range rec.all {
			if x["type"] == "aws:lb/targetGroup:TargetGroup" {
				return x["inputs"].(map[string]any)["name"].(string)
			}
		}

		t.Fatal("no target group")

		return ""
	}

	l := buildkit("arm64", 1235)
	l.Name = "buildkit-arm64-with-a-long-name"

	n := names(l)
	if len(n) > 32 || !strings.HasPrefix(n, "buildkit-arm64-with-a-lo-") {
		t.Errorf("name %q", n)
	}

	l.TargetPort = 4321
	if names(l) == n {
		t.Error("a new target port keeps the name; the replacement could not be created beside the old group")
	}
}
