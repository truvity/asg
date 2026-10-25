package asg

import "github.com/truvity/asg/bootstrap"

// Resolved are the physical names after defaults.
type Resolved struct {
	Base            string
	Group           string
	LaunchTemplate  string
	Role            string
	InstanceProfile string
	Hook            string
	// SecurityGroup is the fixed name, or empty.
	SecurityGroup string
	// SecurityGroupPrefix is the generated-name prefix when SecurityGroup is
	// empty.
	SecurityGroupPrefix string
}

// Resolve applies the defaults for the logical name. A caller that renders
// the plan before calling [New] needs the group and hook names for
// [bootstrap.Options].Lifecycle: use [Resolved.Lifecycle].
func (n Names) Resolve(name string) Resolved {
	pick := func(v, def string) string {
		if v != "" {
			return v
		}

		return def
	}

	base := pick(n.Base, name)
	r := Resolved{Base: base}
	r.Group = pick(n.Group, base)
	r.LaunchTemplate = pick(n.LaunchTemplate, base)
	r.Role = pick(n.Role, base)
	r.InstanceProfile = pick(n.InstanceProfile, r.Role)
	r.Hook = pick(n.Hook, r.Group+"-launch")
	r.SecurityGroup = n.SecurityGroup
	r.SecurityGroupPrefix = r.Group + "-"

	return r
}

// Lifecycle is what bootstrap.Options.Lifecycle needs to abandon the right
// hook of the right group.
func (r Resolved) Lifecycle() *bootstrap.Lifecycle {
	return &bootstrap.Lifecycle{ASGName: r.Group, HookName: r.Hook}
}
