package front

import (
	"fmt"
	"slices"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lb"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/vpc"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// SharedNLBResource is what [NewSharedNLB] creates: the NLB and its security
// group.
type SharedNLBResource struct {
	pulumi.ResourceState

	// LoadBalancer is the NLB.
	LoadBalancer *lb.LoadBalancer
	// SecurityGroup is the NLB's group. It has no ingress rule: each box adds
	// the rule for its own port.
	SecurityGroup *ec2.SecurityGroup
	// Egress is the one egress rule, to the VPC.
	Egress *vpc.SecurityGroupEgressRule

	// ARN, DNSName, ZoneID, SecurityGroupID, VPCID and VPCCIDR are the
	// values a box's stack needs; [SharedNLBResource.Data] bundles them.
	ARN, DNSName, ZoneID, SecurityGroupID, VPCID, VPCCIDR pulumi.StringOutput
}

// Data resolves to a [SharedNLB], the plain data a stack writes to the
// generated file the boxes' stacks read. The value is a [SharedNLB] behind an
// any: an output of a type the SDK does not know cannot be typed.
func (r *SharedNLBResource) Data() pulumi.AnyOutput {
	return pulumi.All(r.ARN, r.DNSName, r.ZoneID, r.SecurityGroupID, r.VPCID, r.VPCCIDR).ApplyT(func(v []any) any {
		return SharedNLB{
			ARN: v[0].(string), DNSName: v[1].(string), ZoneID: v[2].(string),
			SecurityGroupID: v[3].(string), VPCID: v[4].(string), VPCCIDR: v[5].(string),
		}
	}).(pulumi.AnyOutput)
}

// NewSharedNLB creates the box front's own resources: an internal network
// load balancer with cross-zone balancing and deletion protection on (unless
// told otherwise), and its security group with egress to the VPC and no
// ingress.
//
// The security group is part of the NLB from the start. AWS does not allow
// adding one to an NLB created without, and an NLB without a group cannot
// admit by security group.
func NewSharedNLB(ctx *pulumi.Context, name string, args *SharedNLBArgs, opts ...pulumi.ResourceOption) (*SharedNLBResource, error) {
	if args == nil {
		return nil, fmt.Errorf("front: NewSharedNLB(%q, ...): args is nil", name)
	}

	a := args.withDefaults(name)
	if err := a.validate().err(fmt.Sprintf("front: NewSharedNLB(%q, ...)", name)); err != nil {
		return nil, err
	}

	out := &SharedNLBResource{}
	if err := ctx.RegisterComponentResource(TypeSharedNLB, name, out, opts...); err != nil {
		return nil, err
	}

	copts := append([]pulumi.ResourceOption{pulumi.Parent(out)}, opts...)

	parts, err := newParts(ctx, name, a, copts)
	if err != nil {
		return nil, fmt.Errorf("front: NewSharedNLB(%q, ...): %w", name, err)
	}

	out.LoadBalancer, out.SecurityGroup, out.Egress = parts.lb, parts.sg, parts.egress
	out.ARN, out.DNSName, out.ZoneID = parts.lb.Arn, parts.lb.DnsName, parts.lb.ZoneId
	out.SecurityGroupID = parts.sg.ID().ToStringOutput()
	out.VPCID, out.VPCCIDR = a.VPCID.ToStringOutput(), a.VPCCIDR.ToStringOutput()

	if err := ctx.RegisterResourceOutputs(out, pulumi.Map{
		"arn": out.ARN, "dnsName": out.DNSName, "zoneId": out.ZoneID, "securityGroupId": out.SecurityGroupID,
	}); err != nil {
		return nil, err
	}

	return out, nil
}

type parts struct {
	lb     *lb.LoadBalancer
	sg     *ec2.SecurityGroup
	egress *vpc.SecurityGroupEgressRule
}

// newParts creates the NLB, its security group and the group's egress rule.
// The options already carry the parent.
func newParts(ctx *pulumi.Context, name string, a SharedNLBArgs, opts []pulumi.ResourceOption) (*parts, error) {
	tags := func(n string) pulumi.StringMap {
		m := pulumi.StringMap{"Name": pulumi.String(n)}
		for k, v := range a.Tags {
			m[k] = pulumi.String(v)
		}

		return m
	}

	sg, err := ec2.NewSecurityGroup(ctx, name, &ec2.SecurityGroupArgs{
		// A fixed name and a fixed description: the group is referenced by id
		// from other stacks, and a change that replaces it would break each
		// rule they hold.
		Name:        pulumi.String(a.Name),
		Description: pulumi.String("Box front: admits clients per port"),
		VpcId:       a.VPCID,
		Tags:        tags(a.Name),
	}, slices.Clone(opts)...)
	if err != nil {
		return nil, err
	}

	// Separate rules, never inline ones: boxes in other stacks add rules to
	// this group, and inline rules would delete them at the next update.
	egress, err := vpc.NewSecurityGroupEgressRule(ctx, name+"-egress", &vpc.SecurityGroupEgressRuleArgs{
		SecurityGroupId: sg.ID(),
		Description:     pulumi.String("Targets and health checks inside the VPC"),
		IpProtocol:      pulumi.String("-1"),
		CidrIpv4:        a.VPCCIDR,
		Tags:            tags(a.Name + "-egress"),
	}, slices.Clone(opts)...)
	if err != nil {
		return nil, err
	}

	subnets := make(pulumi.StringArray, len(a.SubnetIDs))
	for i, s := range a.SubnetIDs {
		subnets[i] = s.ToStringOutput()
	}

	nlb, err := lb.NewLoadBalancer(ctx, name, &lb.LoadBalancerArgs{
		Name:                         pulumi.String(a.Name),
		LoadBalancerType:             pulumi.String("network"),
		Internal:                     pulumi.Bool(true),
		IpAddressType:                pulumi.String("ipv4"),
		Subnets:                      subnets,
		SecurityGroups:               pulumi.StringArray{sg.ID().ToStringOutput()},
		EnableCrossZoneLoadBalancing: pulumi.Bool(*a.CrossZone),
		EnableDeletionProtection:     pulumi.Bool(*a.DeletionProtection),
		Tags:                         tags(a.Name),
	}, slices.Clone(opts)...)
	if err != nil {
		return nil, err
	}

	return &parts{lb: nlb, sg: sg, egress: egress}, nil
}
