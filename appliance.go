package asg

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/autoscaling"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ebs"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lb"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/s3"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ssm"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg/bootstrap"
)

// Appliance is what [New] creates. With [Compat.Flat] it is a plain holder
// and not a registered component.
type Appliance struct {
	pulumi.ResourceState

	// Names are the physical names after defaults.
	Names Resolved
	// Group is the Auto Scaling group; nil in single-instance mode.
	Group *autoscaling.Group
	// Instance is the plain EC2 instance of single-instance mode
	// (Args.Single); nil with a group.
	Instance *ec2.Instance
	// DataVolume and DataAttachment are the persistent volume of Args.Data
	// and its attachment to Instance; nil without Data.
	DataVolume     *ebs.Volume
	DataAttachment *ec2.VolumeAttachment
	// TargetAttachments register Instance in Single.TargetGroups.
	TargetAttachments []*lb.TargetGroupAttachment
	// LaunchTemplate carries the image, the user-data and IMDSv2.
	LaunchTemplate *ec2.LaunchTemplate
	// Role and InstanceProfile are the instance's identity.
	Role            *iam.Role
	InstanceProfile *iam.InstanceProfile
	// Policy is the role's main inline policy document as exact JSON: the
	// bytes the role carries, so a golden of it is a review artifact.
	Policy pulumi.StringOutput
	// ManagedPolicies are the attachments of Args.IAM.ManagedPolicyARNs.
	ManagedPolicies []*iam.RolePolicyAttachment
	// SecurityGroup is the group the appliance created; nil with
	// Security.ExistingGroups.
	SecurityGroup *ec2.SecurityGroup
	// Rules are the separate rule resources (RulesSeparate, RulesLegacy).
	Rules []pulumi.Resource
	// Hook is the separate lifecycle hook; nil for an inline hook or none.
	Hook *autoscaling.LifecycleHook
	// Objects are the uploaded S3 objects, in the order of UserData.Objects
	// then Objects.Extra.
	Objects []*s3.BucketObjectv2
	// Alarms are the CloudWatch alarms, in the order of Alarms.Alarms.
	Alarms []*cloudwatch.MetricAlarm
}

// builder carries what the steps of New share.
type builder struct {
	ctx      *pulumi.Context
	name     string
	a        Args
	r        Resolved
	out      *Appliance
	opts     []pulumi.ResourceOption
	sgIDs    []pulumi.StringInput
	objDeps  []pulumi.Resource
	region   pulumi.StringOutput
	account  pulumi.StringOutput
	contrib  Contribution
	ingress  []Rule
	logicals Logical
}

// New creates the appliance. It refuses an invalid Args, all problems at
// once, before it registers anything.
//
// The user-data is applied at launch: a change to it is a new launch template
// version, which Refresh rolls out.
func New(ctx *pulumi.Context, name string, args *Args, opts ...pulumi.ResourceOption) (*Appliance, error) {
	if args == nil {
		return nil, fmt.Errorf("appliance: New(%q, ...): args is nil", name)
	}

	a := args.withDefaults(name)
	if err := a.validate(name); err != nil {
		return nil, err
	}

	b := &builder{ctx: ctx, name: name, a: a, r: a.Names.Resolve(name), out: &Appliance{Names: a.Names.Resolve(name)}, contrib: a.Contribution}
	b.ingress = append(slices.Clone(a.Security.Ingress), ingressRules(a.Contribution.Ingress)...)
	b.logicals = b.logical()

	if a.Compat.Flat {
		b.opts = slices.Clone(opts)
	} else {
		typ := a.Compat.ComponentType
		if typ == "" {
			typ = TypeToken
		}

		copts := slices.Clone(opts)
		if len(a.Compat.ComponentAliases) > 0 {
			copts = append(copts, pulumi.Aliases(a.Compat.ComponentAliases))
		}

		if err := ctx.RegisterComponentResource(typ, name, b.out, copts...); err != nil {
			return nil, err
		}

		b.opts = append([]pulumi.ResourceOption{pulumi.Parent(b.out)}, opts...)
	}

	b.region = aws.GetRegionOutput(ctx, aws.GetRegionOutputArgs{}, b.invokeOptions()...).Region()
	b.account = aws.GetCallerIdentityOutput(ctx, aws.GetCallerIdentityOutputArgs{}, b.invokeOptions()...).AccountId()

	for _, step := range []func() error{b.iam, b.network, b.objects, b.compute, b.placement, b.alarms} {
		if err := step(); err != nil {
			return nil, fmt.Errorf("appliance: New(%q, ...): %w", name, err)
		}
	}

	if !a.Compat.Flat {
		if err := ctx.RegisterResourceOutputs(b.out, pulumi.Map{}); err != nil {
			return nil, err
		}
	}

	return b.out, nil
}

func (b *builder) invokeOptions() []pulumi.InvokeOption {
	if b.a.Provider == nil {
		return nil
	}

	return []pulumi.InvokeOption{pulumi.Provider(b.a.Provider)}
}

// child returns the options of a child of the given kind.
func (b *builder) child(kind Kind, extra ...pulumi.ResourceOption) []pulumi.ResourceOption {
	o := slices.Clone(b.opts)
	if al := b.a.Compat.Aliases[kind]; len(al) > 0 {
		o = append(o, pulumi.Aliases(al))
	}

	return append(o, extra...)
}

func (b *builder) tags(name string) pulumi.StringMap {
	m := pulumi.StringMap{"Name": pulumi.String(name), "ManagedBy": pulumi.String("pulumi")}
	for k, v := range b.a.Tags {
		m[k] = pulumi.String(v)
	}

	return m
}

// logical resolves the logical names.
func (b *builder) logical() Logical {
	l := b.a.Compat.Names
	pick := func(v *string, def string) {
		if *v == "" {
			*v = def
		}
	}

	pick(&l.Role, b.name)
	pick(&l.InstanceProfile, b.name)
	pick(&l.SecurityGroup, b.name)
	pick(&l.LaunchTemplate, b.name)
	pick(&l.Group, b.name)
	pick(&l.Hook, b.name+"-hook")
	pick(&l.Instance, b.name)
	pick(&l.DataVolume, b.name+"-data")
	pick(&l.DataAttachment, b.name+"-data-attachment")

	if l.TargetGroupAttachment == nil {
		l.TargetGroupAttachment = func(i int) string { return fmt.Sprintf("%s-target-%d", b.name, i) }
	}

	if l.ManagedPolicy == nil {
		l.ManagedPolicy = func(i int, _ string) string { return fmt.Sprintf("%s-managed-%d", b.name, i) }
	}

	if l.Ingress == nil {
		l.Ingress = func(i, j int) string { return b.ruleName("ingress", i, j) }
	}

	if l.Egress == nil {
		l.Egress = func(i, j int) string { return b.ruleName("egress", i, j) }
	}

	if l.Alarm == nil {
		l.Alarm = func(suffix string) string { return b.name + "-alarm-" + suffix }
	}

	if l.Object == nil {
		l.Object = func(o bootstrap.Object) string { return b.name + "-object-" + o.SHA256[:12] }
	}

	return l
}

func (b *builder) ruleName(dir string, i, j int) string {
	if j == 0 {
		return fmt.Sprintf("%s-%s-%d", b.name, dir, i)
	}

	return fmt.Sprintf("%s-%s-%d-%d", b.name, dir, i, j)
}

// policyContext is the callback input, for the resolved region and account.
func (b *builder) policyContext(region, account string) PolicyContext {
	return PolicyContext{
		Partition:     b.a.Partition,
		Region:        region,
		Account:       account,
		Names:         b.r,
		Bucket:        b.a.Objects.Bucket,
		SSMParameters: b.a.ssmParameters(),
		ObjectKeys:    b.a.objectKeys(),
	}
}

func (b *builder) policyOutput(f func(PolicyContext) (string, error)) pulumi.StringOutput {
	return pulumi.All(b.region, b.account).ApplyT(func(v []any) (string, error) {
		return f(b.policyContext(v[0].(string), v[1].(string)))
	}).(pulumi.StringOutput)
}

// generatedPolicy is the least-privilege document: what the plan reads, the
// group's two actions when there is a hook, and the caller's statements.
func (b *builder) generatedPolicy(pc PolicyContext) (string, error) {
	in := PolicyInput{
		Partition: pc.Partition, Region: pc.Region, Account: pc.Account,
		Bucket: pc.Bucket, SSMParameters: pc.SSMParameters, ObjectKeys: pc.ObjectKeys,
	}
	if b.a.Hook != nil {
		in.Group = b.r.Group
	}

	st := withGroupActions(BootstrapStatements(in), b.contrib.GroupActions, pc, b.r.Group)
	st = append(st, b.a.IAM.Statements...)
	st = append(st, b.contrib.Statements...)

	if f := b.a.IAM.StatementsFunc; f != nil {
		st = append(st, f(pc)...)
	}

	if len(st) == 0 {
		return "", errors.New("the generated policy has no statement; the role would be empty (set IAM.Policy to say so)")
	}

	return Document(st)
}

func (b *builder) iam() error {
	a := b.a
	main := b.generatedPolicy
	if a.IAM.Policy != nil {
		main = a.IAM.Policy
	}

	b.out.Policy = b.policyOutput(main)

	inline := iam.RoleInlinePolicyArray{iam.RoleInlinePolicyArgs{Name: pulumi.String(a.IAM.PolicyName), Policy: b.out.Policy}}
	for _, p := range a.IAM.ExtraInlinePolicies {
		inline = append(inline, iam.RoleInlinePolicyArgs{Name: pulumi.String(p.Name), Policy: b.policyOutput(p.Policy)})
	}

	trust := AssumeRolePolicy(a.Partition)
	if a.Compat.AssumeRolePolicy != "" {
		trust = a.Compat.AssumeRolePolicy
	}

	roleArgs := &iam.RoleArgs{
		Name:             pulumi.String(b.r.Role),
		AssumeRolePolicy: pulumi.String(trust),
		InlinePolicies:   inline,
		Tags:             b.tags(b.r.Role),
	}
	if a.IAM.PermissionsBoundary != "" {
		roleArgs.PermissionsBoundary = pulumi.String(a.IAM.PermissionsBoundary)
	}

	role, err := iam.NewRole(b.ctx, b.logicals.Role, roleArgs, b.child(KindRole)...)
	if err != nil {
		return fmt.Errorf("role: %w", err)
	}

	b.out.Role = role

	for i, arn := range sortedUnique(a.IAM.ManagedPolicyARNs, b.contrib.ManagedPolicyARNs) {
		att, err := iam.NewRolePolicyAttachment(b.ctx, b.logicals.ManagedPolicy(i, arn), &iam.RolePolicyAttachmentArgs{
			Role:      role.Name,
			PolicyArn: pulumi.String(arn),
		}, b.child(KindManagedPolicy)...)
		if err != nil {
			return fmt.Errorf("managed policy %s: %w", arn, err)
		}

		b.out.ManagedPolicies = append(b.out.ManagedPolicies, att)
	}

	profile, err := iam.NewInstanceProfile(b.ctx, b.logicals.InstanceProfile, &iam.InstanceProfileArgs{
		Name: pulumi.String(b.r.InstanceProfile),
		Role: role.Name,
		Tags: b.tags(b.r.InstanceProfile),
	}, b.child(KindInstanceProfile)...)
	if err != nil {
		return fmt.Errorf("instance profile: %w", err)
	}

	b.out.InstanceProfile = profile

	return nil
}

// objects uploads the user-data's objects (and the extra ones). The launch
// template and the group depend on them: no instance may launch before the
// objects it will fetch exist.
func (b *builder) objects() error {
	o := b.a.Objects
	all := append(slices.Clone(b.a.UserData.Objects), o.Extra...)

	for _, obj := range all {
		logical := b.logicals.Object(obj)
		tags := b.tags(logical)

		if o.Tags != nil {
			tags = pulumi.StringMap{}
			for k, v := range o.Tags(obj) {
				tags[k] = pulumi.String(v)
			}
		}

		args := &s3.BucketObjectv2Args{
			Bucket:      pulumi.String(o.Bucket),
			Key:         pulumi.String(obj.Key),
			ContentType: pulumi.String(o.ContentType),
			Tags:        tags,
		}
		if utf8Valid(obj.Content) {
			args.Content = pulumi.String(string(obj.Content))
		} else {
			args.ContentBase64 = pulumi.String(base64Of(obj.Content))
		}

		if o.KMSKeyARN != "" {
			args.ServerSideEncryption = pulumi.String("aws:kms")
			args.KmsKeyId = pulumi.String(o.KMSKeyARN)
		} else {
			args.ServerSideEncryption = pulumi.String("AES256")
		}

		res, err := s3.NewBucketObjectv2(b.ctx, logical, args, b.child(KindObject)...)
		if err != nil {
			return fmt.Errorf("object %s: %w", obj.Key, err)
		}

		b.out.Objects = append(b.out.Objects, res)
		b.objDeps = append(b.objDeps, res)
	}

	return nil
}

func (b *builder) imageID() pulumi.StringInput {
	im := b.a.Image

	switch {
	case im.ID != nil:
		return im.ID
	case im.SSMParameter != "":
		return ssm.LookupParameterOutput(b.ctx, ssm.LookupParameterOutputArgs{Name: pulumi.String(im.SSMParameter)}, b.invokeOptions()...).Value()
	}

	filters := ec2.GetAmiFilterArray{
		ec2.GetAmiFilterArgs{Name: pulumi.String("name"), Values: pulumi.StringArray{pulumi.String(im.NameFilter)}},
		ec2.GetAmiFilterArgs{Name: pulumi.String("state"), Values: pulumi.StringArray{pulumi.String("available")}},
	}
	if im.Architecture != "" {
		filters = append(filters, ec2.GetAmiFilterArgs{Name: pulumi.String("architecture"), Values: pulumi.StringArray{pulumi.String(im.Architecture)}})
	}

	owners := make(pulumi.StringArray, len(im.Owners))
	for i, o := range im.Owners {
		owners[i] = pulumi.String(o)
	}

	return ec2.LookupAmiOutput(b.ctx, ec2.LookupAmiOutputArgs{
		Owners:     owners,
		MostRecent: pulumi.Bool(true),
		Filters:    filters,
	}, b.invokeOptions()...).Id()
}

func (b *builder) compute() error {
	a := b.a
	deps := b.child(KindLaunchTemplate)
	if len(b.objDeps) > 0 {
		deps = append(deps, pulumi.DependsOn(slices.Clone(b.objDeps)))
	}

	sgs := make(pulumi.StringArray, len(b.sgIDs))
	copy(sgs, b.sgIDs)

	args := &ec2.LaunchTemplateArgs{
		Name:                 pulumi.String(b.r.LaunchTemplate),
		UpdateDefaultVersion: pulumi.Bool(true),
		ImageId:              b.imageID(),
		UserData:             pulumi.String(base64Of(a.UserData.UserData)),
		IamInstanceProfile:   ec2.LaunchTemplateIamInstanceProfileArgs{Arn: b.out.InstanceProfile.Arn},
		MetadataOptions: ec2.LaunchTemplateMetadataOptionsArgs{
			HttpTokens:              pulumi.String("required"),
			HttpEndpoint:            pulumi.String("enabled"),
			HttpPutResponseHopLimit: pulumi.Int(a.Instance.HopLimit),
		},
		TagSpecifications: b.tagSpecifications(),
		Tags:              b.tags(b.r.LaunchTemplate),
	}

	// One type: the launch template names it. Several: the group's mixed
	// instances policy names them and the template names none.
	if a.Instance.Type != "" {
		args.InstanceType = pulumi.String(a.Instance.Type)
	}

	var devices ec2.LaunchTemplateBlockDeviceMappingArray

	if rv := a.Instance.RootVolume; rv != nil {
		ebs := ec2.LaunchTemplateBlockDeviceMappingEbsArgs{
			VolumeSize:          pulumi.Int(rv.SizeGiB),
			VolumeType:          pulumi.String(rv.Type),
			Encrypted:           pulumi.String("true"),
			DeleteOnTermination: pulumi.String("true"),
		}
		if rv.KMSKeyID != "" {
			ebs.KmsKeyId = pulumi.String(rv.KMSKeyID)
		}

		devices = append(devices, ec2.LaunchTemplateBlockDeviceMappingArgs{DeviceName: pulumi.String(rv.DeviceName), Ebs: ebs})
	}

	for _, v := range a.Instance.Volumes {
		ebs := ec2.LaunchTemplateBlockDeviceMappingEbsArgs{
			VolumeSize:          pulumi.Int(v.SizeGiB),
			VolumeType:          pulumi.String(v.Type),
			Encrypted:           pulumi.String("true"),
			DeleteOnTermination: pulumi.String("true"),
		}
		if v.IOPS > 0 {
			ebs.Iops = pulumi.Int(v.IOPS)
		}

		if v.ThroughputMiBps > 0 {
			ebs.Throughput = pulumi.Int(v.ThroughputMiBps)
		}

		if v.KMSKeyID != "" {
			ebs.KmsKeyId = pulumi.String(v.KMSKeyID)
		}

		devices = append(devices, ec2.LaunchTemplateBlockDeviceMappingArgs{DeviceName: pulumi.String(v.DeviceName), Ebs: ebs})
	}

	if len(devices) > 0 {
		args.BlockDeviceMappings = devices
	}

	if a.Instance.PublicIP != nil {
		args.NetworkInterfaces = ec2.LaunchTemplateNetworkInterfaceArray{
			ec2.LaunchTemplateNetworkInterfaceArgs{
				AssociatePublicIpAddress: pulumi.String(fmt.Sprint(*a.Instance.PublicIP)),
				DeviceIndex:              pulumi.Int(0),
				SecurityGroups:           sgs,
			},
		}
	} else {
		args.VpcSecurityGroupIds = sgs
	}

	lt, err := ec2.NewLaunchTemplate(b.ctx, b.logicals.LaunchTemplate, args, deps...)
	if err != nil {
		return fmt.Errorf("launch template: %w", err)
	}

	b.out.LaunchTemplate = lt

	return nil
}

// defaultLaunchTemplateTagTypes are the resource types the launch template
// tags at launch unless Compat.LaunchTemplateTagTypes names others.
var defaultLaunchTemplateTagTypes = []string{"instance", "volume"}

// tagSpecifications tags what an instance launch creates with the group's
// tags.
func (b *builder) tagSpecifications() ec2.LaunchTemplateTagSpecificationArray {
	types := b.a.Compat.LaunchTemplateTagTypes
	if len(types) == 0 {
		types = defaultLaunchTemplateTagTypes
	}

	out := make(ec2.LaunchTemplateTagSpecificationArray, len(types))
	for i, t := range types {
		out[i] = ec2.LaunchTemplateTagSpecificationArgs{ResourceType: pulumi.String(t), Tags: b.tags(b.r.Group)}
	}

	return out
}

func (b *builder) groupTags() autoscaling.GroupTagArray {
	m := map[string]string{"Name": b.r.Group, "ManagedBy": "pulumi"}
	if !b.a.Compat.GroupTagsExcludeCommon {
		for k, v := range b.a.Tags {
			m[k] = v
		}
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "Name" && k != "ManagedBy" {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	out := autoscaling.GroupTagArray{}
	for _, k := range append([]string{"Name", "ManagedBy"}, keys...) {
		out = append(out, autoscaling.GroupTagArgs{Key: pulumi.String(k), Value: pulumi.String(m[k]), PropagateAtLaunch: pulumi.Bool(true)})
	}

	return out
}

// placement puts the launch template to work: an Auto Scaling group, or in
// single-instance mode one plain instance.
func (b *builder) placement() error {
	if b.a.Single != nil {
		return b.single()
	}

	return b.group()
}

func (b *builder) group() error {
	a := b.a
	g := a.Group

	subnets := make(pulumi.StringArray, len(g.SubnetIDs))
	for i, s := range g.SubnetIDs {
		subnets[i] = s.ToStringOutput()
	}

	version := b.out.LaunchTemplate.LatestVersion.ApplyT(func(v int) string { return fmt.Sprint(v) }).(pulumi.StringOutput)

	args := &autoscaling.GroupArgs{
		Name:                   pulumi.String(b.r.Group),
		MinSize:                pulumi.Int(g.Min),
		MaxSize:                pulumi.Int(g.Max),
		DesiredCapacity:        pulumi.Int(g.Desired),
		VpcZoneIdentifiers:     subnets,
		HealthCheckType:        pulumi.String(g.HealthCheckType),
		HealthCheckGracePeriod: pulumi.Int(g.GracePeriodSeconds),
		Tags:                   b.groupTags(),
	}

	if types := a.Instance.Types; len(types) > 0 {
		overrides := make(autoscaling.GroupMixedInstancesPolicyLaunchTemplateOverrideArray, len(types))
		for i, t := range types {
			overrides[i] = autoscaling.GroupMixedInstancesPolicyLaunchTemplateOverrideArgs{InstanceType: pulumi.String(t)}
		}

		args.MixedInstancesPolicy = autoscaling.GroupMixedInstancesPolicyArgs{
			// On-demand only, in the order given.
			InstancesDistribution: autoscaling.GroupMixedInstancesPolicyInstancesDistributionArgs{
				OnDemandAllocationStrategy:          pulumi.String("prioritized"),
				OnDemandBaseCapacity:                pulumi.Int(0),
				OnDemandPercentageAboveBaseCapacity: pulumi.Int(100),
			},
			LaunchTemplate: autoscaling.GroupMixedInstancesPolicyLaunchTemplateArgs{
				LaunchTemplateSpecification: autoscaling.GroupMixedInstancesPolicyLaunchTemplateLaunchTemplateSpecificationArgs{
					LaunchTemplateId: b.out.LaunchTemplate.ID(),
					Version:          version,
				},
				Overrides: overrides,
			},
		}
	} else {
		args.LaunchTemplate = autoscaling.GroupLaunchTemplateArgs{Id: b.out.LaunchTemplate.ID(), Version: version}
	}

	if len(g.TerminationPolicies) > 0 {
		args.TerminationPolicies = toStringArray(g.TerminationPolicies)
	}

	if len(g.TargetGroupARNs) > 0 {
		tgs := make(pulumi.StringArray, len(g.TargetGroupARNs))
		for i, t := range g.TargetGroupARNs {
			tgs[i] = t.ToStringOutput()
		}

		args.TargetGroupArns = tgs
	}

	if len(g.EnabledMetrics) > 0 {
		metrics := make(autoscaling.MetricArray, len(g.EnabledMetrics))
		for i, m := range g.EnabledMetrics {
			metrics[i] = autoscaling.Metric(m)
		}

		args.EnabledMetrics = metrics
		args.MetricsGranularity = pulumi.String("1Minute")
	}

	if a.Hook != nil {
		// An instance is not InService until it completes the launch hook, which
		// may take longer than the provider's ten-minute capacity wait. A timeout
		// there fails the create before the warm pool is put, and the state then
		// records a pool that was never made. Readiness is the hook's job.
		args.WaitForCapacityTimeout = pulumi.String("0")
	}

	if h := a.Hook; h != nil && h.Mode == HookInline {
		// Declared on the group, so it exists before the first instance launches.
		args.InitialLifecycleHooks = autoscaling.GroupInitialLifecycleHookArray{
			autoscaling.GroupInitialLifecycleHookArgs{
				Name:                pulumi.String(b.r.Hook),
				LifecycleTransition: pulumi.String("autoscaling:EC2_INSTANCE_LAUNCHING"),
				HeartbeatTimeout:    pulumi.Int(h.HeartbeatTimeoutSeconds),
				DefaultResult:       pulumi.String(h.DefaultResult),
			},
		}
	}

	if w := a.WarmPool; w != nil {
		wp := autoscaling.GroupWarmPoolArgs{PoolState: pulumi.String(w.State), MinSize: pulumi.Int(w.MinSize)}
		if w.MaxGroupPreparedCapacity != nil {
			wp.MaxGroupPreparedCapacity = pulumi.Int(*w.MaxGroupPreparedCapacity)
		}

		if w.ReuseOnScaleIn {
			wp.InstanceReusePolicy = autoscaling.GroupWarmPoolInstanceReusePolicyArgs{ReuseOnScaleIn: pulumi.Bool(true)}
		}

		args.WarmPool = wp
	}

	if r := a.Refresh; r != nil {
		prefs := autoscaling.GroupInstanceRefreshPreferencesArgs{
			MinHealthyPercentage: pulumi.Int(r.MinHealthyPercentage),
			InstanceWarmup:       pulumi.String(fmt.Sprint(r.WarmupSeconds)),
		}

		if r.MaxHealthyPercentage != 0 {
			prefs.MaxHealthyPercentage = pulumi.Int(r.MaxHealthyPercentage)
		}

		if r.SkipMatching {
			prefs.SkipMatching = pulumi.Bool(true)
		}

		if r.AutoRollback {
			prefs.AutoRollback = pulumi.Bool(true)
		}

		refresh := autoscaling.GroupInstanceRefreshArgs{Strategy: pulumi.String("Rolling"), Preferences: prefs}

		if len(r.Triggers) > 0 {
			refresh.Triggers = toStringArray(r.Triggers)
		}

		args.InstanceRefresh = refresh
	}

	opts := b.child(KindGroup)
	if len(b.objDeps) > 0 {
		opts = append(opts, pulumi.DependsOn(slices.Clone(b.objDeps)))
	}

	grp, err := autoscaling.NewGroup(b.ctx, b.logicals.Group, args, opts...)
	if err != nil {
		return fmt.Errorf("group: %w", err)
	}

	b.out.Group = grp

	if h := a.Hook; h != nil && h.Mode == HookSeparate {
		hook, err := autoscaling.NewLifecycleHook(b.ctx, b.logicals.Hook, &autoscaling.LifecycleHookArgs{
			Name:                 pulumi.String(b.r.Hook),
			AutoscalingGroupName: grp.Name,
			LifecycleTransition:  pulumi.String("autoscaling:EC2_INSTANCE_LAUNCHING"),
			HeartbeatTimeout:     pulumi.Int(h.HeartbeatTimeoutSeconds),
			DefaultResult:        pulumi.String(h.DefaultResult),
		}, b.child(KindHook)...)
		if err != nil {
			return fmt.Errorf("lifecycle hook: %w", err)
		}

		b.out.Hook = hook
	}

	return nil
}

func (b *builder) alarms() error {
	al := b.a.Alarms

	for i := range al.Alarms {
		x := &al.Alarms[i]
		dims := pulumi.StringMap{}
		if len(x.Dimensions) == 0 {
			dims["AutoScalingGroupName"] = b.out.Group.Name
		}

		for k, v := range x.Dimensions {
			dims[k] = pulumi.String(v)
		}

		treat := x.TreatMissingData
		if treat == "" {
			treat = "missing"
		}

		name := x.Name
		if name == "" {
			name = b.r.Base + "-" + x.Suffix
		}

		args := &cloudwatch.MetricAlarmArgs{
			Name:               pulumi.String(name),
			AlarmDescription:   pulumi.String(x.Description),
			ComparisonOperator: pulumi.String(x.Comparison),
			EvaluationPeriods:  pulumi.Int(x.EvaluationPeriods),
			MetricName:         pulumi.String(x.MetricName),
			Namespace:          pulumi.String(x.Namespace),
			Period:             pulumi.Int(x.PeriodSeconds),
			Statistic:          pulumi.String(x.Statistic),
			Threshold:          pulumi.Float64(x.Threshold),
			TreatMissingData:   pulumi.String(treat),
			Dimensions:         dims,
			Tags:               b.tags(name),
		}
		if x.DatapointsToAlarm > 0 {
			args.DatapointsToAlarm = pulumi.Int(x.DatapointsToAlarm)
		}

		if len(al.ActionARNs) > 0 {
			args.AlarmActions = toArray(al.ActionARNs)
		}

		if len(al.OKActionARNs) > 0 {
			args.OkActions = toArray(al.OKActionARNs)
		}

		res, err := cloudwatch.NewMetricAlarm(b.ctx, b.logicals.Alarm(x.Suffix), args, b.child(KindAlarm)...)
		if err != nil {
			return fmt.Errorf("alarm %s: %w", x.Suffix, err)
		}

		b.out.Alarms = append(b.out.Alarms, res)
	}

	return nil
}

func toStringArray(in []string) pulumi.StringArray {
	out := make(pulumi.StringArray, len(in))
	for i, s := range in {
		out[i] = pulumi.String(s)
	}

	return out
}

func toArray(in []string) pulumi.Array {
	out := make(pulumi.Array, len(in))
	for i, s := range in {
		out[i] = pulumi.String(s)
	}

	return out
}
