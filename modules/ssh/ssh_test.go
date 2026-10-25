package ssh_test

import (
	"strings"
	"testing"

	"github.com/truvity/asg/internal/testutil"
	"github.com/truvity/asg/modules/ssh"
)

func bundle() ssh.Bundle {
	return ssh.Bundle{
		Packages: []string{"opkssh"},
		Files:    []ssh.File{{Path: "/etc/opk/auth_id", Mode: "0640", Content: "issuer-policy\n"}},
		Commands: []string{"echo configure-opkssh\n"},
	}
}

func TestContributesTheSetupTheRuleAndTheParameter(t *testing.T) {
	c, err := ssh.New(ssh.Args{Bundle: bundle(), HostKeyParameter: "/box/ssh-host-key", IngressCIDRs: []string{"10.20.0.0/16"}}).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	if len(c.Ingress) != 1 || c.Ingress[0].FromPort != 22 || c.Ingress[0].ToPort != 22 || c.Ingress[0].CIDRs[0] != "10.20.0.0/16" || c.Ingress[0].World {
		t.Errorf("ingress %+v", c.Ingress)
	}

	if len(c.SSMParameters) != 1 || c.SSMParameters[0] != "/box/ssh-host-key" {
		t.Errorf("parameters %v", c.SSMParameters)
	}

	r := testutil.Render(t, c)
	testutil.CheckSyntax(t, r)

	if r.Files["/etc/opk/auth_id"] != "issuer-policy\n" {
		t.Errorf("bundle file: %q", r.Files["/etc/opk/auth_id"])
	}

	setup := r.Files["/opt/asg/ssh/setup.sh"]
	for _, want := range []string{
		"dnf install -y opkssh", "echo configure-opkssh", "aws ssm get-parameter --name /box/ssh-host-key",
		"KexAlgorithms ^mlkem768x25519-sha256", "HostKey /etc/ssh/ssh_host_ed25519_key", "if sshd -t",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("setup lacks %q:\n%s", want, setup)
		}
	}

	// The host key is restored before sshd is told to use the bundle's setup.
	if strings.Index(setup, "Host key: restore") > strings.Index(setup, "echo configure-opkssh") {
		t.Error("the host key must be restored before the commands run")
	}

	run := strings.Join(r.RunCmd, "\n")
	if !strings.Contains(run, "timeout 600 bash /opt/asg/ssh/setup.sh || echo") {
		t.Errorf("the setup is not time-boxed and non-fatal:\n%s", run)
	}
}

func TestWithoutAHostKeyThereIsNoParameterAndNoRestore(t *testing.T) {
	c, err := ssh.New(ssh.Args{Bundle: bundle(), IngressCIDRs: []string{"10.20.0.0/16"}}).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	if len(c.SSMParameters) != 0 {
		t.Errorf("parameters %v", c.SSMParameters)
	}

	if strings.Contains(testutil.Render(t, c).Files["/opt/asg/ssh/setup.sh"], "Host key") {
		t.Error("host key restore rendered without a parameter")
	}
}

func TestRefusals(t *testing.T) {
	ok := ssh.Args{Bundle: bundle(), IngressCIDRs: []string{"10.20.0.0/16"}}

	for name, mut := range map[string]func(*ssh.Args){
		"no cidr": func(a *ssh.Args) { a.IngressCIDRs = nil },
		"world":   func(a *ssh.Args) { a.IngressCIDRs = []string{"0.0.0.0/0"} },
		"ipv6":    func(a *ssh.Args) { a.IngressCIDRs = []string{"2001:db8::/32"} },
		"bad key": func(a *ssh.Args) { a.HostKeyParameter = "host key" },
		"empty":   func(a *ssh.Args) { a.Bundle = ssh.Bundle{} },
	} {
		a := ok
		mut(&a)

		if _, err := ssh.New(a).Contribute(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
