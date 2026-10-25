# 0005 — Certificate rotation through Standby

**Status:** Accepted

## Context

Some daemons load their TLS certificate once, at start, and never again
(BuildKit's `buildkitd` is one). A renewed certificate then needs a restart,
and a restart is not free: a daemon with a large local cache can take minutes
to load its index, and a box that is the only one of its group serves nothing
meanwhile. Replacing the instance on every renewal would throw the cache away.

The certificate is issued elsewhere and written to SSM Parameter Store; the
box reads it into `/run` (tmpfs) when it goes in service.

## Decision

1. **`modules/ssmcert` owns the delivery and the rotation.** On every boot
   into service it reads the certificate, the key and an optional CA bundle
   (decrypted) into a directory under `/run`, owned by the daemon's user, and
   records the parameter versions it read. A timer compares those versions
   with the current ones (no decryption needed) every hour.
2. **A rotation runs only in a nightly window** (02:00 to 05:00 UTC by
   default), and only while the group is quiet: every other instance in
   service, as many in service as desired, and room for one more below the
   maximum. Otherwise the next check tries again; a 30-day renewal margin
   gives about 90 attempts.
3. **Standby keeps the group serving.** The box enters Standby without
   decrementing the desired capacity, so the group puts a replacement in
   service (from the warm pool when there is one) and the load balancer
   drains this instance. The box waits for the replacement and for its own
   connections to end, loads the new files, restarts the daemon and waits for
   its ready check. It then exits Standby, completes the launch hook that
   ExitStandby runs again, and sets the desired capacity back to the value it
   read; the group's termination policy picks which instance leaves
   (`NewestInstance` keeps the one with the warm cache, and a warm pool with
   `ReuseOnScaleIn` takes the replacement back).
4. **A failed restart costs only this instance.** The box terminates itself
   without changing the desired capacity; the replacement keeps serving.
5. **The watchdog is paused** while the daemon restarts on purpose
   (`/run/asg-watchdog/paused-until`).
6. **`Contribution.GroupActions`** carries the Auto Scaling actions a module
   needs on its own group, so a Pulumi-free module never needs the account,
   region or group name to build an ARN. The appliance adds them to the
   `OwnGroup` statement.

## Consequences

- The role may enter and exit Standby, set the desired capacity and terminate
  instances of its own group, and describe groups (`*`, the only resource the
  describe call takes). These are actions on the box's own group only; the
  group ARN cannot be narrowed to the calling instance.
- The group needs a maximum above its desired capacity.
- A box without a warm pool still rotates; the replacement is a cold launch
  and the wait for it is bounded (20 minutes by default).
- `InPlace` is the alternative for a daemon that restarts quickly: it waits
  for the port to be idle inside the window and restarts without Standby.
- The key's `kms:Decrypt` stays with the consumer, which knows the key.
