package asg

import (
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg/bootstrap"
)

// Defaults.
const (
	// TypeToken is the Pulumi type of the component unless Compat.ComponentType
	// overrides it.
	TypeToken = "truvity:asg:Box"

	DefaultPartition        = "aws"
	DefaultHeartbeatSeconds = 900
	DefaultGracePeriod      = 300
	DefaultHopLimit         = 1
	DefaultRootDevice       = "/dev/xvda"
	DefaultDataDevice       = "/dev/sdf"
	DefaultRootVolumeGiB    = 8
	DefaultVolumeType       = "gp3"
	// MaxDataVolumes bounds InstanceArgs.Volumes: device names /dev/sdf to
	// /dev/sdp.
	MaxDataVolumes     = 11
	DefaultPolicyName  = "appliance"
	DefaultContentType = "text/plain"
)

// HookMode says where the lifecycle hook is declared.
type HookMode int

const (
	// HookInline (the zero value) declares the hook on the group
	// (initial_lifecycle_hook), so it exists before the first instance
	// launches. A separate hook is created after the group and an instance
	// launched in between is not held by it.
	HookInline HookMode = iota
	// HookSeparate creates an aws.autoscaling.LifecycleHook resource.
	HookSeparate
)

// RuleStyle says how security group rules are declared.
type RuleStyle int

const (
	// RulesInline (the zero value) puts the rules in the group's ingress and
	// egress blocks.
	RulesInline RuleStyle = iota
	// RulesSeparate creates one aws.vpc.SecurityGroupIngressRule or
	// SecurityGroupEgressRule per rule and address.
	RulesSeparate
	// RulesLegacy creates one aws.ec2.SecurityGroupRule per rule, the resource
	// older programs use.
	RulesLegacy
)

type (
	// Args is New's whole input.
	Args struct {
		// Names sets the physical names. Default: all derived from the
		// logical name.
		Names Names
		// Partition is the ARN partition. Default "aws".
		Partition string
		// Tags are added to every resource that takes tags.
		Tags map[string]string
		// Provider is the AWS provider the region, account and image lookups
		// go through. The resources take the options New is called with.
		Provider *aws.Provider

		// UserData is the instance's user-data. Required: a composed plan
		// (bootstrap.Plan.Render) or bootstrap.Verbatim. Its objects are
		// uploaded and its SSM parameters and object keys are granted to the
		// role.
		UserData *bootstrap.Rendered
		// Objects says where UserData.Objects are uploaded.
		Objects ObjectsArgs

		Image    ImageArgs
		Instance InstanceArgs
		// Group configures the Auto Scaling group. Single-instance mode
		// (Single set) has no group and refuses every field of it.
		Group GroupArgs
		// Single, when set, makes the appliance one plain EC2 instance
		// created from the launch template, with no Auto Scaling group, hook,
		// warm pool or refresh. See [SingleArgs].
		Single *SingleArgs
		// Data is a persistent EBS volume that outlives the instance. It
		// needs Single. See [DataVolumeArgs].
		Data *DataVolumeArgs
		// Hook is the launch lifecycle hook; nil declares none.
		Hook *HookArgs
		// WarmPool is the warm pool; nil declares none. It needs Hook.
		WarmPool *WarmPoolArgs
		// Refresh is the group's instance refresh; nil declares none.
		Refresh *RefreshArgs

		IAM      IAMArgs
		Security SecurityArgs
		// Alarms are CloudWatch alarms on the group.
		//
		// Deprecated: monitoring is self-hosted. An alarm with no action is
		// read by nobody; export metrics from the box instead. Alarms keep
		// working and are removed no sooner than the next minor release.
		Alarms AlarmsArgs

		// Contribution is what the modules add (see Collect).
		Contribution Contribution

		// Compat keeps the names of an existing program.
		Compat Compat
	}

	// Names are the physical names. An empty field takes its default from
	// Base.
	Names struct {
		// Base defaults to the logical name.
		Base string
		// Group defaults to Base.
		Group string
		// LaunchTemplate defaults to Base.
		LaunchTemplate string
		// Role defaults to Base; the instance profile has the same name.
		Role string
		// InstanceProfile defaults to Role.
		InstanceProfile string
		// Hook defaults to Group + "-launch".
		Hook string
		// SecurityGroup, when set, is the group's fixed name. Empty: a
		// generated name with the prefix Group + "-", so a replacement is
		// created before the old group is deleted.
		SecurityGroup string
	}

	// ObjectsArgs says where the user-data's S3 objects go.
	ObjectsArgs struct {
		// Bucket is required when the user-data has objects or Extra is set.
		Bucket string
		// KMSKeyARN encrypts the objects with that key (aws:kms); empty:
		// AES256.
		KMSKeyARN string
		// ContentType defaults to "text/plain".
		ContentType string
		// Extra are objects uploaded in addition to UserData.Objects, and
		// readable by the role. A migration whose old user-data fetches
		// objects the program owns lists them here.
		Extra []bootstrap.Object
		// Tags, when set, replaces the object's tags (default: Name = the
		// resource name, ManagedBy = pulumi, Tags).
		Tags func(bootstrap.Object) map[string]string
	}

	// ImageArgs finds the AMI. Exactly one of ID, NameFilter and SSMParameter.
	ImageArgs struct {
		// ID is a literal image id (or an output).
		ID pulumi.StringInput
		// NameFilter is an AMI name pattern, such as "al2023-ami-2023.*-kernel-6.1-arm64";
		// the newest available image wins.
		NameFilter string
		// Owners of the NameFilter lookup. Default "amazon".
		Owners []string
		// Architecture narrows the NameFilter lookup (x86_64, arm64).
		Architecture string
		// SSMParameter is a public or own parameter holding the image id.
		SSMParameter string
	}

	// InstanceArgs configures the launch template.
	InstanceArgs struct {
		// Type is the instance type. Exactly one of Type and Types.
		Type string
		// Types are several instance types, in priority order: the group
		// gets a mixed instances policy (on-demand, prioritized) whose
		// overrides are these types, and the launch template names none.
		// The types must share the image's architecture. A warm pool cannot
		// be combined with a mixed instances policy (an Auto Scaling
		// limit), so Types with WarmPool is refused.
		Types []string
		// HopLimit is the IMDS response hop limit, 1..64. Default 1. IMDSv2 is
		// always required.
		HopLimit int
		// RootVolume nil leaves the image's root volume alone.
		RootVolume *RootVolumeArgs
		// Volumes are extra EBS volumes in the launch template, encrypted
		// and deleted with the instance. A warm-pool stop and start keeps
		// them; a replacement starts with empty ones. The instance formats
		// and mounts them itself (a fragment of the consumer's).
		Volumes []VolumeArgs
		// PublicIP, when set, declares a network interface that associates
		// (true) or does not associate (false) a public address; nil
		// declares no interface and the group's subnets decide.
		PublicIP *bool
		// ExtraSecurityGroupIDs are attached next to the appliance's own
		// group.
		ExtraSecurityGroupIDs []pulumi.StringInput
	}

	// RootVolumeArgs is the root EBS volume. It is encrypted.
	RootVolumeArgs struct {
		// DeviceName defaults to /dev/xvda.
		DeviceName string
		// SizeGiB defaults to 8.
		SizeGiB int
		// Type defaults to gp3.
		Type string
		// KMSKeyID, when set, encrypts with that key instead of the default.
		KMSKeyID string
	}

	// VolumeArgs is one extra EBS volume. It is encrypted and deleted with
	// the instance.
	VolumeArgs struct {
		// DeviceName is /dev/sdf to /dev/sdp (on Nitro instances the volume
		// appears as an NVMe device; AL2023's udev rules link this name to
		// it). Required.
		DeviceName string
		// SizeGiB is required, 1..16384.
		SizeGiB int
		// Type defaults to gp3.
		Type string
		// IOPS and ThroughputMiBps, 0 keeps the type's baseline (gp3: 3000
		// IOPS and 125 MiB/s). Throughput is gp3 only.
		IOPS            int
		ThroughputMiBps int
		// KMSKeyID, when set, encrypts with that key instead of the default.
		KMSKeyID string
	}

	// SingleArgs turns the appliance into one plain EC2 instance
	// (aws.ec2.Instance) created from the launch template. The launch
	// template, role, instance profile, security group, image lookup, S3
	// objects and user-data are the same as in group mode. There is no Auto
	// Scaling group, so no launch hook, warm pool, instance refresh, health
	// check or scaling; Hook, WarmPool, Refresh, Types, Alarms, the Group
	// fields and module GroupActions are refused, and the user-data must be
	// rendered without bootstrap.Options.Lifecycle (nothing would complete a
	// hook, and the role gets no autoscaling permission).
	//
	// A change to the launch template (a newer image, other user-data) is a
	// new template version, and the instance is replaced, deleted before
	// created, so a Data volume is free when the new instance attaches it.
	// The replacement comes up running.
	//
	// The run state is not managed: no instance state resource exists, so
	// something else (a proxy) may stop and start the instance, and a stopped
	// instance shows no diff. It finds the instance by the role tag, since
	// the instance id changes on replacement; the private address survives a
	// stop and start, not a replacement.
	SingleArgs struct {
		// SubnetID is the subnet of the instance. Required. With Data, the
		// subnet must be in Data.AvailabilityZone; New checks that at deploy
		// time and fails the update if not.
		SubnetID pulumi.StringInput
		// RoleTagKey and RoleTagValue are a tag on the instance (and on the
		// Data volume) that names its role, for whatever finds it later.
		// Both required.
		RoleTagKey, RoleTagValue string
		// TargetGroups register the instance in existing target groups.
		// Default none (a proxy that dials the private address needs none).
		TargetGroups []TargetGroupAttachment
	}

	// TargetGroupAttachment registers a single instance in a target group.
	TargetGroupAttachment struct {
		// ARN is the target group's ARN. Required.
		ARN pulumi.StringInput
		// Port overrides the target group's port; 0 keeps it.
		Port int
	}

	// DataVolumeArgs is a persistent EBS volume for a single instance. It is
	// created next to the instance, is protected and retained on delete, so
	// no destroy of the appliance removes it, and it is attached with a
	// separate attachment that moves to the new instance on a replacement.
	// It lives in one availability zone and is always encrypted.
	//
	// The user-data mounts it: see modules/datavolume, which formats a blank
	// volume once and mounts it on every boot. The volume id is not known when
	// the user-data is rendered, so the module finds the device by its
	// DeviceName.
	DataVolumeArgs struct {
		// AvailabilityZone is the zone of the volume and so of the instance.
		// Required.
		AvailabilityZone string
		// DeviceName is /dev/sdf to /dev/sdp. Default /dev/sdf.
		DeviceName string
		// SizeGiB is required, 1..16384.
		SizeGiB int
		// Type defaults to gp3.
		Type string
		// IOPS and ThroughputMiBps, 0 keeps the type's baseline. Throughput
		// is gp3 only.
		IOPS            int
		ThroughputMiBps int
		// KMSKeyID encrypts with that key (a key ARN); empty: the account's
		// default EBS key. The key's policy must let the principal that
		// attaches and EC2 use it.
		KMSKeyID string
	}

	// GroupArgs configures the Auto Scaling group.
	GroupArgs struct {
		// Min, Max and Desired default to 1 (Max and Desired to Min).
		Min, Max, Desired int
		// SubnetIDs are required.
		SubnetIDs []pulumi.StringInput
		// HealthCheckType defaults to EC2.
		HealthCheckType string
		// GracePeriodSeconds defaults to 300.
		GracePeriodSeconds int
		// TargetGroupARNs registers the group's instances in these target
		// groups (front.Front.TargetGroupARNs). With HealthCheckType ELB a
		// failing target health check replaces the instance.
		TargetGroupARNs []pulumi.StringInput
		// EnabledMetrics are group metrics sent to CloudWatch (an alarm on
		// GroupInServiceInstances needs it).
		EnabledMetrics []string
		// TerminationPolicies say which instance a scale-in removes, in
		// order (OldestInstance, NewestInstance, OldestLaunchTemplate,
		// ClosestToNextInstanceHour, AllocationStrategy, Default). Empty:
		// the AWS default.
		TerminationPolicies []string
	}

	// HookArgs is the EC2_INSTANCE_LAUNCHING hook.
	HookArgs struct {
		Mode HookMode
		// HeartbeatTimeoutSeconds defaults to 900, bounded 30..7200.
		HeartbeatTimeoutSeconds int
		// DefaultResult is ABANDON (the default) or CONTINUE. CONTINUE needs
		// UnsafeDefaultContinue: an instance whose scripts never answered is
		// then put in service.
		DefaultResult         string
		UnsafeDefaultContinue bool
	}

	// WarmPoolArgs is a pool of prepared instances.
	WarmPoolArgs struct {
		// State is Stopped (default), Running or Hibernated.
		State string
		// MinSize defaults to 0.
		MinSize int
		// MaxGroupPreparedCapacity nil: no cap beyond the group's maximum.
		MaxGroupPreparedCapacity *int
		// ReuseOnScaleIn returns an instance that scales in to the pool
		// (stopped, its volumes kept) instead of terminating it, so a box
		// with a local cache keeps it.
		ReuseOnScaleIn bool
	}

	// RefreshArgs is the rolling instance refresh the launch template change
	// starts. The Auto Scaling API starts a refresh on every launch template
	// version change whenever the group has a refresh block, so Triggers stays
	// empty unless the caller wants another trigger.
	RefreshArgs struct {
		MinHealthyPercentage int
		// MaxHealthyPercentage lets a refresh launch before it terminates
		// (100 to 200; 0 leaves it unset).
		MaxHealthyPercentage int
		// WarmupSeconds defaults to 300.
		WarmupSeconds int
		// SkipMatching skips instances that already match the launch template.
		SkipMatching bool
		// AutoRollback rolls the refresh back when it fails.
		AutoRollback bool
		// Triggers are extra triggers beyond the launch template. Default
		// none. Naming launch_template is redundant and makes Pulumi warn.
		Triggers []string
	}

	// SecurityArgs is the security group.
	SecurityArgs struct {
		// VPCID is required unless ExistingGroups is set.
		VPCID pulumi.StringInput
		// ExistingGroups use these groups instead of creating one; Ingress
		// and Egress are then refused.
		ExistingGroups []pulumi.StringInput
		// Description cannot change in place. Default "<Base> appliance".
		Description string
		Style       RuleStyle
		Ingress     []Rule
		// Egress nil allows all outbound traffic; an empty non-nil slice
		// allows none.
		Egress []Rule
		// DeleteTimeout bounds the provider's retry of DependencyViolation
		// when a replaced group is deleted, e.g. "30m".
		DeleteTimeout string
	}

	// Rule is one security group rule.
	Rule struct {
		Description string
		// Protocol is tcp (default), udp, icmp, icmpv6 or -1.
		Protocol string
		// FromPort and ToPort; both 0 with -1.
		FromPort, ToPort int
		CIDRs            []string
		IPv6CIDRs        []string
		SourceGroups     []pulumi.StringInput
		// World acknowledges that an ingress rule opens a port to 0.0.0.0/0
		// or ::/0. Without it such a rule is refused.
		World bool
	}

	// AlarmsArgs are CloudWatch alarms on the appliance.
	//
	// Deprecated: see [Args].Alarms.
	AlarmsArgs struct {
		// ActionARNs are notified when an alarm fires (e.g. an SNS topic).
		// Empty: the alarms notify nobody.
		ActionARNs   []string
		OKActionARNs []string
		Alarms       []Alarm
	}

	// Alarm is one metric alarm. Dimensions default to the group.
	//
	// Deprecated: see [Args].Alarms.
	Alarm struct {
		// Suffix names the alarm: "<Base>-<Suffix>", and its logical name.
		Suffix string
		// Name, when set, is the alarm's physical name instead.
		Name        string
		Description string
		Namespace   string
		MetricName  string
		Statistic   string
		// Comparison is GreaterThanThreshold, GreaterThanOrEqualToThreshold,
		// LessThanThreshold or LessThanOrEqualToThreshold.
		Comparison        string
		Threshold         float64
		PeriodSeconds     int
		EvaluationPeriods int
		DatapointsToAlarm int
		// TreatMissingData defaults to missing.
		TreatMissingData string
		Dimensions       map[string]string
	}

	// IAMArgs is the instance role.
	IAMArgs struct {
		// PermissionsBoundary is the full ARN of the boundary policy.
		PermissionsBoundary string
		// ManagedPolicyARNs are attached to the role. AmazonSSMManagedInstanceCore
		// lets the role read every parameter of the account; attach it only
		// for Session Manager.
		ManagedPolicyARNs []string
		// PolicyName is the inline policy's name. Default "appliance".
		PolicyName string
		// SSMParameters and ObjectKeys are granted in addition to the
		// user-data's.
		SSMParameters []string
		ObjectKeys    []string
		// Statements are added to the generated policy.
		Statements []Statement
		// StatementsFunc is like Statements for statements that need the
		// region, account or names.
		StatementsFunc func(PolicyContext) []Statement
		// Policy replaces the generated policy (the migration path of step
		// A). It excludes Statements, StatementsFunc, SSMParameters,
		// ObjectKeys, the user-data's grants and the modules'.
		Policy func(PolicyContext) (string, error)
		// ExtraInlinePolicies are further inline policies of the role.
		ExtraInlinePolicies []InlinePolicy
	}

	// InlinePolicy is a named inline policy document.
	InlinePolicy struct {
		Name   string
		Policy func(PolicyContext) (string, error)
	}

	// Compat keeps an existing program's resources where they are.
	Compat struct {
		// ComponentType overrides the component's Pulumi type, so a program
		// that already has a component of its own type keeps its URNs.
		ComponentType string
		// Flat registers no component: the children keep the caller's
		// parent, as in a program that never had one.
		Flat bool
		// Names are the logical names of the children.
		Names Logical
		// Aliases are added to the children, by kind.
		Aliases map[Kind][]pulumi.Alias
		// ComponentAliases are added to the component.
		ComponentAliases []pulumi.Alias
		// GroupTagsExcludeCommon leaves Args.Tags off the group's propagated
		// tags (only Name and ManagedBy remain), as programs did before.
		GroupTagsExcludeCommon bool
		// LaunchTemplateTagTypes are the resource types the launch template
		// tags at launch, in this order. Empty: instance and volume. A
		// program that tagged only its instances lists "instance", so its
		// launch template keeps its version and nothing is refreshed.
		LaunchTemplateTagTypes []string
		// AssumeRolePolicy, when set, is the role's trust policy, byte for
		// byte, instead of [AssumeRolePolicy]: for a program whose own
		// document says the same in another layout. It must be a JSON
		// object.
		AssumeRolePolicy string
	}

	// Logical overrides logical names. An empty field or nil func keeps the
	// default, which is documented on each field.
	Logical struct {
		// Role, InstanceProfile, SecurityGroup, LaunchTemplate and Group
		// default to the component's name.
		Role, InstanceProfile, SecurityGroup, LaunchTemplate, Group string
		// Hook defaults to "<name>-hook".
		Hook string
		// Instance (single-instance mode) defaults to the component's name.
		Instance string
		// DataVolume and DataAttachment default to "<name>-data" and
		// "<name>-data-attachment".
		DataVolume, DataAttachment string
		// TargetGroupAttachment defaults to "<name>-target-<index>".
		TargetGroupAttachment func(index int) string
		// ManagedPolicy defaults to "<name>-managed-<index>".
		ManagedPolicy func(index int, arn string) string
		// Ingress and Egress default to "<name>-ingress-<i>[-<j>]" and
		// "<name>-egress-<i>[-<j>]" for separate rules.
		Ingress, Egress func(index, address int) string
		// Alarm defaults to "<name>-alarm-<suffix>".
		Alarm func(suffix string) string
		// Object defaults to "<name>-object-<first 12 of the digest>".
		Object func(bootstrap.Object) string
	}
)

// Kind is a kind of child, for [Compat.Aliases].
type Kind string

// Kinds.
const (
	KindRole            Kind = "role"
	KindInstanceProfile Kind = "instance-profile"
	KindManagedPolicy   Kind = "managed-policy"
	KindSecurityGroup   Kind = "security-group"
	KindSecurityRule    Kind = "security-rule"
	KindLaunchTemplate  Kind = "launch-template"
	KindGroup           Kind = "group"
	KindInstance        Kind = "instance"
	KindDataVolume      Kind = "data-volume"
	KindDataAttachment  Kind = "data-attachment"
	KindTargetAttach    Kind = "target-attachment"
	KindHook            Kind = "hook"
	KindAlarm           Kind = "alarm"
	KindObject          Kind = "object"
)
