# Changelog

Entries are written for someone deciding whether to bump: what changed for
them, and for anything breaking, what to do.

## Unreleased

A plain-instance mode with a persistent data volume, for a box that something else starts and stops on demand. Nothing changes for a consumer that sets neither `Single` nor `Data`: every existing golden is byte for byte the same.

- **`Args.Single`: one `aws.ec2.Instance` instead of an Auto Scaling group.** Same launch template, role, instance profile, security group, image lookup, S3 objects and user-data. No group, lifecycle hook, warm pool, refresh or target group by the group. A launch template change (new image, new user-data) replaces the instance, deleted before created. The run state is not managed (no instance state resource), so a stopped instance shows no diff; a replacement comes up running. `SingleArgs` takes the `SubnetID`, a `RoleTagKey`/`RoleTagValue` tag the instance carries so that a proxy can find it, and optional `TargetGroups` (default none). `Appliance` gains `Instance`, `DataVolume`, `DataAttachment` and `TargetAttachments`; `Logical` gains `Instance`, `DataVolume`, `DataAttachment` and `TargetGroupAttachment`.
- **`Args.Single` refuses what needs a group:** `Hook`, `WarmPool`, `Refresh`, `Instance.Types`, `Alarms`, `Group.Min/Max/Desired` and the other `Group` fields, `Instance.PublicIP`, module `GroupActions`, in-service steps and ready checks, and user-data rendered with `bootstrap.Options.Lifecycle` (`bootstrap.Rendered.HasLifecycle` is new). The role gets no autoscaling permission.
- **`Args.Data`: a persistent, encrypted EBS volume** (gp3 by default, size, IOPS, throughput, optional KMS key) in one availability zone, created `protect`ed and `retainOnDelete`, attached with an `aws.ec2.VolumeAttachment` that is deleted first (the instance is stopped before the detach) and recreated on the new instance on a replacement. A subnet in another zone than `Data.AvailabilityZone` fails the update.
- **`modules/datavolume`: mount the volume from the user-data.** A oneshot unit `asg-data-volume.service` waits for the device (by its attached name, or by `VolumeID`), makes a file system only on a device with no signature at all, and mounts it on every boot; `Units` get `Requires=`/`After=` on it. It needs no lifecycle hook.

## v0.5.0

What the Tailscale subnet routers (truvity/tailscale `pkg/awsrouter`) need to move onto the library with no launch template version and no replacement. Nothing changes for a consumer that sets neither field: every golden is byte for byte the same.

- **`Compat.LaunchTemplateTagTypes`: the resource types the launch template tags at launch**, in the order given. Empty keeps `instance` and `volume`. A program that tagged only its instances lists `instance`; without it, the move adds a `volume` tag specification, which is a new launch template version and so an instance refresh of every box. Unknown types and duplicates are refused.
- **`Compat.AssumeRolePolicy`: the role's trust policy, byte for byte**, for a program whose own document says the same as `AssumeRolePolicy` in another layout (the routers' is compact JSON, the library's is indented). It must be a JSON object.

## v0.4.3

- **Fix: no vulnerable `go-git` in the module graph.** asg does not use `go-git` itself; the Pulumi SDK's workspace package does, and it required `github.com/go-git/go-git/v6` `v6.0.0-alpha.4`, which has two published advisories (worktree operations may follow symlinks, and malicious reference names may modify files outside the reference storage), both fixed in `v6.0.0-alpha.5`. `github.com/pulumi/pulumi/sdk/v3` moves from `v3.264.0` to `v3.268.0`, the first release that requires `go-git` `v6.0.0-beta.1`. The module graph now selects `go-git` `v6.0.0-beta.1`, with `go-billy/v6`, `go-crypto` and `sha1cd` moving along with it. No asg code or output changes. A consumer that pins the Pulumi SDK below `v3.268.0` gets it raised to `v3.268.0` when it bumps asg; one that already requires a newer SDK sees no change.

## v0.4.2

- **Fix: a group with a launch hook no longer waits for capacity.** The group was created without `wait_for_capacity_timeout`, so the provider's default of ten minutes applied. A group with a launch hook is by design not InService until the instance completes the hook, which can take longer than ten minutes (`HeartbeatTimeoutSeconds` of 1800, say). The create then timed out after the group existed but before the provider put the warm pool, and the state recorded a `warmPool` that was never made, so later updates never created it: no warm pool, silently. A group with `Hook` set now has `WaitForCapacityTimeout` `"0"`: the provider does not wait, and readiness is the hook's job. A group without a hook is unchanged and keeps the provider's wait, which is how a failed first launch shows up in the run. For an existing stack the update is one in-place change of the group, no replacement. A stack that lost its warm pool this way gets it back from a `pulumi refresh` followed by `pulumi up`; the `defaults`, `builder`, `composed`, `separate` and status box goldens gain the `waitForCapacityTimeout` input.

## v0.4.1

- **Fix: `front` target groups have a name that fits.** A target group was autonamed from its logical name (`<component>-<listener>`) plus Pulumi's 8 characters, which passes 32, the AWS limit, as soon as the two names are longer than 24 together; the first real preview refused it ("plus 7 random chars is longer than maximum length 32"). The name is now the listener's name cut to 24 characters, `-`, and 7 hex digits of the sha256 of the protocol, the target port and the VPC, so it still changes with everything that replaces a target group. No target group was ever created by v0.3.0 or v0.4.0; the `front` goldens gain the `name` input.

## v0.4.0

What a builder box with a local cache needs. Nothing changes for a consumer that sets none of the new fields: the existing resource sets and policies are byte for byte the same; the watchdog's script gains one check, so a box with the watchdog gets a new launch template version.

- **`InstanceArgs.Volumes`: extra EBS volumes in the launch template**, `/dev/sdf` to `/dev/sdp`, encrypted and deleted with the instance, gp3 by default with optional IOPS, throughput and key. A warm-pool stop and start keeps them. The instance formats and mounts them itself.
- **`InstanceArgs.Types`: several instance types.** The group gets a mixed instances policy (on-demand, prioritized, in the order given) and the launch template names no type. Exactly one of `Type` and `Types`. Auto Scaling does not allow a warm pool with a mixed instances policy, so `Types` with `WarmPool` is refused.
- **`WarmPoolArgs.ReuseOnScaleIn`**: an instance that scales in goes back to the pool, stopped with its volumes, instead of being terminated.
- **`GroupArgs.TerminationPolicies`**, such as `NewestInstance`, to keep the instance with the warm cache when the group scales in.
- **`Contribution.GroupActions`**: Auto Scaling actions a module needs on the box's own group (`autoscaling:EnterStandby`, say). They join the `OwnGroup` statement on the group's ARN, or form it when the box has no hook; a wildcard is refused.
- **New module `modules/ssmcert`**: a certificate, its key and an optional CA bundle from SSM SecureStrings into a directory under `/run` on every boot into service, with the versions it read. A timer compares them with the current versions every hour; on a change, inside a nightly window (02:00 to 05:00 UTC by default), the box enters Standby without decrementing the desired capacity, waits for the replacement and for its own connections to drain, loads the new files, restarts the daemon, waits for its ready check, leaves Standby (completing the launch hook that runs again) and sets the desired capacity back; the termination policy picks which instance leaves. A failed restart terminates only this instance, without changing the desired capacity. `InPlace` restarts without Standby once the port has been idle. The role gets the group actions it needs and `autoscaling:DescribeAutoScalingGroups`; the key's `kms:Decrypt` stays the consumer's statement.
- **`modules/watchdog` honours a pause**: nothing happens before the Unix time in `/run/asg-watchdog/paused-until`, which `ssmcert` writes while it restarts the daemon on purpose.
- The test renderer allows gzip, as a real plan does.

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
