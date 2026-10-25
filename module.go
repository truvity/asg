package asg

import (
	"errors"
	"fmt"
	"sort"

	"github.com/truvity/asg/bootstrap"
)

// Module is an optional piece of an appliance that touches several layers at
// once: the user-data (sshd with user certificates, self-registering DNS,
// journald, swap), the role and the network. A module returns plain data; it
// never creates a resource, so the logical names of the appliance are the
// same with and without it.
type Module interface {
	// Name identifies the module in errors.
	Name() string
	// Contribute returns what the module adds.
	Contribute() (Contribution, error)
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
	}

	return out, errors.Join(errs...)
}

// AddTo adds the fragments to the plan and records the SSM parameters, so
// call it before rendering.
func (c Contribution) AddTo(p *bootstrap.Plan) error {
	return errors.Join(p.Add(c.Fragments...), p.AddSSMParameters(c.SSMParameters...))
}

// sortedUnique returns the distinct strings, sorted.
func sortedUnique(lists ...[]string) []string {
	set := map[string]bool{}

	for _, l := range lists {
		for _, s := range l {
			set[s] = true
		}
	}

	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}

	sort.Strings(out)

	return out
}
