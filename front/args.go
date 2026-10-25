package front

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Type tokens of the components.
const (
	TypeSharedNLB = "truvity:asg:SharedNLB"
	TypeFront     = "truvity:asg:Front"
)

// Defaults.
const (
	// DefaultHealthCheckInterval is the target group's health check interval
	// in seconds (an NLB takes 10 or 30).
	DefaultHealthCheckInterval = 10
	// DefaultHealthCheckThreshold is both the healthy and the unhealthy
	// threshold: an NLB requires them to be equal.
	DefaultHealthCheckThreshold = 3
	// MaxListeners is the NLB quota of listeners; it is not adjustable.
	MaxListeners = 50
)

// Protocol is a listener protocol. TLS is not offered: a TLS listener
// terminates the client's TLS, which breaks a box that checks client
// certificates itself.
const (
	TCP    = "TCP"
	UDP    = "UDP"
	TCPUDP = "TCP_UDP"
)

type (
	// SharedNLB is what a box's stack needs to know about the shared NLB. It
	// is plain data, the shape of the file the NLB's stack writes and a box's
	// stack reads.
	SharedNLB struct {
		ARN             string `json:"arn"               yaml:"arn"`
		DNSName         string `json:"dns-name"          yaml:"dns-name"`
		ZoneID          string `json:"zone-id"           yaml:"zone-id"`
		SecurityGroupID string `json:"security-group-id" yaml:"security-group-id"`
		VPCID           string `json:"vpc-id"            yaml:"vpc-id"`
		// VPCCIDR is the range the NLB's egress is limited to.
		VPCCIDR string `json:"vpc-cidr" yaml:"vpc-cidr"`
	}

	// SharedNLBArgs configures [NewSharedNLB] and the NLB inside [NewNLB].
	SharedNLBArgs struct {
		// Name is the physical name of the NLB and its security group,
		// at most 32 characters. Default: the logical name.
		Name string
		// VPCID and VPCCIDR are required. The security group's egress is
		// limited to VPCCIDR.
		VPCID   pulumi.StringInput
		VPCCIDR pulumi.StringInput
		// SubnetIDs are the subnets of an internal NLB, one per zone.
		SubnetIDs []pulumi.StringInput
		// CrossZone defaults to true: a box may run in one zone while its
		// clients sit in all of them.
		CrossZone *bool
		// DeletionProtection defaults to true. Deleting an NLB silently
		// deletes every listener on it, so the protection is lifted
		// deliberately, once no box is left.
		DeletionProtection *bool
		// Tags are added to every resource that takes tags.
		Tags map[string]string
	}

	// NLBArgs configures [NewNLB]: a dedicated NLB and its listeners.
	NLBArgs struct {
		SharedNLBArgs
		Listeners []Listener
	}

	// Listener is one service of a box: a listener port, the target group
	// behind it and who may connect.
	Listener struct {
		// Name names the target group, the listener and the rules, for
		// example "buildkit-arm64": at most 32 characters of letters, digits
		// and hyphens, unique on the NLB. It is the Name tag of the target
		// group, whose physical name is generated.
		Name string
		// Port is the listener port, from the port registry. Unique on the
		// NLB.
		Port int
		// TargetPort is the daemon's port on the instance. Default: Port.
		TargetPort int
		// Protocol is TCP (default), UDP or TCP_UDP.
		Protocol string
		// IdleTimeoutSeconds is the listener's TCP idle timeout, 60..6000;
		// 0 keeps the AWS default (350). TCP only.
		IdleTimeoutSeconds int
		// DeregistrationDelaySeconds is the target group's deregistration
		// delay, 0..3600; 0 keeps the AWS default (300). Auto Scaling
		// deregisters a terminating instance and waits this long before the
		// terminate lifecycle hook runs.
		DeregistrationDelaySeconds int
		// TerminateOnDeregistration closes connections still open when the
		// delay ends.
		TerminateOnDeregistration bool
		// PreserveClientIP nil keeps the AWS default (on for instance
		// targets). The target's security group admits the NLB's group
		// either way.
		PreserveClientIP *bool
		HealthCheck      HealthCheck
		// Sources are the security groups or ranges allowed to connect to
		// this port, and nothing else. At least one.
		Sources []Source
	}

	// HealthCheck is the target group's health check. The zero value is TCP
	// on the target port.
	HealthCheck struct {
		// Protocol is TCP (default), HTTP or HTTPS.
		Protocol string
		// Port is the check's port on the instance; 0 is the target port.
		// Required for a UDP listener, whose check cannot be UDP.
		Port int
		// Path is required for HTTP and HTTPS and refused for TCP.
		Path string
		// IntervalSeconds is 10 (default) or 30.
		IntervalSeconds int
		// Threshold is the healthy and the unhealthy count, 2..10.
		// Default 3.
		Threshold int
	}

	// Source is one thing allowed to connect: exactly one of SecurityGroupID
	// and CIDR.
	Source struct {
		SecurityGroupID pulumi.StringInput
		CIDR            string
		// Description is the rule's description.
		Description string
		// World acknowledges that a CIDR opens the port to 0.0.0.0/0.
		World bool
	}
)
