package asg_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"
)

// composed renders a plan with a secret, a spilled-to-object file and the
// lifecycle of the appliance named "router".
func composed(t *testing.T) *bootstrap.Rendered {
	t.Helper()

	p, err := bootstrap.New(bootstrap.Options{
		Lifecycle: asg.Names{}.Resolve("router").Lifecycle(),
		Objects:   &bootstrap.ObjectStore{Bucket: "acme-config", Prefix: "router"},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = p.AddItems(
		bootstrap.Package{Name: "jq"},
		bootstrap.File{Path: "/etc/router.conf", Content: "a=1\n", Delivery: bootstrap.DeliveryObject},
		bootstrap.SSMSecret{Parameter: "/acme/router/key", Dest: "/run/router/key"},
		bootstrap.SSMSecret{Parameter: "/acme/router/auth", Dest: "/run/router/auth"},
	)
	if err != nil {
		t.Fatal(err)
	}

	r, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func baseArgs(t *testing.T) *asg.Args {
	t.Helper()

	return &asg.Args{
		UserData: composed(t),
		Objects:  asg.ObjectsArgs{Bucket: "acme-config"},
		Image:    asg.ImageArgs{NameFilter: "al2023-ami-2023.*-kernel-6.1-arm64", Architecture: "arm64"},
		Instance: asg.InstanceArgs{Type: "t4g.small"},
		Group:    asg.GroupArgs{SubnetIDs: []pulumi.StringInput{pulumi.String("subnet-0a")}},
		Hook:     &asg.HookArgs{},
		IAM:      asg.IAMArgs{},
		Security: asg.SecurityArgs{VPCID: pulumi.String("vpc-0example")},
	}
}

func TestDefaultsResourceSet(t *testing.T) {
	rec := mustRun(t, "router", baseArgs(t))
	golden(t, "defaults-resources.golden.json", rec.dump(t))
}

func TestPolicyIsExactlyWhatThePlanReads(t *testing.T) {
	rec := mustRun(t, "router", baseArgs(t))
	role := rec.find(t, "aws:iam/role:Role", "router")
	pol := role["inlinePolicies"].([]any)[0].(map[string]any)

	if pol["name"] != "appliance" {
		t.Errorf("policy name %v", pol["name"])
	}

	golden(t, "policy-plan.golden.json", []byte(pol["policy"].(string)+"\n"))

	// Nothing wider than the plan: no wildcard in any resource.
	if strings.Contains(pol["policy"].(string), "*\"") && !strings.Contains(pol["policy"].(string), "autoScalingGroup:*:autoScalingGroupName/router") {
		t.Error("unexpected wildcard resource")
	}
}

func TestPolicyBuilderGoldenWithExtras(t *testing.T) {
	st := asg.BootstrapStatements(asg.PolicyInput{
		Partition: part, Region: testRegion, Account: testAccount, Group: "g",
		Bucket:        "b",
		SSMParameters: []string{"/z/b", "/a/a", "/z/b"},
		ObjectKeys:    []string{"p/2.sh", "p/1.sh"},
	})
	st = append(st, asg.Statement{
		Sid: "Extra", Action: []string{"kms:Decrypt"}, Resource: []string{"arn:" + part + ":kms:eu-west-1:" + testAccount + ":key/k"},
		Condition: map[string]map[string][]string{"StringEquals": {"kms:ViaService": {"ssm.eu-west-1.amazonaws.com"}}},
	})

	doc, err := asg.Document(st)
	if err != nil {
		t.Fatal(err)
	}

	golden(t, "policy-builder.golden.json", []byte(doc+"\n"))
	golden(t, "assume-role.golden.json", []byte(asg.AssumeRolePolicy(part)+"\n"))
}

func TestNoGroupStatementWithoutAHook(t *testing.T) {
	a := baseArgs(t)
	a.Hook = nil

	rec := mustRun(t, "router", a)
	pol := rec.find(t, "aws:iam/role:Role", "router")["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)

	if strings.Contains(pol, "autoscaling") {
		t.Errorf("a group without a hook needs no autoscaling grant:\n%s", pol)
	}
}

func TestObjectsAreUploadedAndGranted(t *testing.T) {
	a := baseArgs(t)
	rec := mustRun(t, "router", a)

	if n := rec.count("aws:s3/bucketObjectv2:BucketObjectv2"); n != len(a.UserData.Objects) || n == 0 {
		t.Fatalf("%d objects uploaded, plan has %d", n, len(a.UserData.Objects))
	}

	o := rec.find(t, "aws:s3/bucketObjectv2:BucketObjectv2", "router-object-"+a.UserData.Objects[0].SHA256[:12])
	if o["key"] != shaRe.ReplaceAllString(a.UserData.Objects[0].Key, "<sha256>") || o["bucket"] != "acme-config" || o["serverSideEncryption"] != "AES256" {
		t.Errorf("object inputs %v", o)
	}
}

func TestUserDataIsBase64OfRendered(t *testing.T) {
	a := baseArgs(t)
	rec := mustRun(t, "router", a)

	if rec.userData != base64.StdEncoding.EncodeToString(a.UserData.UserData) {
		t.Error("launch template user-data is not the rendered user-data")
	}
}

func TestLaunchTemplateRequiresIMDSv2AndHonoursHopLimit(t *testing.T) {
	a := baseArgs(t)
	a.Instance.HopLimit = 2

	lt := mustRun(t, "router", a).find(t, "aws:ec2/launchTemplate:LaunchTemplate", "router")
	md := lt["metadataOptions"].(map[string]any)

	if md["httpTokens"] != "required" || md["httpPutResponseHopLimit"] != float64(2) {
		t.Errorf("metadata options %v", md)
	}

	if lt["imageId"] != "ami-0example" {
		t.Errorf("image %v", lt["imageId"])
	}
}

func TestSeparateHookAndRulesAndAlarms(t *testing.T) {
	a := baseArgs(t)
	a.Hook = &asg.HookArgs{Mode: asg.HookSeparate}
	a.WarmPool = &asg.WarmPoolArgs{}
	a.Group.EnabledMetrics = []string{"GroupInServiceInstances"}
	a.Security.Style = asg.RulesSeparate
	a.Security.Ingress = []asg.Rule{{Protocol: "udp", FromPort: 41641, ToPort: 41641, CIDRs: []string{"0.0.0.0/0"}, World: true}}
	a.Alarms = asg.AlarmsArgs{
		ActionARNs: []string{"arn:" + part + ":sns:eu-west-1:" + testAccount + ":alerts"},
		Alarms: []asg.Alarm{{
			Suffix: "in-service", Namespace: "AWS/AutoScaling", MetricName: "GroupInServiceInstances", Statistic: "Minimum",
			Comparison: "LessThanThreshold", Threshold: 1, PeriodSeconds: 60, EvaluationPeriods: 3,
		}},
	}

	rec := mustRun(t, "router", a)
	golden(t, "separate-resources.golden.json", rec.dump(t))

	g := rec.find(t, "aws:autoscaling/group:Group", "router")
	if _, ok := g["initialLifecycleHooks"]; ok {
		t.Error("a separate hook must not also be inline")
	}

	h := rec.find(t, "aws:autoscaling/lifecycleHook:LifecycleHook", "router-hook")
	if h["defaultResult"] != "ABANDON" || h["heartbeatTimeout"] != float64(900) {
		t.Errorf("hook %v", h)
	}
}

func TestFlatRegistersNoComponent(t *testing.T) {
	a := baseArgs(t)
	a.Compat.Flat = true

	rec := mustRun(t, "router", a)
	for _, x := range rec.all {
		if x["type"] == asg.TypeToken {
			t.Fatal("a component was registered")
		}

		if p := x["parent"].(string); !strings.HasPrefix(p, "pulumi:pulumi:Stack") {
			t.Errorf("%v has parent %s", x["name"], p)
		}
	}
}

func TestDefaultComponentTypeAndParent(t *testing.T) {
	rec := mustRun(t, "router", baseArgs(t))
	for _, x := range rec.all {
		if x["type"] == "aws:iam/role:Role" && x["parent"] != asg.TypeToken+"::router" {
			t.Errorf("role parent %v", x["parent"])
		}
	}
}

type mod struct {
	c asg.Contribution
}

func (mod) Name() string                            { return "test" }
func (m mod) Contribute() (asg.Contribution, error) { return m.c, nil }

func TestModulesContributeToThePolicyAndTheGroup(t *testing.T) {
	c, err := asg.Collect(mod{asg.Contribution{
		SSMParameters: []string{"/acme/router/hostkey"},
		Ingress:       []module.Rule{{FromPort: 22, ToPort: 22, CIDRs: []string{"10.0.0.0/8"}}},
		Statements:    []asg.Statement{{Sid: "Mod", Action: []string{"ec2:DescribeInstances"}, Resource: []string{"*"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	a := baseArgs(t)
	a.Contribution = c
	rec := mustRun(t, "router", a)

	pol := rec.find(t, "aws:iam/role:Role", "router")["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)
	for _, want := range []string{"parameter/acme/router/hostkey", "ec2:DescribeInstances"} {
		if !strings.Contains(pol, want) {
			t.Errorf("policy lacks %s", want)
		}
	}

	if sg := rec.find(t, "aws:ec2/securityGroup:SecurityGroup", "router"); len(sg["ingress"].([]any)) != 1 {
		t.Errorf("ingress %v", sg["ingress"])
	}
}

func TestResolvedNamesFeedTheLifecycle(t *testing.T) {
	r := asg.Names{Base: "x"}.Resolve("router")
	if l := r.Lifecycle(); l.ASGName != "x" || l.HookName != "x-launch" {
		t.Errorf("lifecycle %+v", l)
	}
}
