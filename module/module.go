package module

import (
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/truvity/asg/bootstrap"
)

// Module is an optional piece of an appliance that touches several layers at
// once: the user-data, the role and the network. A module returns plain data;
// it never creates a resource.
type Module interface {
	// Name identifies the module in errors.
	Name() string
	// Contribute returns what the module adds.
	Contribute() (Contribution, error)
}

// Statement is one IAM policy statement. The JSON field order is fixed, and
// Action and Resource are always arrays, so a document is byte-stable.
type Statement struct {
	Sid       string                         `json:"Sid,omitempty"`
	Effect    string                         `json:"Effect"`
	Action    []string                       `json:"Action"`
	Resource  []string                       `json:"Resource"`
	Condition map[string]map[string][]string `json:"Condition,omitempty"`
}

// Rule is one ingress rule of the box's security group. It is the Pulumi-free
// form of the appliance's own rule type.
type Rule struct {
	Description string
	// Protocol is tcp (default), udp, icmp, icmpv6 or -1.
	Protocol string
	// FromPort and ToPort; both 0 with -1.
	FromPort, ToPort int
	CIDRs            []string
	IPv6CIDRs        []string
	// SourceGroupIDs are security group ids that may reach the port.
	SourceGroupIDs []string
	// World acknowledges that the rule opens a port to 0.0.0.0/0 or ::/0.
	World bool
}

// Step is a shell script that runs under bash -e only when the instance goes
// in service, never in the warm pool. Steps run in the order (Order,
// registration); the order 0 is the default and the modules of this library
// use 100 to 900, so a consumer's own step at 0 runs first.
type Step struct {
	// Name is a lower-case label: letters, digits and -.
	Name   string
	Order  int
	Script string
}

// ReadyCheck is a shell script that must exit 0 before the launch hook is
// completed. The runner retries it every two seconds up to TimeoutSeconds
// (default 120) after the in-service steps ran.
type ReadyCheck struct {
	Name           string
	Script         string
	TimeoutSeconds int
}

// WatchCheck is a shell script the watchdog runs every interval while the
// instance is in service; exit 0 is healthy.
type WatchCheck struct {
	Name   string
	Script string
}

// Drain is a script that would run under a terminate hook before the
// instance leaves. The library has no terminate hook yet, so a Drain is
// refused by [Contribution.AddTo].
type Drain struct {
	Name           string
	Script         string
	TimeoutSeconds int
}

// ScrapeTarget is a local metrics endpoint, for the caller's agent
// configuration. The library records it and does nothing else with it.
type ScrapeTarget struct {
	Name    string
	Address string
	Path    string
}

// Contribution is what modules add to an appliance.
type Contribution struct {
	// Fragments go into the bootstrap plan (see [Contribution.AddTo]).
	Fragments []bootstrap.Fragment
	// Statements are added to the role's policy.
	Statements []Statement
	// SSMParameters and ObjectKeys are readable by the role.
	SSMParameters []string
	ObjectKeys    []string
	// Ingress rules are added to the security group.
	Ingress []Rule
	// ManagedPolicyARNs are attached to the role.
	ManagedPolicyARNs []string
	// GroupActions are Auto Scaling actions the box may call on its own
	// group, such as autoscaling:EnterStandby. The appliance adds them to
	// the statement on the group's ARN, so a module never needs the
	// account, region or name to build one.
	GroupActions []string

	// InService steps run when the instance goes in service.
	InService []Step
	// ReadyChecks gate the launch hook.
	ReadyChecks []ReadyCheck
	// Watch are the checks the watchdog runs.
	Watch []WatchCheck
	// Drains run under a terminate hook (not built yet).
	Drains []Drain
	// Scrape are local metrics endpoints.
	Scrape []ScrapeTarget
}

// Collect merges the modules' contributions in the order given.
func Collect(mods ...Module) (Contribution, error) {
	var (
		out  Contribution
		errs []error
	)

	for _, m := range mods {
		if m == nil {
			errs = append(errs, errors.New("nil module"))

			continue
		}

		c, err := m.Contribute()
		if err != nil {
			errs = append(errs, fmt.Errorf("module %q: %w", m.Name(), err))

			continue
		}

		out.Fragments = append(out.Fragments, c.Fragments...)
		out.Statements = append(out.Statements, c.Statements...)
		out.SSMParameters = append(out.SSMParameters, c.SSMParameters...)
		out.ObjectKeys = append(out.ObjectKeys, c.ObjectKeys...)
		out.Ingress = append(out.Ingress, c.Ingress...)
		out.ManagedPolicyARNs = append(out.ManagedPolicyARNs, c.ManagedPolicyARNs...)
		out.GroupActions = append(out.GroupActions, c.GroupActions...)
		out.InService = append(out.InService, c.InService...)
		out.ReadyChecks = append(out.ReadyChecks, c.ReadyChecks...)
		out.Watch = append(out.Watch, c.Watch...)
		out.Drains = append(out.Drains, c.Drains...)
		out.Scrape = append(out.Scrape, c.Scrape...)
	}

	return out, errors.Join(errs...)
}

// AddTo adds the fragments to the plan, records the SSM parameters and writes
// the in-service steps, ready checks and watch checks as files with the
// runner that executes them. Call it before rendering, once.
func (c Contribution) AddTo(p *bootstrap.Plan) error {
	errs := []error{p.Add(c.Fragments...), p.AddSSMParameters(c.SSMParameters...)}

	if len(c.Drains) > 0 {
		errs = append(errs, errors.New("drains need a terminate lifecycle hook, which this version does not build; remove the module that contributes a drain"))
	}

	errs = append(errs, c.validate())

	if len(c.InService)+len(c.ReadyChecks) > 0 {
		if !p.HasLifecycle() {
			errs = append(errs, errors.New("in-service steps and ready checks complete the launch lifecycle hook: set bootstrap.Options.Lifecycle"))
		} else {
			errs = append(errs, p.Add(c.runnerFragment()))
		}
	}

	if len(c.Watch) > 0 {
		errs = append(errs, p.Add(c.watchFragment()))
	}

	return errors.Join(errs...)
}

var (
	nameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	groupActionRe = regexp.MustCompile(`^autoscaling:[A-Z][A-Za-z]+$`)
)

func (c Contribution) validate() error {
	var errs []error

	seen := map[string]bool{}
	check := func(kind, name string) {
		switch {
		case !nameRe.MatchString(name):
			errs = append(errs, fmt.Errorf("%s %q: want lower-case letters, digits and -", kind, name))
		case seen[kind+"/"+name]:
			errs = append(errs, fmt.Errorf("%s %q is contributed twice", kind, name))
		}

		seen[kind+"/"+name] = true
	}

	for _, s := range c.InService {
		check("in-service step", s.Name)

		if s.Script == "" {
			errs = append(errs, fmt.Errorf("in-service step %q: empty script", s.Name))
		}
	}

	for _, r := range c.ReadyChecks {
		check("ready check", r.Name)

		if r.Script == "" || r.TimeoutSeconds < 0 {
			errs = append(errs, fmt.Errorf("ready check %q: want a script and a non-negative timeout", r.Name))
		}
	}

	for _, a := range c.GroupActions {
		if !groupActionRe.MatchString(a) {
			errs = append(errs, fmt.Errorf("group action %q: want one autoscaling:Action, no wildcard", a))
		}
	}

	for _, w := range c.Watch {
		check("watch check", w.Name)

		if w.Script == "" {
			errs = append(errs, fmt.Errorf("watch check %q: empty script", w.Name))
		}
	}

	return errors.Join(errs...)
}

// sortedSteps returns the steps by (Order, registration).
func sortedSteps(in []Step) []Step {
	out := append([]Step(nil), in...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Order < out[b].Order })

	return out
}
