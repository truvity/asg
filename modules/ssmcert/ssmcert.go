// Package ssmcert is a module that delivers a TLS certificate, its key and an
// optional CA bundle from SSM Parameter Store into files under /run, for a
// daemon that loads them once at start and never reloads them.
//
// On every boot into service an in-service step reads the three parameters
// (with decryption) into Dir and records the parameter versions it read. A
// timer then compares those versions with the current ones every
// CheckIntervalMinutes. When a parameter has a new version, the box rotates,
// but only inside the nightly window (WindowStartHour to WindowEndHour, UTC):
//
//   - With Standby (the default), the box keeps the group serving while its
//     daemon restarts. It enters Standby without decrementing the desired
//     capacity, so the group puts a replacement in service (from the warm
//     pool, when there is one) and the load balancer drains this instance.
//     It waits for that replacement and for its own connections on DrainPort
//     to end (at most DrainMinutes), loads the new files, restarts
//     RestartUnits and waits for ReadyCheck. It then exits Standby, which
//     raises the desired capacity by one, and sets the desired capacity back
//     to the value it read before: the group's termination policy chooses
//     which instance leaves (NewestInstance keeps this one, with its warm
//     cache, and a warm pool with ReuseOnScaleIn takes the replacement back).
//     When the restart fails, the box leaves Standby by terminating itself
//     without changing the desired capacity, so the replacement keeps
//     serving and nothing is lost but this instance.
//   - Without Standby, the box restarts in place once DrainPort has had no
//     established connection for IdleMinutes, inside the window.
//
// A group that is busy (an instance not InService, fewer in service than
// desired, or no room for one more below the maximum) is left alone; the
// next check tries again.
//
// The role needs ssm:GetParameter on the three parameters (the module lists
// them), kms:Decrypt on their key through SSM (the consumer's statement: the
// key is the consumer's), and, with Standby, the group actions the module
// contributes plus autoscaling:DescribeAutoScalingGroups, which takes no
// resource narrower than "*".
package ssmcert

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"
)

// Defaults.
const (
	DefaultCheckIntervalMinutes = 60
	DefaultWindowStartHour      = 2
	DefaultWindowEndHour        = 5
	DefaultDrainMinutes         = 30
	DefaultIdleMinutes          = 5
	DefaultReadyTimeoutSeconds  = 600
	DefaultReplacementMinutes   = 20

	// LoadOrder is the order of the in-service step that loads the files:
	// before the consumer's start step (which should use an order above it).
	LoadOrder = 100
	// TimerOrder is the order of the step that starts the version check.
	TimerOrder = 850
)

var (
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	paramRe = regexp.MustCompile(`^/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	dirRe   = regexp.MustCompile(`^/run/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	ownerRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*(:[a-z_][a-z0-9_-]*)?$`)
	unitRe  = regexp.MustCompile(`^[A-Za-z0-9_@:.-]+\.service$`)
	groupRe = regexp.MustCompile(`^[A-Za-z0-9 _.:/@+=,-]{1,255}$`)
)

// Args configure the module.
type Args struct {
	// Name labels the files, units and log lines; lower-case letters,
	// digits and -. Default "tls".
	Name string
	// Cert and Key are the parameters of the certificate (PEM, with its
	// chain) and its private key. Required.
	Cert, Key string
	// CA is the parameter of a CA bundle (PEM), such as the client CA a
	// server verifies clients by. Optional.
	CA string
	// Dir is a directory under /run the files go to: cert.pem, key.pem and
	// ca.pem. Default /run/asg-ssmcert/<Name>.
	Dir string
	// Owner owns the directory and the files ("user" or "user:group"), so
	// a daemon that runs as that user can read them. Default root.
	Owner string
	// RestartUnits are restarted on a rotation. Required.
	RestartUnits []string
	// ReadyCheck is a shell script that exits 0 once the restarted daemon
	// serves again. Required.
	ReadyCheck string
	// ReadyTimeoutSeconds bounds ReadyCheck after a restart. Default 600.
	ReadyTimeoutSeconds int
	// CheckIntervalMinutes between version checks. Default 60.
	CheckIntervalMinutes int
	// WindowStartHour and WindowEndHour (UTC, 0..23) bound when a rotation
	// may run; the window may wrap midnight. Default 2 to 5.
	WindowStartHour, WindowEndHour *int
	// DrainPort is the daemon's listening port: its established
	// connections are waited for (Standby) or must be absent (in place).
	// Required.
	DrainPort int

	// InPlace restarts without Standby, when DrainPort has been idle for
	// IdleMinutes (default 5). The default is Standby.
	InPlace     bool
	IdleMinutes int
	// Group and Hook are the box's Auto Scaling group and its launch
	// lifecycle hook (asg.Resolved.Lifecycle). Required for Standby: leaving
	// Standby runs the launch hook again, and the box completes it.
	Group, Hook string
	// DrainMinutes bounds the wait for this instance's connections to end
	// once it is in Standby. Default 30.
	DrainMinutes int
	// ReplacementMinutes bounds the wait for the replacement to be in
	// service; on a timeout the box leaves Standby and tries again later.
	// Default 20.
	ReplacementMinutes int
}

type mod struct{ a Args }

// New returns the module.
func New(a Args) module.Module { return mod{a: a} }

func (m mod) Name() string { return "ssmcert" }

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}

	return *p
}

func (a Args) withDefaults() Args {
	if a.Name == "" {
		a.Name = "tls"
	}

	if a.Dir == "" {
		a.Dir = "/run/asg-ssmcert/" + a.Name
	}

	if a.ReadyTimeoutSeconds == 0 {
		a.ReadyTimeoutSeconds = DefaultReadyTimeoutSeconds
	}

	if a.CheckIntervalMinutes == 0 {
		a.CheckIntervalMinutes = DefaultCheckIntervalMinutes
	}

	if a.IdleMinutes == 0 {
		a.IdleMinutes = DefaultIdleMinutes
	}

	if a.DrainMinutes == 0 {
		a.DrainMinutes = DefaultDrainMinutes
	}

	if a.ReplacementMinutes == 0 {
		a.ReplacementMinutes = DefaultReplacementMinutes
	}

	return a
}

func (a Args) validate() error {
	var errs []error

	add := func(format string, v ...any) { errs = append(errs, fmt.Errorf(format, v...)) }

	if !nameRe.MatchString(a.Name) {
		add("Name %q: want lower-case letters, digits and -, at most 31", a.Name)
	}

	for what, p := range map[string]string{"Cert": a.Cert, "Key": a.Key} {
		if !paramRe.MatchString(p) {
			add("%s %q: want an absolute parameter name like /estate/tls/cert", what, p)
		}
	}

	if a.CA != "" && !paramRe.MatchString(a.CA) {
		add("CA %q: want an absolute parameter name", a.CA)
	}

	if a.Cert == a.Key || (a.CA != "" && (a.CA == a.Cert || a.CA == a.Key)) {
		add("Cert, Key and CA must be three different parameters")
	}

	if !dirRe.MatchString(a.Dir) {
		add("Dir %q: want a directory under /run (tmpfs, so the key never reaches a disk)", a.Dir)
	}

	if a.Owner != "" && !ownerRe.MatchString(a.Owner) {
		add("Owner %q: want user or user:group", a.Owner)
	}

	if len(a.RestartUnits) == 0 {
		add("RestartUnits is empty: a daemon that loads its certificate once must be restarted to serve a new one")
	}

	for _, u := range a.RestartUnits {
		if !unitRe.MatchString(u) {
			add("RestartUnits entry %q: want a service name such as app.service", u)
		}
	}

	if strings.TrimSpace(a.ReadyCheck) == "" {
		add("ReadyCheck is empty: a rotation must know when the daemon serves again")
	}

	start, end := intOr(a.WindowStartHour, DefaultWindowStartHour), intOr(a.WindowEndHour, DefaultWindowEndHour)
	if start < 0 || start > 23 || end < 0 || end > 23 || start == end {
		add("WindowStartHour %d and WindowEndHour %d: want two different hours 0..23", start, end)
	}

	if a.DrainPort < 1 || a.DrainPort > 65535 {
		add("DrainPort %d: want the daemon's port", a.DrainPort)
	}

	if a.ReadyTimeoutSeconds < 10 || a.CheckIntervalMinutes < 5 || a.IdleMinutes < 1 || a.DrainMinutes < 1 || a.ReplacementMinutes < 5 {
		add("want ReadyTimeoutSeconds >= 10, CheckIntervalMinutes >= 5, IdleMinutes >= 1, DrainMinutes >= 1 and ReplacementMinutes >= 5")
	}

	if !a.InPlace && (!groupRe.MatchString(a.Group) || !groupRe.MatchString(a.Hook)) {
		add("Group %q and Hook %q: Standby needs the box's Auto Scaling group and launch hook names (or set InPlace)", a.Group, a.Hook)
	}

	return errors.Join(errs...)
}

// Paths on the instance.
func (a Args) script() string { return module.LibDir + "/ssmcert-" + a.Name + ".sh" }
func (a Args) conf() string   { return "/etc/asg/ssmcert-" + a.Name + ".conf" }
func (a Args) unit() string   { return "asg-ssmcert-" + a.Name }

// Files are the paths the module writes the certificate, key and CA to.
func (a Args) Files() (cert, key, ca string) {
	a = a.withDefaults()

	return a.Dir + "/cert.pem", a.Dir + "/key.pem", a.Dir + "/ca.pem"
}

func (m mod) Contribute() (module.Contribution, error) {
	a := m.a.withDefaults()
	if err := a.validate(); err != nil {
		return module.Contribution{}, err
	}

	standby := 1
	if a.InPlace {
		standby = 0
	}

	q := make([]string, len(a.RestartUnits))
	for i, u := range a.RestartUnits {
		q[i] = module.ShellQuote(u)
	}

	conf := fmt.Sprintf(`# Rendered by github.com/truvity/asg/modules/ssmcert. Parameter names only, no secret.
NAME=%s
CERT_PARAM=%s
KEY_PARAM=%s
CA_PARAM=%s
DIR=%s
OWNER=%s
RESTART_UNITS=(%s)
READY_TIMEOUT=%d
WINDOW_START=%d
WINDOW_END=%d
DRAIN_PORT=%d
STANDBY=%d
GROUP=%s
HOOK=%s
DRAIN_SECONDS=%d
IDLE_SECONDS=%d
REPLACEMENT_SECONDS=%d
`, module.ShellQuote(a.Name), module.ShellQuote(a.Cert), module.ShellQuote(a.Key), module.ShellQuote(a.CA),
		module.ShellQuote(a.Dir), module.ShellQuote(a.Owner), strings.Join(q, " "), a.ReadyTimeoutSeconds,
		intOr(a.WindowStartHour, DefaultWindowStartHour), intOr(a.WindowEndHour, DefaultWindowEndHour), a.DrainPort,
		standby, module.ShellQuote(a.Group), module.ShellQuote(a.Hook), a.DrainMinutes*60, a.IdleMinutes*60, a.ReplacementMinutes*60)

	ready := "#!/bin/bash\n# ssmcert ready check: " + a.Name + "\n" + strings.TrimRight(a.ReadyCheck, "\n") + "\n"

	service := fmt.Sprintf("[Unit]\nDescription=Check %s's certificate parameters for a new version and rotate in the window\n\n"+
		"[Service]\nType=oneshot\nEnvironment=ASG_SSMCERT_CONF=%s\nExecStart=%s check\n", a.Name, a.conf(), a.script())

	// No [Install]: only a boot into service starts the timer.
	timer := fmt.Sprintf("[Unit]\nDescription=Check %s's certificate parameters every %d minutes\n\n"+
		"[Timer]\nOnBootSec=%dmin\nOnUnitActiveSec=%dmin\nRandomizedDelaySec=300\n", a.Name, a.CheckIntervalMinutes,
		a.CheckIntervalMinutes, a.CheckIntervalMinutes)

	params := []string{a.Cert, a.Key}
	if a.CA != "" {
		params = append(params, a.CA)
	}

	c := module.Contribution{
		Fragments: []bootstrap.Fragment{bootstrap.Compose("ssmcert "+a.Name,
			// Auto: the script may move to an S3 object when the user-data is full.
			bootstrap.File{Path: a.script(), Mode: "0755", Content: script},
			bootstrap.File{Path: a.conf(), Mode: "0644", Delivery: bootstrap.DeliveryInline, Content: conf},
			bootstrap.File{Path: "/etc/asg/ssmcert-" + a.Name + "-ready.sh", Mode: "0755", Delivery: bootstrap.DeliveryInline, Content: ready},
			bootstrap.Unit{Name: a.unit() + ".service", Content: service},
			bootstrap.Unit{Name: a.unit() + ".timer", Content: timer},
		)},
		SSMParameters: params,
		InService: []module.Step{
			{Name: "ssmcert-" + a.Name + "-load", Order: LoadOrder, Script: fmt.Sprintf("ASG_SSMCERT_CONF=%s %s load\n", a.conf(), a.script())},
			{Name: "ssmcert-" + a.Name + "-timer", Order: TimerOrder, Script: "systemctl start " + a.unit() + ".timer\n"},
		},
	}

	if !a.InPlace {
		c.GroupActions = []string{
			"autoscaling:EnterStandby", "autoscaling:ExitStandby", "autoscaling:SetDesiredCapacity",
			"autoscaling:TerminateInstanceInAutoScalingGroup",
		}
		c.Statements = []module.Statement{{
			Sid:    "SSMCertReadGroup",
			Effect: "Allow",
			// Describe calls take no resource narrower than "*".
			Action:   []string{"autoscaling:DescribeAutoScalingGroups"},
			Resource: []string{"*"},
		}}
	}

	return c, nil
}
