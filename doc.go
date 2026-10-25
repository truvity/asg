// Package asg provisions an EC2 appliance: a machine, or a few, that
// is built from user-data, joins an Auto Scaling group and is replaced rather
// than repaired. It sits next to [bootstrap], which composes the user-data;
// this package gives that user-data a home and the least privilege it needs.
//
// One call, [New], registers these children under one component (or, with
// [Compat.Flat], directly under the caller's parent):
//
//   - an Auto Scaling group of Min..Max and a launch template;
//   - a warm pool of stopped instances (optional);
//   - an EC2_INSTANCE_LAUNCHING lifecycle hook, on the group or as a separate
//     resource, whose DefaultResult is ABANDON: bootstrap-ready.service sends
//     CONTINUE on every boot, so an instance that never finished is replaced
//     instead of put in service half configured;
//   - an IAM role and instance profile whose policy is built from the
//     user-data: ssm:GetParameter on exactly the plan's parameter names,
//     s3:GetObject on exactly the plan's object keys and the two Auto Scaling
//     actions the lifecycle scripts call, scoped to the group, plus the
//     statements the caller adds;
//   - a security group, its rules inline or separate;
//   - the plan's S3 objects, uploaded (this is the only thing bootstrap
//     leaves to the caller), and the CloudWatch alarms the caller asks for.
//
// The image is found by name filter or by SSM parameter at deploy time and
// pinned in the launch template, so a newer image is a visible diff and a
// rolling refresh. IMDSv2 is always required; the hop limit is configurable
// (a container on the instance needs 2).
//
// # Migrating without a replace
//
// An appliance that exists today is adopted in two steps, and both are
// possible with this package.
//
// Step A. The caller passes the old user-data unchanged ([bootstrap.Verbatim])
// and the options of [Compat] reproduce every logical name, parent, type and
// physical name; [Args.IAM].Policy returns the old policy document. The
// preview is empty: no launch template version, no refresh.
//
// Step B. The caller switches to a composed plan and drops the overrides. The
// launch template gets one new version, which an instance refresh rolls out,
// and the role's policy is tightened in place.
//
// # Extension points
//
// A [Module] contributes bootstrap fragments, IAM statements, SSM parameters,
// security group rules, and the steps and checks of the on-box lifecycle in
// one piece. [Collect] merges modules into a [Contribution] that
// [Args.Contribution] takes and that [Contribution.AddTo] feeds to the plan.
// The Pulumi-free package module defines the types and modules/ ships four:
// watchdog, ssh, selfregister and tunnel.
//
// # The on-box lifecycle
//
// A step that must run only when the instance goes in service (start a
// daemon, read a secret) is a [Step]; a check that must pass before the launch
// hook is completed is a [ReadyCheck]. The library writes them as files and
// runs them from one oneshot unit that does nothing in the warm pool, and
// bootstrap-ready.service completes the hook only if that unit succeeded.
package asg
