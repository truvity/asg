// Package ssh is a module that gives the box SSH access through opkssh (OIDC
// sign-in), optional host certificates and an optional fixed host key from
// SSM, and opens TCP 22 to the listed networks. The ec2-user key pair stays
// off: opkssh is the only way in.
//
// The module does not know how opkssh or the host certificates are set up.
// The caller renders a [Bundle], packages, files and commands, with whatever
// library it uses (truvity/tailscale's hostaccess renders one) and hands it
// over; the module installs the packages, writes the files and runs the
// commands in one fail-safe, time-boxed program on the first boot, so a broken
// SSH setup can never keep the box out of service. Nothing here is secret.
package ssh

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"
)

// Port is the TCP port opened to Args.IngressCIDRs.
const Port = 22

// Paths on the instance.
const (
	// Dir holds the setup program.
	Dir = "/opt/asg/ssh"
	// HostKeyPath is the fixed host key, when HostKeyParameter is set.
	HostKeyPath   = "/etc/ssh/ssh_host_ed25519_key"
	hostKeyDropIn = "/etc/ssh/sshd_config.d/05-asg-hostkey.conf"
	kexDropIn     = "/etc/ssh/sshd_config.d/06-asg-kex.conf"

	// SetupOrder is the order of the install step. It runs before the box's
	// own install step (order 0) and its failure never stops the boot.
	SetupOrder = -10
	timeoutSec = 600
)

var hostKeyParameterRE = regexp.MustCompile(`^/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

// File is a file the bundle writes.
type File struct {
	// Path is absolute.
	Path string
	// Mode is octal such as "0644". Default "0644".
	Mode    string
	Content string
}

// Bundle is what to apply to the host: the packages, the files and the
// commands, in that order.
type Bundle struct {
	Packages []string
	Files    []File
	// Commands are shell scripts run in order, as root, each fail-safe.
	Commands []string
}

// Args configure SSH.
type Args struct {
	// Bundle is the opkssh (and host certificate) setup.
	Bundle Bundle
	// HostKeyParameter, when set, is the name of an SSM SecureString holding
	// the box's one ed25519 host key (OpenSSH private-key format). The role
	// may read exactly that parameter. Before sshd is restarted the box
	// restores it, derives the .pub and limits sshd to that key, so clients
	// can pin one known_hosts line. Fail-safe: on any failure sshd keeps its
	// own generated key. Empty: the box keeps whatever host key it generates.
	HostKeyParameter string
	// IngressCIDRs are the networks allowed to reach TCP 22. Required.
	IngressCIDRs []string
}

type mod struct{ a Args }

// New returns the SSH module.
func New(a Args) module.Module { return mod{a: a} }

func (mod) Name() string { return "ssh" }

func (a Args) validate() error {
	var errs []error

	if len(a.IngressCIDRs) == 0 {
		errs = append(errs, errors.New("IngressCIDRs is empty: name the networks allowed to reach TCP 22"))
	}

	for _, c := range a.IngressCIDRs {
		if p, err := netip.ParsePrefix(c); err != nil || !p.Addr().Is4() {
			errs = append(errs, fmt.Errorf("IngressCIDRs entry %q is not an IPv4 CIDR", c))
		} else if p.Bits() == 0 {
			errs = append(errs, fmt.Errorf("IngressCIDRs entry %q opens SSH to the whole internet", c))
		}
	}

	if p := a.HostKeyParameter; p != "" && !hostKeyParameterRE.MatchString(p) {
		errs = append(errs, fmt.Errorf("HostKeyParameter %q is not an absolute SSM parameter name", p))
	}

	if len(a.Bundle.Packages)+len(a.Bundle.Files)+len(a.Bundle.Commands) == 0 {
		errs = append(errs, errors.New("Bundle is empty: render the opkssh setup and pass it in"))
	}

	return errors.Join(errs...)
}

func (m mod) Contribute() (module.Contribution, error) {
	a := m.a
	if err := a.validate(); err != nil {
		return module.Contribution{}, err
	}

	items := make([]bootstrap.Item, 0, len(a.Bundle.Files)+1)

	for _, f := range a.Bundle.Files {
		items = append(items, bootstrap.File{Path: f.Path, Mode: f.Mode, Content: f.Content, Delivery: bootstrap.DeliveryInline})
	}

	var prog strings.Builder

	prog.WriteString("#!/bin/bash\n# Rendered by github.com/truvity/asg/modules/ssh. Every step is fail-safe.\n")

	if len(a.Bundle.Packages) > 0 {
		fmt.Fprintf(&prog, "dnf install -y %s || echo \"asg ssh: package install failed\"\n", strings.Join(a.Bundle.Packages, " "))
	}

	if p := a.HostKeyParameter; p != "" {
		fmt.Fprintf(&prog, hostKeyRestore, p, HostKeyPath, hostKeyDropIn)
	}

	fmt.Fprintf(&prog, kexPrefer, kexDropIn)

	for _, c := range a.Bundle.Commands {
		prog.WriteString(strings.TrimRight(c, "\n") + "\n")
	}

	items = append(items,
		bootstrap.File{Path: Dir + "/setup.sh", Mode: "0700", Content: prog.String(), Delivery: bootstrap.DeliveryInline},
		bootstrap.Step{
			Name: "ssh setup", Phase: bootstrap.PhaseInstall, Order: SetupOrder,
			// Time-boxed and never fatal: every step runs under set -e.
			Script: fmt.Sprintf("timeout %d bash %s/setup.sh || echo \"asg ssh: setup did not finish; continuing without SSH\"\n", timeoutSec, Dir),
		},
	)

	c := module.Contribution{
		Fragments: []bootstrap.Fragment{bootstrap.Compose("ssh", items...)},
		Ingress: []module.Rule{{
			Description: "SSH (opkssh), from the listed networks", Protocol: "tcp", FromPort: Port, ToPort: Port, CIDRs: a.IngressCIDRs,
		}},
	}
	if a.HostKeyParameter != "" {
		c.SSMParameters = []string{a.HostKeyParameter}
	}

	return c, nil
}

// kexPrefer puts the post-quantum hybrid key exchange first and leaves the
// rest of sshd's own defaults after it (the ^ prefix prepends to the default
// list, so the defaults the installed OpenSSH ships stay current). Amazon
// Linux 2023's OpenSSH supports it. Fail-safe: sshd -t must pass with the
// drop-in, or the drop-in is removed and sshd keeps its defaults.
const kexPrefer = `# Key exchange: prefer the standardised ML-KEM hybrid, then the sntrup hybrid, then sshd's defaults. Fail-safe.
(
  set -euo pipefail
  printf 'KexAlgorithms ^mlkem768x25519-sha256,sntrup761x25519-sha512@openssh.com\n' >%[1]s
  if sshd -t; then systemctl restart sshd; else rm -f %[1]s; exit 1; fi
) || { rm -f %[1]s; echo "asg ssh: post-quantum key exchange not enabled; sshd keeps its defaults"; }
`

const hostKeyRestore = `# Host key: restore the one fixed key from SSM before sshd is restarted. Fail-safe.
(
  set -euo pipefail
  umask 077
  tok="$(curl -sf -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)"
  region="$(curl -sf -H "X-aws-ec2-metadata-token: $tok" http://169.254.169.254/latest/meta-data/placement/region)"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  aws ssm get-parameter --name %[1]s --with-decryption --query Parameter.Value --output text --region "$region" >"$tmp/key"
  ssh-keygen -y -f "$tmp/key" >"$tmp/key.pub"
  install -m 0600 -o root -g root "$tmp/key" %[2]s
  install -m 0644 -o root -g root "$tmp/key.pub" %[2]s.pub
  printf 'HostKey %[2]s\n' >%[3]s
  if sshd -t; then systemctl restart sshd; else rm -f %[3]s; exit 1; fi
) || echo "asg ssh: host key restore failed; sshd keeps its generated key"
`
