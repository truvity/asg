package asg_test

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
)

func TestRefusals(t *testing.T) {
	two := 1

	cases := map[string]struct {
		mut  func(*asg.Args)
		want string
	}{
		"no user-data":       {func(a *asg.Args) { a.UserData = nil }, "UserData is nil"},
		"objects, no bucket": {func(a *asg.Args) { a.Objects.Bucket = "" }, "Objects.Bucket is empty"},
		"two image sources":  {func(a *asg.Args) { a.Image.SSMParameter = "/a/b" }, "exactly one of"},
		"no image":           {func(a *asg.Args) { a.Image = asg.ImageArgs{} }, "exactly one of"},
		"bad instance type":  {func(a *asg.Args) { a.Instance.Type = "big" }, "not an instance type"},
		"hop limit":          {func(a *asg.Args) { a.Instance.HopLimit = 65 }, "HopLimit"},
		"no subnets":         {func(a *asg.Args) { a.Group.SubnetIDs = nil }, "SubnetIDs is empty"},
		"sizes":              {func(a *asg.Args) { a.Group.Min, a.Group.Desired, a.Group.Max = 3, 2, 4 }, "Group sizes"},
		"warm pool, no hook": {func(a *asg.Args) { a.Hook = nil; a.WarmPool = &asg.WarmPoolArgs{} }, "WarmPool needs Hook"},
		"warm pool cap": {func(a *asg.Args) {
			a.Group.Max = 3
			a.WarmPool = &asg.WarmPoolArgs{MaxGroupPreparedCapacity: &two}
		}, "below Group.Max"},
		"continue by default": {func(a *asg.Args) { a.Hook.DefaultResult = "CONTINUE" }, "UnsafeDefaultContinue"},
		"heartbeat":           {func(a *asg.Args) { a.Hook.HeartbeatTimeoutSeconds = 5 }, "HeartbeatTimeoutSeconds"},
		"bad boundary":        {func(a *asg.Args) { a.IAM.PermissionsBoundary = "boundary" }, "not a policy ARN"},
		"bad parameter":       {func(a *asg.Args) { a.IAM.SSMParameters = []string{"relative/name"} }, "ssm parameter"},
		"bare wildcard action": {func(a *asg.Args) {
			a.IAM.Statements = []asg.Statement{{Action: []string{"*"}, Resource: []string{"*"}}}
		}, "bare *"},
		"statement no resource": {func(a *asg.Args) { a.IAM.Statements = []asg.Statement{{Action: []string{"ec2:Describe*"}}} }, "no Resource"},
		"policy plus extras": {func(a *asg.Args) {
			a.IAM.Policy = func(asg.PolicyContext) (string, error) { return "{}", nil }
			a.IAM.SSMParameters = []string{"/a/b"}
		}, "IAM.Policy replaces"},
		"world ingress": {func(a *asg.Args) {
			a.Security.Ingress = []asg.Rule{{FromPort: 22, ToPort: 22, CIDRs: []string{"0.0.0.0/0"}}}
		}, "whole internet"},
		"bad cidr": {func(a *asg.Args) {
			a.Security.Ingress = []asg.Rule{{FromPort: 22, ToPort: 22, CIDRs: []string{"10.0.0.1/8"}}}
		}, "canonical IPv4 CIDR"},
		"port range": {func(a *asg.Args) {
			a.Security.Ingress = []asg.Rule{{FromPort: 90, ToPort: 80, CIDRs: []string{"10.0.0.0/8"}}}
		}, "ports 90-80"},
		"no vpc": {func(a *asg.Args) { a.Security.VPCID = nil }, "VPCID is nil"},
		"existing and rules": {func(a *asg.Args) {
			a.Security.ExistingGroups = []pulumi.StringInput{pulumi.String("sg-1")}
		}, "no group is created"},
		"alarm metric not enabled": {func(a *asg.Args) {
			a.Alarms.Alarms = []asg.Alarm{{
				Suffix: "x", Namespace: "AWS/AutoScaling", MetricName: "GroupInServiceInstances", Statistic: "Minimum",
				Comparison: "LessThanThreshold", PeriodSeconds: 60, EvaluationPeriods: 1,
			}}
		}, "EnabledMetrics"},
		"flat plus type": {func(a *asg.Args) { a.Compat.Flat, a.Compat.ComponentType = true, "x:y:Z" }, "Compat.Flat"},
		"bad role name":  {func(a *asg.Args) { a.Names.Role = strings.Repeat("r", 65) }, "Names.Role"},
		"bad tag type":   {func(a *asg.Args) { a.Compat.LaunchTemplateTagTypes = []string{"instances"} }, "not a resource type"},
		"tag type twice": {func(a *asg.Args) {
			a.Compat.LaunchTemplateTagTypes = []string{"instance", "instance"}
		}, "names \"instance\" twice"},
		"trust policy not json": {func(a *asg.Args) { a.Compat.AssumeRolePolicy = "Allow ec2" }, "Compat.AssumeRolePolicy is not a JSON object"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := baseArgs(t)
			c.mut(a)

			rec, err := run(t, "router", a)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want it to contain %q", err, c.want)
			}

			if len(rec.all) != 0 {
				t.Errorf("%d resources registered before the refusal", len(rec.all))
			}
		})
	}
}

func TestRefusesAllProblemsAtOnce(t *testing.T) {
	a := baseArgs(t)
	a.Instance.Type = "big"
	a.Group.SubnetIDs = nil
	a.Security.VPCID = nil

	_, err := run(t, "router", a)
	if err == nil {
		t.Fatal("no error")
	}

	for _, want := range []string{"not an instance type", "SubnetIDs is empty", "VPCID is nil"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestNilArgs(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := asg.New(ctx, "x", nil)

		return err
	}, pulumi.WithMocks("p", "s", &recorder{}))
	if err == nil {
		t.Fatal("nil args accepted")
	}
}

func TestContinueIsAllowedWhenAcknowledged(t *testing.T) {
	a := baseArgs(t)
	a.Hook.DefaultResult, a.Hook.UnsafeDefaultContinue = "CONTINUE", true

	if _, err := run(t, "router", a); err != nil {
		t.Fatal(err)
	}
}
