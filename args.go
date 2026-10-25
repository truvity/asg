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
	DefaultRootVolumeGiB    = 8
	DefaultVolumeType       = "gp3"
	DefaultPolicyName       = "appliance"
	DefaultContentType      = "text/plain"
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
		Group    GroupArgs
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
		// Type is required.
		Type string
		// HopLimit is the IMDS response hop limit, 1..64. Default 1. IMDSv2 is
		// always required.
		HopLimit int
		// RootVolume nil leaves the image's root volume alone.
		RootVolume *RootVolumeArgs
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
	}

	// Logical overrides logical names. An empty field or nil func keeps the
	// default, which is documented on each field.
	Logical struct {
		// Role, InstanceProfile, SecurityGroup, LaunchTemplate and Group
		// default to the component's name.
		Role, InstanceProfile, SecurityGroup, LaunchTemplate, Group string
		// Hook defaults to "<name>-hook".
		Hook string
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
	KindHook            Kind = "hook"
	KindAlarm           Kind = "alarm"
	KindObject          Kind = "object"
)
