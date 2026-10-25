package front

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// nameRe is the shape of a load balancer or target group name: letters,
// digits and hyphens, no hyphen at either end, at most 32 characters.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,30}[A-Za-z0-9])?$`)

// problems collects messages and joins them.
type problems struct{ list []error }

func (p *problems) add(format string, a ...any) { p.list = append(p.list, fmt.Errorf(format, a...)) }

func (p *problems) err(prefix string) error {
	if len(p.list) == 0 {
		return nil
	}

	return fmt.Errorf("%s: %w", prefix, errors.Join(p.list...))
}

func (a *SharedNLBArgs) withDefaults(name string) SharedNLBArgs {
	out := *a
	if out.Name == "" {
		out.Name = name
	}

	if out.CrossZone == nil {
		t := true
		out.CrossZone = &t
	}

	if out.DeletionProtection == nil {
		t := true
		out.DeletionProtection = &t
	}

	return out
}

func (a *SharedNLBArgs) validate() *problems {
	p := &problems{}

	if !nameRe.MatchString(a.Name) || strings.HasPrefix(a.Name, "internal-") {
		p.add("Name %q: want 1..32 letters, digits and hyphens, no hyphen at either end and not starting with internal-", a.Name)
	}

	if a.VPCID == nil {
		p.add("VPCID is required")
	}

	if a.VPCCIDR == nil {
		p.add("VPCCIDR is required: the security group's egress is limited to it")
	}

	if len(a.SubnetIDs) == 0 {
		p.add("SubnetIDs is empty: an NLB needs a subnet (one per zone)")
	}

	for i, s := range a.SubnetIDs {
		if s == nil {
			p.add("SubnetIDs[%d] is nil", i)
		}
	}

	return p
}

// validate checks a plain SharedNLB read from a file; the messages say what
// is missing, because the usual cause is a file the NLB's stack has not
// written yet.
func (n SharedNLB) validate() *problems {
	p := &problems{}

	for _, f := range []struct{ name, v string }{
		{"arn", n.ARN}, {"dns-name", n.DNSName}, {"zone-id", n.ZoneID},
		{"security-group-id", n.SecurityGroupID}, {"vpc-id", n.VPCID}, {"vpc-cidr", n.VPCCIDR},
	} {
		if f.v == "" {
			p.add("the shared NLB has no %s: has the box-front stack of this environment been applied and its generated file committed?", f.name)
		}
	}

	return p
}

// withDefaults returns the listener with its defaults applied.
func (l Listener) withDefaults() Listener {
	if l.Protocol == "" {
		l.Protocol = TCP
	}

	if l.TargetPort == 0 {
		l.TargetPort = l.Port
	}

	if l.HealthCheck.Protocol == "" {
		l.HealthCheck.Protocol = "TCP"
	}

	if l.HealthCheck.IntervalSeconds == 0 {
		l.HealthCheck.IntervalSeconds = DefaultHealthCheckInterval
	}

	if l.HealthCheck.Threshold == 0 {
		l.HealthCheck.Threshold = DefaultHealthCheckThreshold
	}

	return l
}

func validPort(n int) bool { return n >= 1 && n <= 65535 }

func (l Listener) validate(p *problems) {
	at := fmt.Sprintf("Listeners[%q]", l.Name)

	if !nameRe.MatchString(l.Name) {
		p.add("%s: Name: want 1..32 letters, digits and hyphens, no hyphen at either end", at)
	}

	if !validPort(l.Port) {
		p.add("%s: Port %d: want 1..65535", at, l.Port)
	}

	if !validPort(l.TargetPort) {
		p.add("%s: TargetPort %d: want 1..65535", at, l.TargetPort)
	}

	switch l.Protocol {
	case TCP, UDP, TCPUDP:
	case "TLS":
		p.add("%s: Protocol TLS is refused: a TLS listener terminates the client's TLS and breaks a box that checks client certificates; use TCP", at)
	default:
		p.add("%s: Protocol %q: want TCP, UDP or TCP_UDP", at, l.Protocol)
	}

	if l.IdleTimeoutSeconds != 0 {
		if l.Protocol != TCP {
			p.add("%s: IdleTimeoutSeconds is a TCP listener attribute", at)
		}

		if l.IdleTimeoutSeconds < 60 || l.IdleTimeoutSeconds > 6000 {
			p.add("%s: IdleTimeoutSeconds %d: want 60..6000", at, l.IdleTimeoutSeconds)
		}
	}

	if l.DeregistrationDelaySeconds < 0 || l.DeregistrationDelaySeconds > 3600 {
		p.add("%s: DeregistrationDelaySeconds %d: want 0..3600", at, l.DeregistrationDelaySeconds)
	}

	h := l.HealthCheck

	switch h.Protocol {
	case "TCP":
		if h.Path != "" {
			p.add("%s: HealthCheck.Path is set but the protocol is TCP", at)
		}
	case "HTTP", "HTTPS":
		if h.Path == "" || !strings.HasPrefix(h.Path, "/") {
			p.add("%s: HealthCheck.Path %q: want an absolute path for %s", at, h.Path, h.Protocol)
		}
	default:
		p.add("%s: HealthCheck.Protocol %q: want TCP, HTTP or HTTPS", at, h.Protocol)
	}

	if h.Port != 0 && !validPort(h.Port) {
		p.add("%s: HealthCheck.Port %d: want 1..65535 or 0 for the target port", at, h.Port)
	}

	if l.Protocol == UDP && h.Port == 0 {
		p.add("%s: a UDP listener needs HealthCheck.Port: the check cannot be UDP, so name the TCP port that answers", at)
	}

	if h.IntervalSeconds != 10 && h.IntervalSeconds != 30 {
		p.add("%s: HealthCheck.IntervalSeconds %d: an NLB takes 10 or 30", at, h.IntervalSeconds)
	}

	if h.Threshold < 2 || h.Threshold > 10 {
		p.add("%s: HealthCheck.Threshold %d: want 2..10", at, h.Threshold)
	}

	if len(l.Sources) == 0 {
		p.add("%s: Sources is empty: nobody could connect; name the security group that may", at)
	}

	for i, s := range l.Sources {
		sat := fmt.Sprintf("%s: Sources[%d]", at, i)

		switch {
		case s.SecurityGroupID != nil && s.CIDR != "":
			p.add("%s: set SecurityGroupID or CIDR, not both", sat)
		case s.SecurityGroupID == nil && s.CIDR == "":
			p.add("%s: set SecurityGroupID or CIDR", sat)
		case s.CIDR != "":
			pre, err := netip.ParsePrefix(s.CIDR)

			switch {
			case err != nil:
				p.add("%s: CIDR %q is not a prefix", sat, s.CIDR)
			case pre.Bits() == 0 && !s.World:
				p.add("%s: CIDR %s opens the port to the world; set World to say so", sat, s.CIDR)
			}
		}
	}
}

// validateListeners checks the listeners one call creates against each other:
// names and ports are unique, and the total is under the quota.
func validateListeners(ls []Listener) error {
	p := &problems{}
	names := map[string]bool{}
	ports := map[string]bool{}

	if len(ls) == 0 {
		p.add("no listeners")
	}

	if len(ls) > MaxListeners {
		p.add("%d listeners: an NLB takes at most %d", len(ls), MaxListeners)
	}

	for i := range ls {
		l := ls[i]
		l.validate(p)

		if names[l.Name] {
			p.add("Listeners: name %q is used twice", l.Name)
		}

		names[l.Name] = true

		for _, proto := range protocols(l.Protocol) {
			k := fmt.Sprintf("%s/%d", proto, l.Port)
			if ports[k] {
				p.add("Listeners: port %s is used twice (%q): the port is the service's identity", k, l.Name)
			}

			ports[k] = true
		}
	}

	return p.err("front")
}

// protocols are the IP protocols a listener protocol stands for.
func protocols(p string) []string {
	switch p {
	case UDP:
		return []string{"udp"}
	case TCPUDP:
		return []string{"tcp", "udp"}
	}

	return []string{"tcp"}
}
