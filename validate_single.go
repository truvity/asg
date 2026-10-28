package asg

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	tagKeyRe   = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,128}$`)
	tagValueRe = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{0,256}$`)
	azRe       = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9][a-z]$`)
)

// validateSingle checks single-instance mode: what it needs, and everything
// of group mode that has no meaning without a group.
func (a Args) validateSingle(name string) []error {
	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}
	s := a.Single
	g := a.Group

	if a.Hook != nil {
		add("Single has no Auto Scaling group, so Hook is refused")
	}

	if a.WarmPool != nil {
		add("Single has no Auto Scaling group, so WarmPool is refused")
	}

	if a.Refresh != nil {
		add("Single has no Auto Scaling group, so Refresh is refused")
	}

	if len(a.Instance.Types) > 0 {
		add("Single takes one Instance.Type: Types makes a mixed instances policy, which needs a group")
	}

	if g.Min != 0 || g.Max != 0 || g.Desired != 0 {
		add("Group.Min, Max and Desired (%d, %d, %d) are refused: Single is exactly one instance", g.Min, g.Max, g.Desired)
	}

	if len(g.SubnetIDs) > 0 || g.HealthCheckType != "" || g.GracePeriodSeconds != 0 || len(g.TargetGroupARNs) > 0 ||
		len(g.EnabledMetrics) > 0 || len(g.TerminationPolicies) > 0 {
		add("Group is refused with Single: the subnet is Single.SubnetID and target groups are Single.TargetGroups")
	}

	if len(a.Alarms.Alarms) > 0 || len(a.Alarms.ActionARNs) > 0 || len(a.Alarms.OKActionARNs) > 0 {
		add("Alarms are alarms on an Auto Scaling group: refused with Single")
	}

	if a.UserData != nil && a.UserData.HasLifecycle {
		add("the user-data was rendered with bootstrap.Options.Lifecycle, which completes a launch hook that Single does not have: render the plan without it")
	}

	if len(a.Contribution.GroupActions) > 0 {
		add("Contribution.GroupActions are Auto Scaling actions on the box's group: refused with Single")
	}

	if len(a.Contribution.InService)+len(a.Contribution.ReadyChecks) > 0 {
		add("Contribution has in-service steps or ready checks, which run under a launch hook: refused with Single")
	}

	if a.Instance.PublicIP != nil {
		add("Instance.PublicIP declares a launch template network interface, which cannot be combined with the instance's subnet: refused with Single")
	}

	if s.SubnetID == nil {
		add("Single.SubnetID is required")
	}

	switch {
	case !tagKeyRe.MatchString(s.RoleTagKey) || strings.HasPrefix(strings.ToLower(s.RoleTagKey), "aws:"):
		add("Single.RoleTagKey %q: want 1-128 of letters, digits and _ . : / = + @ - space, not starting with aws:", s.RoleTagKey)
	case s.RoleTagValue == "" || !tagValueRe.MatchString(s.RoleTagValue):
		add("Single.RoleTagValue %q: want 1-256 of letters, digits and _ . : / = + @ - space", s.RoleTagValue)
	}

	for k := range a.Tags {
		if k == s.RoleTagKey {
			add("Tags already has the key %q that Single.RoleTagKey sets", k)
		}
	}

	for i, t := range s.TargetGroups {
		if t.ARN == nil {
			add("Single.TargetGroups[%d].ARN is required", i)
		}

		if t.Port < 0 || t.Port > 65535 {
			add("Single.TargetGroups[%d].Port %d: want 0 (the target group's) or 1..65535", i, t.Port)
		}
	}

	errs = append(errs, a.validateData(name)...)

	return errs
}

func (a Args) validateData(name string) []error {
	d := a.Data
	if d == nil {
		return nil
	}

	var errs []error

	add := func(format string, v ...any) {
		errs = append(errs, fmt.Errorf("appliance %q: "+format, append([]any{name}, v...)...))
	}

	if !azRe.MatchString(d.AvailabilityZone) {
		add("Data.AvailabilityZone %q: want a zone such as eu-west-1a", d.AvailabilityZone)
	}

	if !dataDeviceRe.MatchString(d.DeviceName) {
		add("Data.DeviceName %q: want /dev/sdf to /dev/sdp", d.DeviceName)
	}

	for i, v := range a.Instance.Volumes {
		if v.DeviceName == d.DeviceName {
			add("Data.DeviceName %q is also Instance.Volumes[%d]", d.DeviceName, i)
		}
	}

	if d.SizeGiB < 1 || d.SizeGiB > 16384 {
		add("Data.SizeGiB %d: want 1..16384", d.SizeGiB)
	}

	if !volumeTypes[d.Type] {
		add("Data.Type %q: want gp3, gp2, io1, io2, st1 or sc1", d.Type)
	}

	if d.IOPS < 0 || d.ThroughputMiBps < 0 {
		add("Data: IOPS and ThroughputMiBps must not be negative")
	}

	if d.ThroughputMiBps > 0 && d.Type != "gp3" {
		add("Data.ThroughputMiBps is set on a %s volume: only gp3 takes a throughput", d.Type)
	}

	if d.KMSKeyID != "" && !kmsARNRe.MatchString(d.KMSKeyID) {
		add("Data.KMSKeyID %q: want a key ARN", d.KMSKeyID)
	}

	return errs
}
