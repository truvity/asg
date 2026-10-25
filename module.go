package asg

import (
	"sort"

	"github.com/truvity/asg/module"
)

// Module, Contribution and Collect are the extension mechanism, defined in the
// Pulumi-free package [module]. They are re-exported here so a caller needs
// one import.
type (
	// Module is an optional piece of an appliance; see [module.Module].
	Module = module.Module
	// Contribution is what modules add; see [module.Contribution].
	Contribution = module.Contribution
	// Step is an in-service step; see [module.Step].
	Step = module.Step
	// ReadyCheck gates the launch hook; see [module.ReadyCheck].
	ReadyCheck = module.ReadyCheck
	// WatchCheck is a watchdog check; see [module.WatchCheck].
	WatchCheck = module.WatchCheck
)

// Collect merges the modules' contributions in the order given.
func Collect(mods ...Module) (Contribution, error) { return module.Collect(mods...) }

// ingressRules converts the modules' ingress rules to the appliance's.
func ingressRules(in []module.Rule) []Rule {
	out := make([]Rule, len(in))

	for i := range in {
		r := &in[i]
		out[i] = Rule{
			Description: r.Description, Protocol: r.Protocol, FromPort: r.FromPort, ToPort: r.ToPort,
			CIDRs: r.CIDRs, IPv6CIDRs: r.IPv6CIDRs, World: r.World,
		}

		for _, g := range r.SourceGroupIDs {
			out[i].SourceGroups = append(out[i].SourceGroups, pulumiString(g))
		}
	}

	return out
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
