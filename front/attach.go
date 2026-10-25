package front

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lb"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/vpc"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
)

// Front is the box's view of its NLB, shared or dedicated: its target groups,
// listeners and rules, and what the box's own resources take from them.
type Front struct {
	pulumi.ResourceState

	// TargetGroups, Listeners and Rules are in the order of the listeners
	// the call was given. Rules are the ingress rules on the NLB's security
	// group.
	TargetGroups []*lb.TargetGroup
	Listeners    []*lb.Listener
	Rules        []*vpc.SecurityGroupIngressRule

	// LoadBalancer and SecurityGroup are set by [NewNLB] only: an attached
	// box does not own the NLB.
	LoadBalancer  *lb.LoadBalancer
	SecurityGroup *ec2.SecurityGroup

	// DNSName and ZoneID are the NLB's, for the record the estate's DNS
	// stack writes. SecurityGroupID is the NLB's group.
	DNSName, ZoneID, SecurityGroupID pulumi.StringOutput

	specs []Listener
}

// Attach creates a box's part of the shared NLB: for each listener a target
// group, a listener and, on the NLB's security group, one ingress rule per
// source scoped to the listener's port.
//
// The NLB is the plain data of the file its stack wrote, so a missing file is
// an error here, at preview, with a message saying what to apply.
func Attach(ctx *pulumi.Context, name string, nlb SharedNLB, listeners []Listener, opts ...pulumi.ResourceOption) (*Front, error) {
	if err := nlb.validate().err(fmt.Sprintf("front: Attach(%q, ...)", name)); err != nil {
		return nil, err
	}

	ls := make([]Listener, len(listeners))
	for i := range listeners {
		ls[i] = listeners[i].withDefaults()
	}

	if err := validateListeners(ls); err != nil {
		return nil, fmt.Errorf("front: Attach(%q, ...): %w", name, err)
	}

	f := &Front{}
	if err := ctx.RegisterComponentResource(TypeFront, name, f, opts...); err != nil {
		return nil, err
	}

	copts := append([]pulumi.ResourceOption{pulumi.Parent(f)}, opts...)

	ref := ref{
		arn: pulumi.String(nlb.ARN).ToStringOutput(), sg: pulumi.String(nlb.SecurityGroupID).ToStringOutput(),
		vpc: pulumi.String(nlb.VPCID).ToStringOutput(),
	}
	f.DNSName, f.ZoneID = pulumi.String(nlb.DNSName).ToStringOutput(), pulumi.String(nlb.ZoneID).ToStringOutput()
	f.SecurityGroupID = ref.sg

	if err := f.attach(ctx, name, ref, ls, copts); err != nil {
		return nil, fmt.Errorf("front: Attach(%q, ...): %w", name, err)
	}

	if err := ctx.RegisterResourceOutputs(f, pulumi.Map{"dnsName": f.DNSName, "zoneId": f.ZoneID}); err != nil {
		return nil, err
	}

	return f, nil
}

// NewNLB creates a dedicated NLB for one box, with its security group and the
// box's listeners, and returns the same [Front] that [Attach] does.
func NewNLB(ctx *pulumi.Context, name string, args *NLBArgs, opts ...pulumi.ResourceOption) (*Front, error) {
	if args == nil {
		return nil, fmt.Errorf("front: NewNLB(%q, ...): args is nil", name)
	}

	a := args.withDefaults(name)
	ls := make([]Listener, len(args.Listeners))

	for i := range args.Listeners {
		ls[i] = args.Listeners[i].withDefaults()
	}

	p := a.validate()
	if err := validateListeners(ls); err != nil {
		p.list = append(p.list, err)
	}

	if err := p.err(fmt.Sprintf("front: NewNLB(%q, ...)", name)); err != nil {
		return nil, err
	}

	f := &Front{}
	if err := ctx.RegisterComponentResource(TypeFront, name, f, opts...); err != nil {
		return nil, err
	}

	copts := append([]pulumi.ResourceOption{pulumi.Parent(f)}, opts...)

	built, err := newParts(ctx, name, a, copts)
	if err != nil {
		return nil, fmt.Errorf("front: NewNLB(%q, ...): %w", name, err)
	}

	f.LoadBalancer, f.SecurityGroup = built.lb, built.sg
	f.DNSName, f.ZoneID = built.lb.DnsName, built.lb.ZoneId
	f.SecurityGroupID = built.sg.ID().ToStringOutput()

	r := ref{arn: built.lb.Arn, sg: f.SecurityGroupID, vpc: a.VPCID.ToStringOutput()}
	if err := f.attach(ctx, name, r, ls, copts); err != nil {
		return nil, fmt.Errorf("front: NewNLB(%q, ...): %w", name, err)
	}

	if err := ctx.RegisterResourceOutputs(f, pulumi.Map{"dnsName": f.DNSName, "zoneId": f.ZoneID}); err != nil {
		return nil, err
	}

	return f, nil
}

// ref is the NLB as the attach step sees it, whoever created it.
type ref struct {
	arn, sg, vpc pulumi.StringOutput
}

func (f *Front) attach(ctx *pulumi.Context, name string, nlb ref, ls []Listener, opts []pulumi.ResourceOption) error {
	f.specs = ls

	for i := range ls {
		l := ls[i]

		tg, err := targetGroup(ctx, name, nlb, l, opts)
		if err != nil {
			return err
		}

		lst, err := listener(ctx, name, nlb, l, tg, opts)
		if err != nil {
			return err
		}

		f.TargetGroups = append(f.TargetGroups, tg)
		f.Listeners = append(f.Listeners, lst)

		for i, s := range l.Sources {
			for _, proto := range protocols(l.Protocol) {
				args := &vpc.SecurityGroupIngressRuleArgs{
					SecurityGroupId: nlb.sg,
					Description:     pulumi.String(sourceDescription(l, s)),
					IpProtocol:      pulumi.String(proto),
					FromPort:        pulumi.Int(l.Port),
					ToPort:          pulumi.Int(l.Port),
				}

				if s.SecurityGroupID != nil {
					args.ReferencedSecurityGroupId = s.SecurityGroupID.ToStringOutput()
				} else {
					args.CidrIpv4 = pulumi.String(s.CIDR)
				}

				r, err := vpc.NewSecurityGroupIngressRule(ctx, fmt.Sprintf("%s-%s-in-%d-%s", name, l.Name, i, proto), args, slices.Clone(opts)...)
				if err != nil {
					return err
				}

				f.Rules = append(f.Rules, r)
			}
		}
	}

	return nil
}

func sourceDescription(l Listener, s Source) string {
	if s.Description != "" {
		return s.Description
	}

	return "Clients of " + l.Name
}

func healthPort(l Listener) string {
	if l.HealthCheck.Port == 0 {
		return "traffic-port"
	}

	return strconv.Itoa(l.HealthCheck.Port)
}

func targetGroup(ctx *pulumi.Context, name string, nlb ref, l Listener, opts []pulumi.ResourceOption) (*lb.TargetGroup, error) {
	hc := &lb.TargetGroupHealthCheckArgs{
		Enabled:            pulumi.Bool(true),
		Protocol:           pulumi.String(l.HealthCheck.Protocol),
		Port:               pulumi.String(healthPort(l)),
		Interval:           pulumi.Int(l.HealthCheck.IntervalSeconds),
		HealthyThreshold:   pulumi.Int(l.HealthCheck.Threshold),
		UnhealthyThreshold: pulumi.Int(l.HealthCheck.Threshold),
	}
	if l.HealthCheck.Path != "" {
		hc.Path = pulumi.String(l.HealthCheck.Path)
	}

	args := &lb.TargetGroupArgs{
		// No fixed name: a target group is replaced when its port, protocol
		// or VPC changes, and a replacement created beside the old one cannot
		// reuse its name. The Name tag carries the listener's name.
		TargetType:  pulumi.String("instance"),
		Protocol:    pulumi.String(l.Protocol),
		Port:        pulumi.Int(l.TargetPort),
		VpcId:       nlb.vpc,
		HealthCheck: hc,
		Tags:        pulumi.StringMap{"Name": pulumi.String(l.Name)},
	}

	if l.DeregistrationDelaySeconds != 0 {
		args.DeregistrationDelay = pulumi.Int(l.DeregistrationDelaySeconds)
	}

	if l.TerminateOnDeregistration {
		args.ConnectionTermination = pulumi.Bool(true)
	}

	if l.PreserveClientIP != nil {
		args.PreserveClientIp = pulumi.String(strconv.FormatBool(*l.PreserveClientIP))
	}

	return lb.NewTargetGroup(ctx, name+"-"+l.Name, args, slices.Clone(opts)...)
}

func listener(ctx *pulumi.Context, name string, nlb ref, l Listener, tg *lb.TargetGroup, opts []pulumi.ResourceOption) (*lb.Listener, error) {
	args := &lb.ListenerArgs{
		LoadBalancerArn: nlb.arn,
		Port:            pulumi.Int(l.Port),
		Protocol:        pulumi.String(l.Protocol),
		DefaultActions: lb.ListenerDefaultActionArray{
			lb.ListenerDefaultActionArgs{Type: pulumi.String("forward"), TargetGroupArn: tg.Arn},
		},
		Tags: pulumi.StringMap{"Name": pulumi.String(l.Name)},
	}

	if l.IdleTimeoutSeconds != 0 {
		args.TcpIdleTimeoutSeconds = pulumi.Int(l.IdleTimeoutSeconds)
	}

	return lb.NewListener(ctx, name+"-"+l.Name, args, slices.Clone(opts)...)
}

// TargetGroupARNs are the target groups for asg.GroupArgs.TargetGroupARNs, in
// the order of the listeners. Each value resolves only once its listener
// exists: a group registered in a target group that no listener serves is
// refused by Auto Scaling.
func (f *Front) TargetGroupARNs() []pulumi.StringInput {
	out := make([]pulumi.StringInput, len(f.TargetGroups))

	for i, tg := range f.TargetGroups {
		out[i] = pulumi.All(f.Listeners[i].Arn, tg.Arn).ApplyT(func(v []any) string { return v[1].(string) }).(pulumi.StringOutput)
	}

	return out
}

// TargetIngress are the rules for the box's own security group
// (asg.SecurityArgs.Ingress): each target port, and each health check port
// that differs, from the NLB's security group and from no one else. This is
// what admits traffic through the NLB when client IP preservation makes
// packets carry the client's address; a client cannot reach the box around
// the NLB.
func (f *Front) TargetIngress() []asg.Rule {
	var rules []asg.Rule

	seen := map[string]bool{}

	add := func(l Listener, proto string, port int, what string) {
		k := proto + "/" + strconv.Itoa(port)
		if seen[k] {
			return
		}

		seen[k] = true

		rules = append(rules, asg.Rule{
			Description:  what + " of " + l.Name + " from the box front",
			Protocol:     proto,
			FromPort:     port,
			ToPort:       port,
			SourceGroups: []pulumi.StringInput{f.SecurityGroupID},
		})
	}

	for i := range f.specs {
		l := f.specs[i]

		for _, proto := range protocols(l.Protocol) {
			add(l, proto, l.TargetPort, "Traffic")
		}

		// The health check is always TCP, HTTP or HTTPS, so its rule is TCP;
		// a port that is the target port already has one.
		if hp := l.HealthCheck.Port; hp != 0 {
			add(l, "tcp", hp, "Health check")
		}
	}

	return rules
}
