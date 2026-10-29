package asg_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
)

// singleArgs is the shape of an on-demand builder: one plain instance in zone
// a, a persistent cache volume, and a role tag a proxy finds it by.
func singleArgs(t *testing.T) *asg.Args {
	t.Helper()

	a := baseArgs(t)
	a.UserData = composedWith(t, nil)
	a.Hook = nil
	a.Group = asg.GroupArgs{}
	a.Instance.Type = "m7g.xlarge"
	a.Instance.RootVolume = &asg.RootVolumeArgs{SizeGiB: 20}
	a.Single = &asg.SingleArgs{SubnetID: pulumi.String("subnet-0a"), RoleTagKey: "builder-role", RoleTagValue: "ci-builder"}
	a.Data = &asg.DataVolumeArgs{AvailabilityZone: testRegion + "a", SizeGiB: 500, IOPS: 6000, ThroughputMiBps: 250}

	return a
}

func TestSingleInstanceResourceSet(t *testing.T) {
	golden(t, "single-resources.golden.json", mustRun(t, "builder", singleArgs(t)).dump(t))
}

func TestSingleInstanceHasNoGroupHookWarmPoolOrAutoscalingGrant(t *testing.T) {
	rec := mustRun(t, "builder", singleArgs(t))

	for _, typ := range []string{
		"aws:autoscaling/group:Group", "aws:autoscaling/lifecycleHook:LifecycleHook", "aws:autoscaling/attachment:Attachment",
		"aws:ec2/instanceState:InstanceState", "aws:lb/targetGroupAttachment:TargetGroupAttachment",
	} {
		if n := rec.count(typ); n != 0 {
			t.Errorf("%d %s in single-instance mode", n, typ)
		}
	}

	pol := rec.find(t, "aws:iam/role:Role", "builder")["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)
	if strings.Contains(pol, "autoscaling") {
		t.Errorf("an instance without a group needs no autoscaling grant:\n%s", pol)
	}

	lt := rec.find(t, "aws:ec2/launchTemplate:LaunchTemplate", "builder")
	if lt["instanceType"] != "m7g.xlarge" {
		t.Errorf("launch template %v", lt)
	}
}

func TestSingleInstanceIsReplacedDeletedFirstAndRunStateIsNotManaged(t *testing.T) {
	rec := mustRun(t, "builder", singleArgs(t))

	inst := rec.find(t, "aws:ec2/instance:Instance", "builder")
	lt := inst["launchTemplate"].(map[string]any)

	if lt["id"] != "builder_id" || lt["version"] != "3" {
		t.Errorf("launch template reference %v", lt)
	}

	// A stopped instance must show no diff: nothing in the instance's inputs
	// says which state it should be in.
	for _, k := range []string{"instanceState", "state", "ami", "instanceType", "userData", "iamInstanceProfile", "vpcSecurityGroupIds"} {
		if _, ok := inst[k]; ok {
			t.Errorf("instance sets %s: that belongs to the launch template or to the proxy", k)
		}
	}

	o := rec.options(t, "aws:ec2/instance:Instance", "builder")
	if o["deleteBeforeReplace"] != true {
		t.Errorf("the instance is not deleted before it is replaced: %v", o)
	}

	if rc, _ := o["replaceOnChanges"].([]string); len(rc) != 1 || rc[0] != "launchTemplate" {
		t.Errorf("replaceOnChanges %v", o["replaceOnChanges"])
	}

	// The refresh reads the launch template's (gzip) user-data back into the
	// instance; the program never sets it there, so it must not plan to
	// remove it (a perpetual diff that stops and starts the instance).
	ic, _ := o["ignoreChanges"].([]string)
	if !slices.Equal(ic, []string{"userData", "userDataBase64"}) {
		t.Errorf("ignoreChanges %v: the instance must ignore the user-data read back by a refresh", o["ignoreChanges"])
	}

	tags := inst["tags"].(map[string]any)
	if tags["builder-role"] != "ci-builder" || tags["Name"] != "builder" || tags["ManagedBy"] != "pulumi" {
		t.Errorf("instance tags %v", tags)
	}
}

func TestDataVolumeIsEncryptedProtectedAndMovesWithTheInstance(t *testing.T) {
	a := singleArgs(t)
	a.Data.KMSKeyID = "arn:" + part + ":kms:" + testRegion + ":" + testAccount + ":key/0000"

	rec := mustRun(t, "builder", a)

	vol := rec.find(t, "aws:ebs/volume:Volume", "builder-data")
	if vol["availabilityZone"] != testRegion+"a" || vol["size"] != float64(500) || vol["type"] != "gp3" || vol["encrypted"] != true ||
		vol["iops"] != float64(6000) || vol["throughput"] != float64(250) || vol["kmsKeyId"] != a.Data.KMSKeyID {
		t.Errorf("volume %v", vol)
	}

	if tags := vol["tags"].(map[string]any); tags["builder-role"] != "ci-builder" {
		t.Errorf("the volume lacks the role tag: %v", tags)
	}

	o := rec.options(t, "aws:ebs/volume:Volume", "builder-data")
	if o["protect"] != true || o["retainOnDelete"] != true {
		t.Errorf("the volume is not protected and retained: %v", o)
	}

	att := rec.find(t, "aws:ec2/volumeAttachment:VolumeAttachment", "builder-data-attachment")
	if att["deviceName"] != "/dev/sdf" || att["instanceId"] != "builder_id" || att["volumeId"] != "builder-data_id" || att["stopInstanceBeforeDetaching"] != true {
		t.Errorf("attachment %v", att)
	}

	if o := rec.options(t, "aws:ec2/volumeAttachment:VolumeAttachment", "builder-data-attachment"); o["deleteBeforeReplace"] != true {
		t.Errorf("the attachment must be detached before the new one is made: %v", o)
	}
}

func TestSingleInstanceWithoutDataVolumeAndDefaults(t *testing.T) {
	a := singleArgs(t)
	a.Data = nil

	rec := mustRun(t, "builder", a)
	if rec.count("aws:ebs/volume:Volume") != 0 || rec.count("aws:ec2/volumeAttachment:VolumeAttachment") != 0 {
		t.Error("a volume without Data")
	}

	// The default attaches to no target group.
	if rec.count("aws:lb/targetGroupAttachment:TargetGroupAttachment") != 0 {
		t.Error("a target group attachment by default")
	}
}

func TestSingleInstanceCanJoinTargetGroups(t *testing.T) {
	a := singleArgs(t)
	a.Single.TargetGroups = []asg.TargetGroupAttachment{{ARN: pulumi.String("tg-arn")}, {ARN: pulumi.String("tg-arn-2"), Port: 8443}}

	rec := mustRun(t, "builder", a)

	first := rec.find(t, "aws:lb/targetGroupAttachment:TargetGroupAttachment", "builder-target-0")
	second := rec.find(t, "aws:lb/targetGroupAttachment:TargetGroupAttachment", "builder-target-1")

	if first["targetGroupArn"] != "tg-arn" || first["targetId"] != "builder_id" || first["port"] != nil || second["port"] != float64(8443) {
		t.Errorf("attachments %v %v", first, second)
	}
}

func TestSubnetInAnotherZoneThanTheVolumeFailsTheUpdate(t *testing.T) {
	a := singleArgs(t)
	a.Single.SubnetID = pulumi.String("subnet-0b")

	_, err := run(t, "builder", a)
	if err == nil || !strings.Contains(err.Error(), "is in eu-west-1b but Data.AvailabilityZone is eu-west-1a") {
		t.Errorf("error %v", err)
	}
}

func TestSingleInstanceRefusals(t *testing.T) {
	cases := map[string]struct {
		mut  func(*asg.Args)
		want string
	}{
		"hook":      {func(a *asg.Args) { a.Hook = &asg.HookArgs{} }, "Hook is refused"},
		"warm pool": {func(a *asg.Args) { a.WarmPool = &asg.WarmPoolArgs{} }, "WarmPool is refused"},
		"refresh":   {func(a *asg.Args) { a.Refresh = &asg.RefreshArgs{} }, "Refresh is refused"},
		"types": {func(a *asg.Args) {
			a.Instance.Type = ""
			a.Instance.Types = []string{"m7g.large", "m6g.large"}
		}, "one Instance.Type"},
		"min":            {func(a *asg.Args) { a.Group.Min = 1 }, "Group.Min, Max and Desired"},
		"max":            {func(a *asg.Args) { a.Group.Max = 2 }, "Group.Min, Max and Desired"},
		"desired":        {func(a *asg.Args) { a.Group.Desired = 1 }, "Group.Min, Max and Desired"},
		"group subnets":  {func(a *asg.Args) { a.Group.SubnetIDs = []pulumi.StringInput{pulumi.String("subnet-0a")} }, "Group is refused"},
		"group tgs":      {func(a *asg.Args) { a.Group.TargetGroupARNs = []pulumi.StringInput{pulumi.String("x")} }, "Group is refused"},
		"alarms":         {func(a *asg.Args) { a.Alarms.Alarms = []asg.Alarm{{Suffix: "x"}} }, "Alarms are alarms"},
		"lifecycle plan": {func(a *asg.Args) { a.UserData = composed(t) }, "Options.Lifecycle"},
		"group actions": {func(a *asg.Args) {
			a.Contribution = asg.Contribution{GroupActions: []string{"autoscaling:EnterStandby"}}
		}, "GroupActions"},
		"in-service": {func(a *asg.Args) {
			a.Contribution = asg.Contribution{InService: []asg.Step{{Name: "x", Script: "true"}}}
		}, "in-service steps"},
		"public ip":    {func(a *asg.Args) { v := true; a.Instance.PublicIP = &v }, "Instance.PublicIP"},
		"no subnet":    {func(a *asg.Args) { a.Single.SubnetID = nil }, "Single.SubnetID is required"},
		"no tag key":   {func(a *asg.Args) { a.Single.RoleTagKey = "" }, "RoleTagKey"},
		"aws tag key":  {func(a *asg.Args) { a.Single.RoleTagKey = "aws:role" }, "RoleTagKey"},
		"no tag value": {func(a *asg.Args) { a.Single.RoleTagValue = "" }, "RoleTagValue"},
		"tag clash":    {func(a *asg.Args) { a.Tags = map[string]string{"builder-role": "x"} }, "already has the key"},
		"tg arn":       {func(a *asg.Args) { a.Single.TargetGroups = []asg.TargetGroupAttachment{{}} }, "ARN is required"},
		"tg port": {func(a *asg.Args) {
			a.Single.TargetGroups = []asg.TargetGroupAttachment{{ARN: pulumi.String("x"), Port: 70000}}
		}, "Port 70000"},
		"zone":        {func(a *asg.Args) { a.Data.AvailabilityZone = "eu-west-1" }, "Data.AvailabilityZone"},
		"data device": {func(a *asg.Args) { a.Data.DeviceName = "/dev/xvda" }, "Data.DeviceName"},
		"data clash": {func(a *asg.Args) {
			a.Instance.Volumes = []asg.VolumeArgs{{DeviceName: "/dev/sdf", SizeGiB: 10}}
		}, "also Instance.Volumes[0]"},
		"data size":   {func(a *asg.Args) { a.Data.SizeGiB = 0 }, "Data.SizeGiB"},
		"data type":   {func(a *asg.Args) { a.Data.Type = "standard2" }, "Data.Type"},
		"data gp2 tp": {func(a *asg.Args) { a.Data.Type = "gp2" }, "only gp3"},
		"data kms":    {func(a *asg.Args) { a.Data.KMSKeyID = "alias/x" }, "Data.KMSKeyID"},
		"data no single": {func(a *asg.Args) {
			*a = *baseArgs(t)
			a.Data = &asg.DataVolumeArgs{AvailabilityZone: "eu-west-1a", SizeGiB: 10}
		}, "set Single"},
	}

	for name, tc := range cases {
		a := singleArgs(t)
		tc.mut(a)

		_, err := run(t, "builder", a)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
}

// Group mode is untouched by the new fields: baseArgs still builds a group
// with the old defaults (the defaults golden holds the rest).
func TestGroupModeStillDefaultsTheGroup(t *testing.T) {
	g := baseGroup(t)
	if g["minSize"] != float64(1) || g["maxSize"] != float64(1) || g["healthCheckType"] != "EC2" {
		t.Errorf("group %v", g)
	}
}
