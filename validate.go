package asg

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/truvity/asg/bootstrap"
)

// partitionExpr is the partition part of an ARN. The ARN regexes are built
// from it so that no ARN prefix is written out in the source (leak canary).
const partitionExpr = `aws[a-z-]*`

var (
	resourceNameRe = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,64}$`)
	hookNameRe     = regexp.MustCompile(`^[A-Za-z0-9 _.:/@+=,-]{1,255}$`)
	bucketRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	keyRe          = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	paramRe        = regexp.MustCompile(`^/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	instanceTypeRe = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z0-9]+$`)
	partitionRe    = regexp.MustCompile(`^aws(-[a-z]+)*$`)
	boundaryRe     = regexp.MustCompile(`^arn:` + partitionExpr + `:iam::[0-9]{12}:policy/.+$`)
	policyARNRe    = regexp.MustCompile(`^arn:` + partitionExpr + `:iam::(aws|[0-9]{12}):policy/.+$`)
	kmsARNRe       = regexp.MustCompile(`^arn:` + partitionExpr + `:kms:[a-z0-9-]+:[0-9]{12}:key/[A-Za-z0-9-]+$`)
	actionRe       = regexp.MustCompile(`^[A-Za-z0-9-]+:[A-Za-z0-9*?]+$`)
	sidRe          = regexp.MustCompile(`^[A-Za-z0-9]*$`)
	suffixRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	amiNameRe      = regexp.MustCompile(`^[A-Za-z0-9_.*?()/\-]+$`)
	timeoutRe      = regexp.MustCompile(`^[0-9]+(s|m|h)$`)
	deviceRe       = regexp.MustCompile(`^[a-z]{1,4}[0-9]?$`)
	dataDeviceRe   = regexp.MustCompile(`^/dev/sd[f-p]$`)
	groupActionRe  = regexp.MustCompile(`^autoscaling:[A-Z][A-Za-z]+$`)
	ssmAMIRe       = regexp.MustCompile(`^/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
)

var (
	comparisons = map[string]bool{
		"GreaterThanThreshold": true, "GreaterThanOrEqualToThreshold": true,
		"LessThanThreshold": true, "LessThanOrEqualToThreshold": true,
	}
	protocols = map[string]bool{"tcp": true, "udp": true, "icmp": true, "icmpv6": true, "-1": true}
	// volumeTypes are the EBS types a data volume may have.
	volumeTypes = map[string]bool{"gp3": true, "gp2": true, "io1": true, "io2": true, "st1": true, "sc1": true}
	// terminationPolicies are the Auto Scaling termination policies by name.
	terminationPolicies = map[string]bool{
		"Default": true, "AllocationStrategy": true, "OldestLaunchTemplate": true, "OldestLaunchConfiguration": true,
		"ClosestToNextInstanceHour": true, "NewestInstance": true, "OldestInstance": true,
	}
)

// withDefaults returns a copy of a with every default applied.
func (a Args) withDefaults(name string) Args {
	if a.Partition == "" {
		a.Partition = DefaultPartition
	}

	if a.Group.Min == 0 && a.Group.Max == 0 && a.Group.Desired == 0 {
		a.Group.Min = 1
	}

	if a.Group.Max == 0 {
		a.Group.Max = max(a.Group.Min, a.Group.Desired)
	}

	if a.Group.Desired == 0 {
		a.Group.Desired = a.Group.Min
	}

	if a.Group.HealthCheckType == "" {
		a.Group.HealthCheckType = "EC2"
	}

	if a.Group.GracePeriodSeconds == 0 {
		a.Group.GracePeriodSeconds = DefaultGracePeriod
	}

	if a.Instance.HopLimit == 0 {
		a.Instance.HopLimit = DefaultHopLimit
	}

	if rv := a.Instance.RootVolume; rv != nil {
		cp := *rv
		if cp.DeviceName == "" {
			cp.DeviceName = DefaultRootDevice
		}

		if cp.SizeGiB == 0 {
			cp.SizeGiB = DefaultRootVolumeGiB
		}

		if cp.Type == "" {
			cp.Type = DefaultVolumeType
		}

		a.Instance.RootVolume = &cp
	}

	if len(a.Instance.Volumes) > 0 {
		vs := make([]VolumeArgs, len(a.Instance.Volumes))
		for i, v := range a.Instance.Volumes {
			if v.Type == "" {
				v.Type = DefaultVolumeType
			}

			vs[i] = v
		}

		a.Instance.Volumes = vs
	}

	if h := a.Hook; h != nil {
		cp := *h
		if cp.HeartbeatTimeoutSeconds == 0 {
			cp.HeartbeatTimeoutSeconds = DefaultHeartbeatSeconds
		}

		if cp.DefaultResult == "" {
			cp.DefaultResult = "ABANDON"
		}

		a.Hook = &cp
	}

	if w := a.WarmPool; w != nil {
		cp := *w
		if cp.State == "" {
			cp.State = "Stopped"
		}

		a.WarmPool = &cp
	}

	if r := a.Refresh; r != nil {
		cp := *r
		if cp.WarmupSeconds == 0 {
			cp.WarmupSeconds = DefaultGracePeriod
		}

		a.Refresh = &cp
	}

	if a.Objects.ContentType == "" {
		a.Objects.ContentType = DefaultContentType
	}

	if len(a.Image.Owners) == 0 && a.Image.NameFilter != "" {
		a.Image.Owners = []string{"amazon"}
	}

	if a.IAM.PolicyName == "" {
		a.IAM.PolicyName = DefaultPolicyName
	}

	if a.Security.Description == "" {
		a.Security.Description = a.Names.Resolve(name).Base + " appliance"
	}

	return a
}

// objectKeys are every key the role may read: the user-data's objects, the
// extra ones, Args.IAM's and the modules'.
func (a Args) objectKeys() []string {
	var keys []string

	if a.UserData != nil {
		for _, o := range a.UserData.Objects {
			keys = append(keys, o.Key)
		}
	}

	for _, o := range a.Objects.Extra {
		keys = append(keys, o.Key)
	}

	return sortedUnique(keys, a.IAM.ObjectKeys, a.Contribution.ObjectKeys)
}

// ssmParameters are every parameter the role may read.
func (a Args) ssmParameters() []string {
	var params []string
	if a.UserData != nil {
		params = a.UserData.SSMParameters
	}

	return sortedUnique(params, a.IAM.SSMParameters, a.Contribution.SSMParameters)
}

// validate reports every problem it can find, not only the first. a has its
// defaults applied.
func (a Args) validate(name string) error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}

	if name == "" {
		errs = append(errs, errors.New("appliance: the logical name is empty"))
	}

	if !partitionRe.MatchString(a.Partition) {
		add("Partition %q: want aws, aws-cn, aws-us-gov", a.Partition)
	}

	r := a.Names.Resolve(name)
	for what, v := range map[string]string{
		"Names.Group": r.Group, "Names.LaunchTemplate": r.LaunchTemplate, "Names.Role": r.Role, "Names.InstanceProfile": r.InstanceProfile,
	} {
		if !resourceNameRe.MatchString(v) {
			add("%s %q: want 1-64 of letters, digits and + = , . @ _ -", what, v)
		}
	}

	if r.SecurityGroup != "" && !resourceNameRe.MatchString(r.SecurityGroup) {
		add("Names.SecurityGroup %q: want 1-64 of letters, digits and + = , . @ _ -", r.SecurityGroup)
	}

	if !hookNameRe.MatchString(r.Hook) {
		add("Names.Hook %q is not a hook name", r.Hook)
	}

	errs = append(errs, a.validateUserData(name)...)
	errs = append(errs, a.validateImage(name)...)
	errs = append(errs, a.validateCompute(name)...)
	errs = append(errs, a.validateGroup(name)...)
	errs = append(errs, a.validateIAM(name)...)
	errs = append(errs, a.validateSecurity(name)...)
	errs = append(errs, a.validateAlarms(name)...)
	errs = append(errs, a.validateCompat(name)...)

	return errors.Join(errs...)
}

func (a Args) validateUserData(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}

	if a.UserData == nil {
		add("UserData is nil: pass a rendered bootstrap plan, or bootstrap.Verbatim")

		return errs
	}

	if len(a.UserData.UserData) == 0 {
		add("UserData is empty")
	}

	if n := len(a.UserData.UserData); n > 16384 {
		add("UserData is %d bytes, over the 16384-byte EC2 limit", n)
	}

	hasObjects := len(a.UserData.Objects) > 0 || len(a.Objects.Extra) > 0
	switch {
	case hasObjects && a.Objects.Bucket == "":
		add("the user-data has S3 objects but Objects.Bucket is empty")
	case a.Objects.Bucket != "" && !bucketRe.MatchString(a.Objects.Bucket):
		add("Objects.Bucket %q is not a bucket name", a.Objects.Bucket)
	}

	if a.Objects.KMSKeyARN != "" && !kmsARNRe.MatchString(a.Objects.KMSKeyARN) {
		add("Objects.KMSKeyARN %q is not a KMS key ARN (a key, not an alias)", a.Objects.KMSKeyARN)
	}

	for _, o := range append(append([]bootstrap.Object{}, a.UserData.Objects...), a.Objects.Extra...) {
		if !keyRe.MatchString(o.Key) || strings.Contains(o.Key, "..") {
			add("object key %q: want letters, digits and _ . - between slashes", o.Key)
		}
	}

	if hasObjects || len(a.IAM.ObjectKeys)+len(a.Contribution.ObjectKeys) > 0 {
		for _, k := range append(append([]string{}, a.IAM.ObjectKeys...), a.Contribution.ObjectKeys...) {
			if !keyRe.MatchString(k) || strings.Contains(k, "..") {
				add("object key %q: want letters, digits and _ . - between slashes", k)
			}
		}
	}

	return errs
}

func (a Args) validateImage(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}

	set := 0
	if a.Image.ID != nil {
		set++
	}

	if a.Image.NameFilter != "" {
		set++
	}

	if a.Image.SSMParameter != "" {
		set++
	}

	if set != 1 {
		add("Image: set exactly one of ID, NameFilter and SSMParameter (%d are set)", set)
	}

	if a.Image.NameFilter != "" && !amiNameRe.MatchString(a.Image.NameFilter) {
		add("Image.NameFilter %q: unexpected characters", a.Image.NameFilter)
	}

	if a.Image.SSMParameter != "" && !ssmAMIRe.MatchString(a.Image.SSMParameter) {
		add("Image.SSMParameter %q is not a parameter name", a.Image.SSMParameter)
	}

	if a.Image.NameFilter == "" && (len(a.Image.Owners) > 0 || a.Image.Architecture != "") {
		add("Image.Owners and Image.Architecture apply to NameFilter")
	}

	if arch := a.Image.Architecture; arch != "" && arch != "x86_64" && arch != "arm64" {
		add("Image.Architecture %q: want x86_64 or arm64", arch)
	}

	return errs
}

func (a Args) validateCompute(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}

	switch {
	case a.Instance.Type != "" && len(a.Instance.Types) > 0:
		add("Instance.Type and Instance.Types are both set: name one type, or several")
	case len(a.Instance.Types) > 0:
		seen := map[string]bool{}

		for i, t := range a.Instance.Types {
			if !instanceTypeRe.MatchString(t) {
				add("Instance.Types[%d] %q is not an instance type", i, t)
			}

			if seen[t] {
				add("Instance.Types names %q twice", t)
			}

			seen[t] = true
		}

		if a.WarmPool != nil {
			add("Instance.Types makes a mixed instances policy, which Auto Scaling does not allow with a warm pool: name one Type")
		}
	case !instanceTypeRe.MatchString(a.Instance.Type):
		add("Instance.Type %q is not an instance type", a.Instance.Type)
	}

	if a.Instance.HopLimit < 1 || a.Instance.HopLimit > 64 {
		add("Instance.HopLimit %d: want 1..64", a.Instance.HopLimit)
	}

	if rv := a.Instance.RootVolume; rv != nil {
		if rv.SizeGiB < 1 {
			add("Instance.RootVolume.SizeGiB %d: want at least 1", rv.SizeGiB)
		}

		if !strings.HasPrefix(rv.DeviceName, "/dev/") && !deviceRe.MatchString(rv.DeviceName) {
			add("Instance.RootVolume.DeviceName %q: want /dev/xvda or sda1 style", rv.DeviceName)
		}
	}

	if len(a.Instance.Volumes) > MaxDataVolumes {
		add("Instance.Volumes has %d volumes: at most %d (/dev/sdf to /dev/sdp)", len(a.Instance.Volumes), MaxDataVolumes)
	}

	devices := map[string]bool{}
	if rv := a.Instance.RootVolume; rv != nil {
		devices[rv.DeviceName] = true
	}

	for i, v := range a.Instance.Volumes {
		if !dataDeviceRe.MatchString(v.DeviceName) {
			add("Instance.Volumes[%d].DeviceName %q: want /dev/sdf to /dev/sdp", i, v.DeviceName)
		}

		if devices[v.DeviceName] {
			add("Instance.Volumes[%d].DeviceName %q is used twice", i, v.DeviceName)
		}

		devices[v.DeviceName] = true

		if v.SizeGiB < 1 || v.SizeGiB > 16384 {
			add("Instance.Volumes[%d].SizeGiB %d: want 1..16384", i, v.SizeGiB)
		}

		if !volumeTypes[v.Type] {
			add("Instance.Volumes[%d].Type %q: want gp3, gp2, io1, io2, st1 or sc1", i, v.Type)
		}

		if v.IOPS < 0 || v.ThroughputMiBps < 0 {
			add("Instance.Volumes[%d]: IOPS and ThroughputMiBps must not be negative", i)
		}

		if v.ThroughputMiBps > 0 && v.Type != "gp3" {
			add("Instance.Volumes[%d].ThroughputMiBps is set on a %s volume: only gp3 takes a throughput", i, v.Type)
		}

		if v.KMSKeyID != "" && !kmsARNRe.MatchString(v.KMSKeyID) {
			add("Instance.Volumes[%d].KMSKeyID %q: want a key ARN", i, v.KMSKeyID)
		}
	}

	for i, s := range a.Instance.ExtraSecurityGroupIDs {
		if s == nil {
			add("Instance.ExtraSecurityGroupIDs[%d] is nil", i)
		}
	}

	return errs
}

func (a Args) validateGroup(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}
	g := a.Group

	if g.Min < 0 || g.Max < 1 || g.Min > g.Desired || g.Desired > g.Max {
		add("Group sizes: want 0 <= Min (%d) <= Desired (%d) <= Max (%d) and Max >= 1", g.Min, g.Desired, g.Max)
	}

	if len(g.SubnetIDs) == 0 {
		add("Group.SubnetIDs is empty: the group needs a subnet (two in different zones to survive a zone outage)")
	}

	for i, s := range g.SubnetIDs {
		if s == nil {
			add("Group.SubnetIDs[%d] is nil", i)
		}
	}

	if g.HealthCheckType != "EC2" && g.HealthCheckType != "ELB" {
		add("Group.HealthCheckType %q: want EC2 or ELB", g.HealthCheckType)
	}

	if g.GracePeriodSeconds < 0 {
		add("Group.GracePeriodSeconds %d is negative", g.GracePeriodSeconds)
	}

	seenPolicy := map[string]bool{}
	for _, p := range g.TerminationPolicies {
		if !terminationPolicies[p] {
			add("Group.TerminationPolicies %q: not a termination policy", p)
		}

		if seenPolicy[p] {
			add("Group.TerminationPolicies names %q twice", p)
		}

		seenPolicy[p] = true
	}

	for i, t := range g.TargetGroupARNs {
		if t == nil {
			add("Group.TargetGroupARNs[%d] is nil", i)
		}
	}

	if h := a.Hook; h != nil {
		if h.HeartbeatTimeoutSeconds < 30 || h.HeartbeatTimeoutSeconds > 7200 {
			add("Hook.HeartbeatTimeoutSeconds %d: want 30..7200", h.HeartbeatTimeoutSeconds)
		}

		switch h.DefaultResult {
		case "ABANDON":
			if h.UnsafeDefaultContinue {
				add("Hook.UnsafeDefaultContinue is set but DefaultResult is ABANDON")
			}
		case "CONTINUE":
			if !h.UnsafeDefaultContinue {
				add("Hook.DefaultResult CONTINUE puts an instance in service whose scripts never answered; set UnsafeDefaultContinue to accept that")
			}
		default:
			add("Hook.DefaultResult %q: want ABANDON or CONTINUE", h.DefaultResult)
		}

		if h.Mode != HookInline && h.Mode != HookSeparate {
			add("Hook.Mode %d: unknown", h.Mode)
		}
	}

	if w := a.WarmPool; w != nil {
		if a.Hook == nil {
			add("WarmPool needs Hook: without a launch hook the instance is stopped before its first boot finishes")
		}

		if w.State != "Stopped" && w.State != "Running" && w.State != "Hibernated" {
			add("WarmPool.State %q: want Stopped, Running or Hibernated", w.State)
		}

		if w.MinSize < 0 {
			add("WarmPool.MinSize %d is negative", w.MinSize)
		}

		if c := w.MaxGroupPreparedCapacity; c != nil && *c < g.Max {
			add("WarmPool.MaxGroupPreparedCapacity %d is below Group.Max %d: the pool could not hold a full group", *c, g.Max)
		}
	}

	if r := a.Refresh; r != nil {
		if r.MinHealthyPercentage < 0 || r.MinHealthyPercentage > 100 {
			add("Refresh.MinHealthyPercentage %d: want 0..100", r.MinHealthyPercentage)
		}

		if r.WarmupSeconds < 0 {
			add("Refresh.WarmupSeconds %d is negative", r.WarmupSeconds)
		}

		if r.MaxHealthyPercentage != 0 && (r.MaxHealthyPercentage < 100 || r.MaxHealthyPercentage > 200) {
			add("Refresh.MaxHealthyPercentage %d: want 0 (unset) or 100..200", r.MaxHealthyPercentage)
		}
	}

	return errs
}

func (a Args) validateIAM(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}
	i := a.IAM

	if i.PermissionsBoundary != "" && !boundaryRe.MatchString(i.PermissionsBoundary) {
		add("IAM.PermissionsBoundary %q is not a policy ARN", i.PermissionsBoundary)
	}

	if !resourceNameRe.MatchString(i.PolicyName) {
		add("IAM.PolicyName %q: want 1-64 of letters, digits and + = , . @ _ -", i.PolicyName)
	}

	for _, arn := range append(append([]string{}, i.ManagedPolicyARNs...), a.Contribution.ManagedPolicyARNs...) {
		if !policyARNRe.MatchString(arn) {
			add("managed policy %q is not a policy ARN", arn)
		}
	}

	for _, p := range append(append([]string{}, i.SSMParameters...), a.Contribution.SSMParameters...) {
		if !paramRe.MatchString(p) || strings.Contains(p, "..") {
			add("ssm parameter %q: want an absolute name like /estate/token", p)
		}
	}

	if a.UserData != nil {
		for _, p := range a.UserData.SSMParameters {
			if !paramRe.MatchString(p) {
				add("user-data ssm parameter %q: want an absolute name like /estate/token", p)
			}
		}
	}

	if i.Policy != nil && (len(i.Statements) > 0 || i.StatementsFunc != nil || len(i.SSMParameters) > 0 || len(i.ObjectKeys) > 0 ||
		len(a.Contribution.Statements) > 0 || len(a.Contribution.SSMParameters) > 0 || len(a.Contribution.ObjectKeys) > 0 ||
		len(a.Contribution.GroupActions) > 0) {
		add("IAM.Policy replaces the generated policy: it excludes Statements, StatementsFunc, SSMParameters, ObjectKeys and the modules' grants")
	}

	for n, s := range append(append([]Statement{}, i.Statements...), a.Contribution.Statements...) {
		errs = append(errs, validateStatement(name, fmt.Sprintf("statement %d", n), s)...)
	}

	for _, act := range a.Contribution.GroupActions {
		if !groupActionRe.MatchString(act) {
			add("group action %q: want one autoscaling:Action, no wildcard", act)
		}
	}

	seen := map[string]bool{i.PolicyName: true}
	for n, p := range i.ExtraInlinePolicies {
		switch {
		case !resourceNameRe.MatchString(p.Name):
			add("IAM.ExtraInlinePolicies[%d].Name %q is not a policy name", n, p.Name)
		case seen[p.Name]:
			add("inline policy name %q is used twice", p.Name)
		case p.Policy == nil:
			add("IAM.ExtraInlinePolicies[%d] (%s) has no Policy", n, p.Name)
		}

		seen[p.Name] = true
	}

	return errs
}

func validateStatement(name, where string, s Statement) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: %s: "+format, append([]any{name, where}, v...)...))
	}

	if s.Effect != "" && s.Effect != "Allow" && s.Effect != "Deny" {
		add("Effect %q: want Allow or Deny", s.Effect)
	}

	if !sidRe.MatchString(s.Sid) {
		add("Sid %q: want letters and digits only", s.Sid)
	}

	if len(s.Action) == 0 {
		add("no Action")
	}

	for _, act := range s.Action {
		if !actionRe.MatchString(act) {
			add("Action %q: want service:Name, and a bare * is refused", act)
		}
	}

	if len(s.Resource) == 0 {
		add("no Resource")
	}

	for _, r := range s.Resource {
		if r == "" {
			add("empty Resource")
		}
	}

	return errs
}

func (a Args) validateSecurity(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}
	s := a.Security

	existing := len(s.ExistingGroups) > 0

	switch {
	case existing && (s.VPCID != nil || len(s.Ingress) > 0 || s.Egress != nil || len(a.Contribution.Ingress) > 0):
		add("Security.ExistingGroups: no group is created, so VPCID, Ingress, Egress and the modules' ingress rules are refused")
	case !existing && s.VPCID == nil:
		add("Security.VPCID is nil")
	}

	for i, g := range s.ExistingGroups {
		if g == nil {
			add("Security.ExistingGroups[%d] is nil", i)
		}
	}

	if s.Style < RulesInline || s.Style > RulesLegacy {
		add("Security.Style %d: unknown", s.Style)
	}

	if s.DeleteTimeout != "" && !timeoutRe.MatchString(s.DeleteTimeout) {
		add("Security.DeleteTimeout %q: want a duration such as 30m", s.DeleteTimeout)
	}

	ingress := append(append([]Rule{}, s.Ingress...), ingressRules(a.Contribution.Ingress)...)
	for i := range ingress {
		errs = append(errs, validateRule(name, fmt.Sprintf("ingress rule %d", i), &ingress[i], true)...)
	}

	for i := range s.Egress {
		errs = append(errs, validateRule(name, fmt.Sprintf("egress rule %d", i), &s.Egress[i], false)...)
	}

	return errs
}

func validateRule(name, where string, r *Rule, ingress bool) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: %s: "+format, append([]any{name, where}, v...)...))
	}

	proto := r.Protocol
	if proto == "" {
		proto = "tcp"
	}

	if !protocols[proto] {
		add("Protocol %q: want tcp, udp, icmp, icmpv6 or -1", r.Protocol)
	}

	switch {
	case proto == "-1" && (r.FromPort != 0 || r.ToPort != 0):
		add("ports apply to tcp and udp; -1 means all")
	case r.FromPort < 0 || r.ToPort > 65535 || r.FromPort > r.ToPort:
		add("ports %d-%d: want 0 <= from <= to <= 65535", r.FromPort, r.ToPort)
	case (proto == "tcp" || proto == "udp") && r.ToPort == 0:
		add("a tcp or udp rule needs a port")
	}

	if len(r.CIDRs)+len(r.IPv6CIDRs)+len(r.SourceGroups) == 0 {
		add("no CIDRs, IPv6CIDRs or SourceGroups")
	}

	world := false

	for _, c := range r.CIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			add("CIDR %q is not a canonical IPv4 CIDR", c)

			continue
		}

		world = world || p.Bits() == 0
	}

	for _, c := range r.IPv6CIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is6() || p != p.Masked() {
			add("IPv6 CIDR %q is not a canonical IPv6 CIDR", c)

			continue
		}

		world = world || p.Bits() == 0
	}

	for i, g := range r.SourceGroups {
		if g == nil {
			add("SourceGroups[%d] is nil", i)
		}
	}

	if ingress && world && !r.World {
		add("opens the port to the whole internet; set World to say so")
	}

	if r.World && !world {
		add("World is set but no CIDR is 0.0.0.0/0 or ::/0")
	}

	if !ingress && r.World {
		add("World applies to ingress rules")
	}

	return errs
}

func (a Args) validateAlarms(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}

	suffixes := map[string]bool{}
	metrics := map[string]bool{}

	for _, m := range a.Group.EnabledMetrics {
		metrics[m] = true
	}

	for i := range a.Alarms.Alarms {
		al := &a.Alarms.Alarms[i]
		switch {
		case !suffixRe.MatchString(al.Suffix):
			add("Alarms[%d].Suffix %q: want lower-case letters, digits and -", i, al.Suffix)
		case suffixes[al.Suffix]:
			add("alarm suffix %q is used twice", al.Suffix)
		}

		suffixes[al.Suffix] = true

		if al.Namespace == "" || al.MetricName == "" || al.Statistic == "" {
			add("alarm %q: Namespace, MetricName and Statistic are required", al.Suffix)
		}

		if !comparisons[al.Comparison] {
			add("alarm %q: Comparison %q is not a CloudWatch comparison operator", al.Suffix, al.Comparison)
		}

		if al.PeriodSeconds < 10 || al.EvaluationPeriods < 1 || al.DatapointsToAlarm < 0 || al.DatapointsToAlarm > al.EvaluationPeriods {
			add("alarm %q: want PeriodSeconds >= 10, EvaluationPeriods >= 1 and 0 <= DatapointsToAlarm <= EvaluationPeriods", al.Suffix)
		}

		if al.Namespace == "AWS/AutoScaling" && len(al.Dimensions) == 0 && !metrics[al.MetricName] {
			add("alarm %q: the group metric %s is not in Group.EnabledMetrics, so it would never receive data", al.Suffix, al.MetricName)
		}
	}

	if len(a.Alarms.ActionARNs)+len(a.Alarms.OKActionARNs) > 0 && len(a.Alarms.Alarms) == 0 {
		add("Alarms: action ARNs without alarms")
	}

	return errs
}

func (a Args) validateCompat(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}
	c := a.Compat

	if c.Flat && c.ComponentType != "" {
		add("Compat.Flat registers no component, so Compat.ComponentType has no use")
	}

	if c.Flat && len(c.ComponentAliases) > 0 {
		add("Compat.Flat registers no component, so Compat.ComponentAliases has no use")
	}

	seen := map[string]bool{}

	for _, t := range c.LaunchTemplateTagTypes {
		switch t {
		case "instance", "volume", "network-interface", "spot-instances-request":
		default:
			add("Compat.LaunchTemplateTagTypes: %q is not a resource type a launch template tags (instance, volume, network-interface, spot-instances-request)", t)
		}

		if seen[t] {
			add("Compat.LaunchTemplateTagTypes names %q twice", t)
		}

		seen[t] = true
	}

	if p := c.AssumeRolePolicy; p != "" {
		var doc map[string]any
		if err := json.Unmarshal([]byte(p), &doc); err != nil {
			add("Compat.AssumeRolePolicy is not a JSON object: %v", err)
		}
	}

	for k := range c.Aliases {
		switch k {
		case KindRole, KindInstanceProfile, KindManagedPolicy, KindSecurityGroup, KindSecurityRule,
			KindLaunchTemplate, KindGroup, KindHook, KindAlarm, KindObject:
		default:
			add("Compat.Aliases: unknown kind %q", k)
		}
	}

	return errs
}
