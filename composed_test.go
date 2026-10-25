package asg_test

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/modules/selfregister"
	"github.com/truvity/asg/modules/ssh"
	"github.com/truvity/asg/modules/tunnel"
	"github.com/truvity/asg/modules/watchdog"
)

// composedBox is the status box's step B shape: a consumer fragment of its
// own (the program and its setup script, delivered as a verified object) and
// the four modules the status box uses.
func composedBox(t *testing.T) *asg.Args {
	t.Helper()

	names := asg.Names{Base: "box-status"}

	p, err := bootstrap.New(bootstrap.Options{
		Lifecycle: names.Resolve("status").Lifecycle(),
		Objects:   &bootstrap.ObjectStore{Bucket: "acme-status-replica", Prefix: "box"},
	})
	if err != nil {
		t.Fatal(err)
	}

	own := bootstrap.Compose("box",
		bootstrap.File{Path: "/etc/box/params.sh", Content: "BUCKET='acme-status-replica'\n", Delivery: bootstrap.DeliveryInline},
		bootstrap.File{Path: "/usr/local/sbin/box-setup.sh", Mode: "0755", Content: "#!/bin/bash\necho setup\n", Delivery: bootstrap.DeliveryObject},
		bootstrap.Step{Name: "box install", Script: "/usr/local/sbin/box-setup.sh install\n"},
	)

	conn := bootstrap.UnitFragment{
		Install:       "echo install-cloudflared\n",
		Unit:          "[Service]\nExecStart=/usr/local/bin/cloudflared --no-autoupdate tunnel run\n",
		UnitPath:      "/etc/systemd/system/cloudflared.service",
		LoadToken:     "echo load-token\n",
		Start:         "systemctl start cloudflared.service\n",
		SSMParameters: []string{"/acme/status/tunnel-token"},
	}

	mods, err := asg.Collect(
		watchdog.New(watchdog.Args{Checks: []asg.WatchCheck{{Name: "box", Script: "curl -fsS http://127.0.0.1:8080/health >/dev/null\n"}}}),
		ssh.New(ssh.Args{
			Bundle:           ssh.Bundle{Packages: []string{"opkssh"}, Commands: []string{"echo configure\n"}},
			HostKeyParameter: "/acme/status/ssh-host-key", IngressCIDRs: []string{"10.20.0.0/16"},
		}),
		selfregister.New(selfregister.Args{
			RoleARN:      "ar" + "n:" + part + ":iam::" + testAccount + ":role/dns-register",
			HostedZoneID: "Z0EXAMPLE12345", RecordName: "box.example.test",
		}),
		tunnel.New(tunnel.Args{Connector: conn, MetricsPort: 20241, Watchdog: true}),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Add(own); err != nil {
		t.Fatal(err)
	}

	if err := mods.AddTo(p); err != nil {
		t.Fatal(err)
	}

	if err := p.AddSSMParameters("/acme/status/gatus-env"); err != nil {
		t.Fatal(err)
	}

	rendered, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}

	a := statusBoxArgs(t, rendered)
	a.Names = names
	a.IAM.Policy = nil
	a.IAM.Statements = []asg.Statement{{
		Sid:      "ReplicaObjects",
		Action:   []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"},
		Resource: []string{"ar" + "n:" + part + ":s3:::acme-status-replica/box/*"},
	}}
	a.Contribution = mods
	// The setup script is the plan's own object now, not an extra one.
	a.Objects.Extra = a.Objects.Extra[1:]
	a.Objects.Tags = nil
	a.Refresh = &asg.RefreshArgs{}

	return a
}

// TestComposedBoxResourceSet is the step B review golden: the resource set a
// box made of the plan and four modules asks for, with the generated policy,
// the security group rule of the ssh module and no refresh trigger.
func TestComposedBoxResourceSet(t *testing.T) {
	rec := mustRun(t, "status", composedBox(t))
	golden(t, "composed-resources.golden.json", rec.dump(t))

	g := rec.find(t, "aws:autoscaling/group:Group", "status")
	if _, ok := g["instanceRefresh"].(map[string]any)["triggers"]; ok {
		t.Error("the group still names a refresh trigger")
	}

	sg := rec.find(t, "aws:ec2/securityGroup:SecurityGroup", "status")
	if in := sg["ingress"].([]any); len(in) != 2 {
		t.Errorf("ingress %v, want the private page and ssh", in)
	}
}

func TestComposedBoxUserDataGolden(t *testing.T) {
	a := composedBox(t)
	golden(t, "composed-userdata.golden.yaml", []byte(a.UserData.CloudConfig))

	for _, o := range a.UserData.Objects {
		if !strings.HasSuffix(o.Key, ".sh") || !strings.HasPrefix(o.Key, "box/config/") {
			t.Errorf("object key %s", o.Key)
		}
	}
}

// TestPolicyOutputIsTheExactDocument: Appliance.Policy is the role's policy
// as exact JSON, the review artifact; it equals what the role carries.
func TestPolicyOutputIsTheExactDocument(t *testing.T) {
	a := composedBox(t)

	var got string

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		app, err := asg.New(ctx, "status", a)
		if err != nil {
			return err
		}

		ctx.Export("policy", app.Policy.ApplyT(func(s string) string { got = s; return s }).(pulumi.StringOutput))

		return nil
	}, pulumi.WithMocks("p", "s", &recorder{}))
	if err != nil {
		t.Fatal(err)
	}

	// Object keys carry the content digest; the golden does not.
	got = shaRe.ReplaceAllString(got, "<sha256>")
	golden(t, "composed-policy.golden.json", []byte(got+"\n"))

	rec := mustRun(t, "status", composedBox(t))
	role := rec.find(t, "aws:iam/role:Role", "status")["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)

	if role != got {
		t.Error("the Policy output differs from the role's inline policy")
	}

	for _, want := range []string{"SelfRegisterDNS", "ReadParameters", "OwnGroup", "ReplicaObjects", "ssh-host-key", "tunnel-token", "gatus-env"} {
		if !strings.Contains(got, want) {
			t.Errorf("policy lacks %s", want)
		}
	}
}
