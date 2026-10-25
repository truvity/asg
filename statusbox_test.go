package asg_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
	"github.com/truvity/asg/bootstrap"
)

// The status box (truvity/observability pkg/statusbox/ec2, consumed by
// gitops stack status-ec2) is the first asg. testdata/
// statusbox-resources.golden.json is the resource set its NewEC2 asks the
// provider for under Pulumi mocks (component "status", boundary, Session
// Manager and one tag set), recorded from that module: type, logical name,
// parent and every input. User-data and object bodies are replaced by
// placeholders, and every sha256 by <sha256>, because they are the caller's
// text. testdata/statusbox-policy.tmpl.json is its role policy with
// REGION, ACCOUNT and PARTITION placeholders.

const statusPolicy = "testdata/statusbox-policy.tmpl.json"

func hexSum(s string) string {
	h := sha256.Sum256([]byte(s))

	return hex.EncodeToString(h[:])
}

// statusBoxArgs is how the status box adopts the library in step A.
func statusBoxArgs(t *testing.T, userData *bootstrap.Rendered) *asg.Args {
	t.Helper()

	tmpl, err := os.ReadFile(statusPolicy)
	if err != nil {
		t.Fatal(err)
	}

	setup, cfg := "setup script body", "storage:\n  type: sqlite\n"
	objs := []bootstrap.Object{
		{Key: "box/config/" + hexSum(setup) + ".sh", SHA256: hexSum(setup), Content: []byte(setup)},
		{Key: "box/config/" + hexSum(cfg) + ".yaml", SHA256: hexSum(cfg), Content: []byte(cfg)},
	}
	names := map[string]string{objs[0].SHA256: "setup-script", objs[1].SHA256: "config-ops"}
	tagNames := map[string]string{objs[0].SHA256: "status-setup-script", objs[1].SHA256: "status-ops"}

	yes := true
	two := 2

	return &asg.Args{
		Names: asg.Names{Base: "statusbox-status"},
		Tags:  map[string]string{"Estate": "acme"},

		UserData: userData,
		Objects: asg.ObjectsArgs{
			Bucket: "acme-status-replica",
			Extra:  objs,
			Tags: func(o bootstrap.Object) map[string]string {
				return map[string]string{"Name": tagNames[o.SHA256], "ManagedBy": "pulumi", "Estate": "acme"}
			},
		},
		Image:    asg.ImageArgs{SSMParameter: "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"},
		Instance: asg.InstanceArgs{Type: "t4g.nano", RootVolume: &asg.RootVolumeArgs{}, PublicIP: &yes},
		Group:    asg.GroupArgs{SubnetIDs: []pulumi.StringInput{pulumi.String("subnet-0a"), pulumi.String("subnet-0b")}},
		Hook:     &asg.HookArgs{},
		WarmPool: &asg.WarmPoolArgs{MinSize: 1, MaxGroupPreparedCapacity: &two},
		Refresh:  &asg.RefreshArgs{},
		IAM: asg.IAMArgs{
			PolicyName:          "statusbox",
			PermissionsBoundary: "arn:" + part + ":iam::" + testAccount + ":policy/boundary",
			ManagedPolicyARNs:   []string{"arn:" + part + ":iam::" + part + ":policy/AmazonSSMManagedInstanceCore"},
			Policy: func(pc asg.PolicyContext) (string, error) {
				s := strings.ReplaceAll(string(tmpl), "REGION", pc.Region)
				s = strings.ReplaceAll(s, "PARTITION", pc.Partition)

				return strings.TrimSpace(strings.ReplaceAll(s, "ACCOUNT", pc.Account)), nil
			},
		},
		Security: asg.SecurityArgs{
			VPCID:         pulumi.String("vpc-0example"),
			Description:   "statusbox: no inbound except the private page from the listed networks",
			DeleteTimeout: "30m",
			Ingress: []asg.Rule{{
				Description: "the private status page, from the peered network",
				FromPort:    8081, ToPort: 8081,
				CIDRs: []string{"10.20.0.0/16"},
			}},
		},
		Compat: asg.Compat{
			ComponentType:          "statusbox:ec2:Box",
			GroupTagsExcludeCommon: true,
			Names: asg.Logical{
				ManagedPolicy: func(int, string) string { return "status-ssm-core" },
				Object:        func(o bootstrap.Object) string { return "status-" + names[o.SHA256] },
			},
		},
	}
}

func verbatim(t *testing.T) *bootstrap.Rendered {
	t.Helper()

	r, err := bootstrap.Verbatim("#cloud-config\n# the status box's old user-data, carried unchanged\n", 0)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

// TestStatusBoxStepAReproducesTheResourceSet is the zero-replace proof: with
// the old user-data verbatim and the Compat hooks, the library asks for the
// same resources, with the same types, names, parents and inputs, as the
// module it replaces.
func TestStatusBoxStepAReproducesTheResourceSet(t *testing.T) {
	a := statusBoxArgs(t, verbatim(t))
	rec, err := run(t, "status", a)
	if err != nil {
		t.Fatal(err)
	}

	want, err := os.ReadFile("testdata/statusbox-resources.golden.json")
	if err != nil {
		t.Fatal(err)
	}

	if got := string(rec.dump(t)); got != string(want) {
		t.Errorf("step A differs from the status box's resource set:\n%s", got)
	}
}

// TestStatusBoxStepBChangesOnlyLaunchTemplateAndPolicy: switching to composed
// user-data and dropping the policy override changes the launch template's
// user-data and the role's policy, and nothing else.
func TestStatusBoxStepBChangesOnlyLaunchTemplateAndPolicy(t *testing.T) {
	a := statusBoxArgs(t, verbatim(t))
	base, err := run(t, "status", a)
	if err != nil {
		t.Fatal(err)
	}

	base.dump(t) // sorts

	p, err := bootstrap.New(bootstrap.Options{Lifecycle: a.Names.Resolve("status").Lifecycle()})
	if err != nil {
		t.Fatal(err)
	}

	if err := p.AddItems(bootstrap.Package{Name: "jq"}, bootstrap.SSMSecret{Parameter: "/acme/status/tunnel-token", Dest: "/run/box/token"}); err != nil {
		t.Fatal(err)
	}

	rendered, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}

	b := statusBoxArgs(t, rendered)
	b.IAM.Policy = nil

	next, err := run(t, "status", b)
	if err != nil {
		t.Fatal(err)
	}

	next.dump(t)

	if next.userData == base.userData || next.userData == "" {
		t.Error("step B must change the launch template's user-data")
	}

	if next.count("aws:autoscaling/group:Group") != 1 || len(next.all) != len(base.all) {
		t.Fatalf("resource count %d, want %d", len(next.all), len(base.all))
	}

	changed := map[string]bool{}

	for i, x := range base.all {
		if string(mustJSON(t, x)) != string(mustJSON(t, next.all[i])) {
			changed[x["type"].(string)] = true
		}
	}

	// The user-data is a placeholder in the dump, so it is compared apart,
	// above; the role's policy is an input of the role.
	if len(changed) != 1 || !changed["aws:iam/role:Role"] {
		t.Errorf("changed types %v, want only the role", changed)
	}
}
