package asg

import (
	"fmt"
	"slices"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/vpc"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func protocolOf(r *Rule) string {
	if r.Protocol == "" {
		return "tcp"
	}

	return r.Protocol
}

// network creates the security group and its rules, and collects the ids the
// launch template attaches.
func (b *builder) network() error {
	s := b.a.Security
	b.sgIDs = append(b.sgIDs, s.ExistingGroups...)

	defer func() { b.sgIDs = append(b.sgIDs, b.a.Instance.ExtraSecurityGroupIDs...) }()

	if len(s.ExistingGroups) > 0 {
		return nil
	}

	args := &ec2.SecurityGroupArgs{
		Description: pulumi.String(s.Description),
		VpcId:       s.VPCID,
	}

	tagName := b.r.Group

	if b.r.SecurityGroup != "" {
		args.Name = pulumi.String(b.r.SecurityGroup)
		tagName = b.r.SecurityGroup
	} else {
		// A generated name, never a fixed one: a change to the description
		// replaces the group, and with a fixed name Pulumi would have to delete
		// the old group first, while running and warm-pool instances still
		// hold interfaces on it (DependencyViolation). With a prefix the
		// replacement is created first and the old group is deleted last.
		args.NamePrefix = pulumi.String(b.r.SecurityGroupPrefix)
	}

	args.Tags = b.tags(tagName)

	if s.Style == RulesInline {
		args.Ingress = b.inlineIngress()
		args.Egress = b.inlineEgress()
	}

	extra := b.child(KindSecurityGroup)
	if s.DeleteTimeout != "" {
		extra = append(extra, pulumi.Timeouts(&pulumi.CustomTimeouts{Delete: s.DeleteTimeout}))
	}

	sg, err := ec2.NewSecurityGroup(b.ctx, b.logicals.SecurityGroup, args, slices.Clone(extra)...)
	if err != nil {
		return fmt.Errorf("security group: %w", err)
	}

	b.out.SecurityGroup = sg
	b.sgIDs = append(b.sgIDs, sg.ID().ToStringOutput())

	if s.Style != RulesInline {
		return b.separateRules(sg)
	}

	return nil
}

func (b *builder) egressRules() []Rule {
	if b.a.Security.Egress != nil {
		return b.a.Security.Egress
	}

	return []Rule{{Protocol: "-1", CIDRs: []string{"0.0.0.0/0"}}}
}

func cidrArray(in []string) pulumi.StringArray {
	if len(in) == 0 {
		return nil
	}

	return toStringArray(in)
}

func groupArray(in []pulumi.StringInput) pulumi.StringArray {
	if len(in) == 0 {
		return nil
	}

	out := make(pulumi.StringArray, len(in))
	for i, g := range in {
		out[i] = g.ToStringOutput()
	}

	return out
}

func (b *builder) inlineIngress() ec2.SecurityGroupIngressArray {
	var out ec2.SecurityGroupIngressArray

	for i := range b.ingress {
		r := &b.ingress[i]
		x := ec2.SecurityGroupIngressArgs{
			Protocol:       pulumi.String(protocolOf(r)),
			FromPort:       pulumi.Int(r.FromPort),
			ToPort:         pulumi.Int(r.ToPort),
			CidrBlocks:     cidrArray(r.CIDRs),
			Ipv6CidrBlocks: cidrArray(r.IPv6CIDRs),
			SecurityGroups: groupArray(r.SourceGroups),
		}
		if r.Description != "" {
			x.Description = pulumi.String(r.Description)
		}

		out = append(out, x)
	}

	return out
}

func (b *builder) inlineEgress() ec2.SecurityGroupEgressArray {
	var out ec2.SecurityGroupEgressArray

	rules := b.egressRules()
	for i := range rules {
		r := &rules[i]
		x := ec2.SecurityGroupEgressArgs{
			Protocol:       pulumi.String(protocolOf(r)),
			FromPort:       pulumi.Int(r.FromPort),
			ToPort:         pulumi.Int(r.ToPort),
			CidrBlocks:     cidrArray(r.CIDRs),
			Ipv6CidrBlocks: cidrArray(r.IPv6CIDRs),
			SecurityGroups: groupArray(r.SourceGroups),
		}
		if r.Description != "" {
			x.Description = pulumi.String(r.Description)
		}

		out = append(out, x)
	}

	return out
}

// separateRules declares the rules as resources of their own.
func (b *builder) separateRules(sg *ec2.SecurityGroup) error {
	legacy := b.a.Security.Style == RulesLegacy

	if err := b.rules(sg, "ingress", b.ingress, legacy, b.logicals.Ingress); err != nil {
		return err
	}

	return b.rules(sg, "egress", b.egressRules(), legacy, b.logicals.Egress)
}

func (b *builder) rules(sg *ec2.SecurityGroup, dir string, rules []Rule, legacy bool, name func(i, j int) string) error {
	for i := range rules {
		r := &rules[i]
		if legacy {
			args := &ec2.SecurityGroupRuleArgs{
				Type:            pulumi.String(dir),
				Protocol:        pulumi.String(protocolOf(r)),
				FromPort:        pulumi.Int(r.FromPort),
				ToPort:          pulumi.Int(r.ToPort),
				SecurityGroupId: sg.ID(),
				CidrBlocks:      cidrArray(r.CIDRs),
				Ipv6CidrBlocks:  cidrArray(r.IPv6CIDRs),
			}
			if r.Description != "" {
				args.Description = pulumi.String(r.Description)
			}

			if len(r.SourceGroups) > 1 {
				return fmt.Errorf("%s rule %d: a legacy rule names one source group; split the rule", dir, i)
			}

			if len(r.SourceGroups) == 1 {
				args.SourceSecurityGroupId = r.SourceGroups[0].ToStringOutput()
			}

			res, err := ec2.NewSecurityGroupRule(b.ctx, name(i, 0), args, b.child(KindSecurityRule)...)
			if err != nil {
				return fmt.Errorf("%s rule %d: %w", dir, i, err)
			}

			b.out.Rules = append(b.out.Rules, res)

			continue
		}

		j := 0

		add := func(cidr4, cidr6 string, src pulumi.StringInput) error {
			desc := pulumi.StringPtr(r.Description)
			if r.Description == "" {
				desc = nil
			}

			var res pulumi.Resource

			var err error

			if dir == "ingress" {
				args := &vpc.SecurityGroupIngressRuleArgs{SecurityGroupId: sg.ID(), IpProtocol: pulumi.String(protocolOf(r)), Description: desc}
				fillPorts(r, &args.FromPort, &args.ToPort)

				if cidr4 != "" {
					args.CidrIpv4 = pulumi.String(cidr4)
				}

				if cidr6 != "" {
					args.CidrIpv6 = pulumi.String(cidr6)
				}

				if src != nil {
					args.ReferencedSecurityGroupId = src.ToStringOutput()
				}

				res, err = vpc.NewSecurityGroupIngressRule(b.ctx, name(i, j), args, b.child(KindSecurityRule)...)
			} else {
				args := &vpc.SecurityGroupEgressRuleArgs{SecurityGroupId: sg.ID(), IpProtocol: pulumi.String(protocolOf(r)), Description: desc}
				fillPorts(r, &args.FromPort, &args.ToPort)

				if cidr4 != "" {
					args.CidrIpv4 = pulumi.String(cidr4)
				}

				if cidr6 != "" {
					args.CidrIpv6 = pulumi.String(cidr6)
				}

				if src != nil {
					args.ReferencedSecurityGroupId = src.ToStringOutput()
				}

				res, err = vpc.NewSecurityGroupEgressRule(b.ctx, name(i, j), args, b.child(KindSecurityRule)...)
			}

			if err != nil {
				return fmt.Errorf("%s rule %d: %w", dir, i, err)
			}

			j++

			b.out.Rules = append(b.out.Rules, res)

			return nil
		}

		for _, c := range r.CIDRs {
			if err := add(c, "", nil); err != nil {
				return err
			}
		}

		for _, c := range r.IPv6CIDRs {
			if err := add("", c, nil); err != nil {
				return err
			}
		}

		for _, g := range r.SourceGroups {
			if err := add("", "", g); err != nil {
				return err
			}
		}
	}

	return nil
}

// fillPorts sets the ports of a rule; protocol -1 takes none.
func fillPorts(r *Rule, from, to *pulumi.IntPtrInput) {
	if protocolOf(r) == "-1" {
		return
	}

	*from = pulumi.IntPtr(r.FromPort)
	*to = pulumi.IntPtr(r.ToPort)
}
