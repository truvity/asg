package asg_test

import (
	"strings"
	"testing"

	"github.com/truvity/asg"
)

// builderArgs is the shape of a builder box: a data volume, a warm pool that
// takes scaled-in instances back, launch-before-terminate and a termination
// policy that keeps the oldest instance.
func builderArgs(t *testing.T) *asg.Args {
	t.Helper()

	a := baseArgs(t)
	a.Instance.Type = "m7g.xlarge"
	a.Instance.RootVolume = &asg.RootVolumeArgs{SizeGiB: 20}
	a.Instance.Volumes = []asg.VolumeArgs{{DeviceName: "/dev/sdf", SizeGiB: 100}}
	a.Group.Min, a.Group.Max = 1, 3
	a.Group.TerminationPolicies = []string{"NewestInstance"}
	a.WarmPool = &asg.WarmPoolArgs{MinSize: 1, ReuseOnScaleIn: true}
	a.Refresh = &asg.RefreshArgs{MinHealthyPercentage: 100, MaxHealthyPercentage: 200}

	return a
}

func TestBuilderResourceSet(t *testing.T) {
	golden(t, "builder-resources.golden.json", mustRun(t, "builder", builderArgs(t)).dump(t))
}

func TestDataVolumesAreEncryptedAndDeletedWithTheInstance(t *testing.T) {
	a := builderArgs(t)
	a.Instance.Volumes[0].IOPS, a.Instance.Volumes[0].ThroughputMiBps = 4000, 250

	lt := mustRun(t, "builder", a).find(t, "aws:ec2/launchTemplate:LaunchTemplate", "builder")

	bdm := lt["blockDeviceMappings"].([]any)
	if len(bdm) != 2 {
		t.Fatalf("block devices %v", bdm)
	}

	data := bdm[1].(map[string]any)
	ebs := data["ebs"].(map[string]any)

	if data["deviceName"] != "/dev/sdf" || ebs["volumeSize"] != float64(100) || ebs["volumeType"] != "gp3" ||
		ebs["encrypted"] != "true" || ebs["deleteOnTermination"] != "true" || ebs["iops"] != float64(4000) || ebs["throughput"] != float64(250) {
		t.Errorf("data volume %v", data)
	}

	if lt["instanceType"] != "m7g.xlarge" {
		t.Errorf("instance type %v", lt["instanceType"])
	}
}

func TestWarmPoolReuseAndTerminationPolicies(t *testing.T) {
	g := mustRun(t, "builder", builderArgs(t)).find(t, "aws:autoscaling/group:Group", "builder")

	wp := g["warmPool"].(map[string]any)
	if r, _ := wp["instanceReusePolicy"].(map[string]any); r == nil || r["reuseOnScaleIn"] != true {
		t.Errorf("warm pool %v", wp)
	}

	if tp, _ := g["terminationPolicies"].([]any); len(tp) != 1 || tp[0] != "NewestInstance" {
		t.Errorf("termination policies %v", g["terminationPolicies"])
	}

	if _, ok := baseGroup(t)["terminationPolicies"]; ok {
		t.Error("termination policies set without being asked")
	}
}

func baseGroup(t *testing.T) map[string]any {
	t.Helper()

	return mustRun(t, "router", baseArgs(t)).find(t, "aws:autoscaling/group:Group", "router")
}

func TestSeveralTypesMakeAMixedInstancesPolicy(t *testing.T) {
	a := baseArgs(t)
	a.Instance.Type = ""
	a.Instance.Types = []string{"m7g.xlarge", "m6g.xlarge"}

	rec := mustRun(t, "router", a)

	if lt := rec.find(t, "aws:ec2/launchTemplate:LaunchTemplate", "router"); lt["instanceType"] != nil {
		t.Errorf("the launch template names a type: %v", lt["instanceType"])
	}

	g := rec.find(t, "aws:autoscaling/group:Group", "router")
	if g["launchTemplate"] != nil {
		t.Errorf("the group names its launch template directly: %v", g["launchTemplate"])
	}

	mip := g["mixedInstancesPolicy"].(map[string]any)
	dist := mip["instancesDistribution"].(map[string]any)

	if dist["onDemandAllocationStrategy"] != "prioritized" || dist["onDemandPercentageAboveBaseCapacity"] != float64(100) {
		t.Errorf("distribution %v", dist)
	}

	ov := mip["launchTemplate"].(map[string]any)["overrides"].([]any)
	if len(ov) != 2 || ov[0].(map[string]any)["instanceType"] != "m7g.xlarge" || ov[1].(map[string]any)["instanceType"] != "m6g.xlarge" {
		t.Errorf("overrides %v", ov)
	}
}

func TestGroupActionsJoinTheOwnGroupStatement(t *testing.T) {
	c, err := asg.Collect(mod{asg.Contribution{GroupActions: []string{"autoscaling:EnterStandby", "autoscaling:ExitStandby"}}})
	if err != nil {
		t.Fatal(err)
	}

	a := baseArgs(t)
	a.Contribution = c
	pol := mustRun(t, "router", a).find(t, "aws:iam/role:Role", "router")["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)

	if strings.Count(pol, `"Sid": "OwnGroup"`) != 1 {
		t.Fatalf("want one OwnGroup statement:\n%s", pol)
	}

	for _, want := range []string{"autoscaling:CompleteLifecycleAction", "autoscaling:EnterStandby", "autoscaling:ExitStandby", "autoscaling:SetInstanceHealth"} {
		if !strings.Contains(pol, want) {
			t.Errorf("policy lacks %s", want)
		}
	}

	// Without a hook the modules' actions still get a statement of their own.
	a = baseArgs(t)
	a.Hook = nil
	a.Contribution = c
	pol = mustRun(t, "router", a).find(t, "aws:iam/role:Role", "router")["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)

	if !strings.Contains(pol, "autoscaling:EnterStandby") || strings.Contains(pol, "CompleteLifecycleAction") {
		t.Errorf("policy without a hook:\n%s", pol)
	}
}

func TestBuilderRefusals(t *testing.T) {
	cases := map[string]struct {
		mut  func(*asg.Args)
		want string
	}{
		"type and types":   {func(a *asg.Args) { a.Instance.Types = []string{"m7g.large"} }, "both set"},
		"types, warm pool": {func(a *asg.Args) { a.Instance.Type = ""; a.Instance.Types = []string{"m7g.large", "m6g.large"} }, "warm pool"},
		"duplicate type": {func(a *asg.Args) {
			a.WarmPool = nil
			a.Instance.Type = ""
			a.Instance.Types = []string{"m7g.large", "m7g.large"}
		}, "twice"},
		"device":            {func(a *asg.Args) { a.Instance.Volumes[0].DeviceName = "/dev/xvda" }, "/dev/sdf to /dev/sdp"},
		"same device":       {func(a *asg.Args) { a.Instance.Volumes = append(a.Instance.Volumes, a.Instance.Volumes[0]) }, "used twice"},
		"size":              {func(a *asg.Args) { a.Instance.Volumes[0].SizeGiB = 0 }, "SizeGiB 0"},
		"volume type":       {func(a *asg.Args) { a.Instance.Volumes[0].Type = "standard2" }, "want gp3"},
		"throughput on gp2": {func(a *asg.Args) { a.Instance.Volumes[0].Type, a.Instance.Volumes[0].ThroughputMiBps = "gp2", 200 }, "only gp3"},
		"termination":       {func(a *asg.Args) { a.Group.TerminationPolicies = []string{"Youngest"} }, "not a termination policy"},
		"group action": {func(a *asg.Args) {
			a.Contribution = asg.Contribution{GroupActions: []string{"autoscaling:*"}}
		}, "no wildcard"},
	}

	for name, tc := range cases {
		a := builderArgs(t)
		tc.mut(a)

		_, err := run(t, "builder", a)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
}
