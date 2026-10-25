# 0003 — Modules, and who owns the on-box lifecycle

**Status:** Accepted

## Context

Decision 0002 (8) left modules for a later change: "No module ships here".
The status box needs four pieces that a second box would repeat: a local
health watchdog, SSH through opkssh, a self-registering DNS record and a
Cloudflare tunnel connector. Each has to run in the right phase. A warm-pool
instance is installed and parked, and must not start a daemon, hold a secret
or hold a tunnel connector that would take traffic it cannot serve. Both
existing boxes completed the launch hook from their own setup script.

## Decision

1. **A Pulumi-free `module` package** defines `Module` and `Contribution`
   (fragments, statements, SSM parameters, object keys, ingress rules, managed
   policies, in-service steps, ready checks, watch checks, drains, scrape
   targets). The root package re-exports them. A provider-neutral half of a
   service can return modules without importing Pulumi, and a module can ship
   in a consumer's repository without a release of this one.
2. **Modules are data, never resources** (0002 point 8, kept). Turning one on
   changes the launch template version and the policy, never the logical
   names of the resources.
3. **The library owns the on-box lifecycle.** In-service steps, ready checks
   and watch checks are written as files under `/usr/local/lib/asg`. One
   oneshot unit, `asg-in-service.service`, reads the target lifecycle state on
   every boot: it does nothing in the warm pool and, going in service, runs
   the steps in order and then the ready checks, each retried until its
   timeout. `bootstrap-ready.service` already waits for the plan's units and
   completes the hook only if they are active, so the launch is completed
   after the checks passed and abandoned when they did not. No consumer calls
   `complete-lifecycle-action` itself. This needs `bootstrap.Options.Lifecycle`,
   and `AddTo` refuses the contribution without it.
4. **The tunnel module takes the connector as a `bootstrap.UnitFragment`**
   copied from truvity/edge's `instanceconnector`. This library neither
   imports edge nor carries a second cloudflared pin. The token load and the
   start are an in-service step, not `RunAuto`, because `RunAuto` loads the
   token and starts the unit on every boot, the warm pool included. The
   metrics listener is switched on with the `TUNNEL_METRICS` environment
   variable in a drop-in, so no change to edge is needed.
5. **ssh takes a rendered bundle, not a library.** The caller renders
   packages, files and commands with whatever it uses (truvity/tailscale's
   hostaccess renders one), so this module does not depend on it.
6. **Drains are data until a terminate hook exists.** `AddTo` refuses a drain
   rather than ignore it.
7. **Refresh triggers are empty by default**: the API starts a refresh on every
   launch template version change when a refresh block exists.
8. **The default component type is `truvity:asg:Box`**, free to rename while
   the only consumer sets `Compat.ComponentType`.

## Roads not taken

- Modules that create resources: enabling one would be a state migration.
- Each module completing the hook: the warm-pool rule would be written four
  times, and a check that failed could not abandon the launch.
- Starting the tunnel with `RunAuto`: a stopped instance resumed from the pool
  would hold a connector before its origin runs.
- Importing edge for the tunnel: every box would pull the Cloudflare provider.

## Consequences

The status box's setup script shrinks to the parts that are its own (the
program, its configuration and its replica). The user-data of a box that adopts
modules changes, so the adoption is a launch template version and an in-place
policy update, as step B of 0002 describes.
