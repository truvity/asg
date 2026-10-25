# Changelog

Entries are written for someone deciding whether to bump: what changed for
them, and for anything breaking, what to do.

## v0.3.0

- **New package `github.com/truvity/asg/front`: boxes behind a network load balancer.** `front.NewSharedNLB` creates an internal NLB with its security group (the group exists from creation, because an NLB made without one can never get one), cross-zone balancing and deletion protection on. `front.Attach` adds a box's target group, TCP listener and port-scoped ingress rules to it; `front.NewNLB` creates a dedicated NLB and returns the same `front.Front`. The port is the service's identity; TLS listeners are refused. See `docs/decisions/0004-front.md`.
- **`GroupArgs.TargetGroupARNs`.** The group registers in these target groups (`Front.TargetGroupARNs()`). With `HealthCheckType` ELB and the grace period, a failing target replaces the instance. Nothing changes for a group that sets none: the existing resource sets are byte for byte the same.

## v0.2.0

What the status box (truvity/observability `pkg/statusbox/ec2`) needs to move from carried-over user-data to composed user-data, and nothing more.

- **Breaking: instance refresh triggers are empty by default.** The Auto Scaling API starts a refresh on every launch template version change whenever the group has a refresh block, so the old default trigger `launch_template` was redundant, and Pulumi warned about it on every run ("instanceRefresh.triggers: 'launchTemplate' always triggers an instance refresh"). A consumer that relied on the default sees one in-place update of the group (the trigger is removed), no refresh and no replacement; a consumer that wants a trigger names it in `RefreshArgs.Triggers`. `RefreshArgs` also gains `MaxHealthyPercentage` (100 to 200), `SkipMatching` and `AutoRollback`.
- **Breaking: the default component type is `truvity:asg:Box`** (was `truvity:aws-structure:Appliance`). A consumer that sets `Compat.ComponentType`, as the status box does, is not affected. A consumer that relied on the default must either set `Compat.ComponentType` to the old value or add `Compat.ComponentAliases` with the old type; the goldens in `testdata/` were regenerated for the new type.
- **Breaking: module ingress rules are `module.Rule`.** `Contribution.Ingress` holds the Pulumi-free `module.Rule` (its source groups are plain ids); `Args.Security.Ingress` keeps `asg.Rule`. No module shipped before, so only a consumer that wrote its own module changes.
- **Added the `module` package**, the Pulumi-free extension mechanism: `Module`, `Contribution` (fragments, IAM statements, SSM parameters, object keys, ingress rules, managed policies, in-service steps, ready checks, watchdog checks, drains and scrape targets), `Collect` and `Contribution.AddTo`. `asg.Module`, `asg.Contribution`, `asg.Collect` and `asg.Statement` are aliases of it. The library now owns the on-box lifecycle: steps that must run only when an instance goes in service (never in the warm pool) and checks that must pass before the launch hook completes are written as files and run by one oneshot unit, `asg-in-service.service`, which `bootstrap-ready.service` waits for. Drains are accepted as data and refused by `AddTo`, because there is no terminate hook yet.
- **Added modules**, each returning data and creating no resource:
  - `modules/watchdog`: local checks every interval; after `FailMinutes` of failure it restarts the named units, then marks the instance Unhealthy. Lifted from the status box's health timer.
  - `modules/ssh`: opkssh through a bundle the caller renders (packages, files, commands), an optional fixed host key from SSM, the post-quantum key exchange preference, and TCP 22 for the listed networks. Lifted from the status box's `ssh.go`.
  - `modules/selfregister`: a Route 53 A record for the box's private IP, upserted through an assumed role when the instance goes in service. Lifted from the status box's `selfregister.go`.
  - `modules/tunnel`: a Cloudflare tunnel connector around a `bootstrap.UnitFragment` the caller copies from truvity/edge's `instanceconnector`. The binary and unit are installed on the first boot; the token is read and the unit started only on a boot into service. With `MetricsPort` the launch hook completes only once cloudflared has an edge connection.
- **Added** `bootstrap.Plan.HasLifecycle`.
- **Added** the `Policy` output of `asg.New` is documented as the exact JSON of the role's main inline policy, and `testdata/composed-policy.golden.json` is the review golden of a box made of the four modules.
- **Deprecated** `AlarmsArgs`, `Alarm` and `Args.Alarms`: monitoring is self-hosted and an alarm with no action is read by nobody. They keep working.
- The generated scripts say `github.com/truvity/asg/bootstrap` in their header comment instead of the old repository; the user-data of a consumer that renders a plan changes by that comment.
- Tests run against Pulumi mocks and fake commands; nothing reaches an AWS account.

## v0.1.0

- **The module `github.com/truvity/asg`, moved from truvity/aws-structure v0.32.0.** The EC2 appliance component is the root package (`asg.New`, formerly `github.com/truvity/aws-structure/pkg/appliance`) and the user-data composer is `github.com/truvity/asg/bootstrap` (formerly `.../pkg/bootstrap`). No behaviour change: the resource sets, rendered user-data and policies match the goldens that moved with them byte for byte, so a consumer that only changes its import paths gets an empty preview.
- ADRs 0001 (bootstrap) and 0002 (the appliance component), carried over from aws-structure's 0003 and 0004.
