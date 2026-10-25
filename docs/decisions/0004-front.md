# 0004 — Front: boxes behind a shared network load balancer

**Status:** Accepted

## Context

A box whose clients are inside the VPC (a builder, a worker reached over SSH)
needs a stable address and a health-checked set of targets. Giving each box an
NLB of its own costs an NLB per box and architecture. An NLB cannot choose a
target by name: TCP carries no SNI routing, and a TLS listener terminates the
client's TLS, which breaks a box that checks client certificates itself. So
one NLB can serve several boxes only if each takes a port of its own.

## Decision

1. **The port is the service's identity.** One NLB per environment and
   account is shared by several Auto Scaling groups, one TCP listener per
   service. The ports are held in a registry by the caller, with a test that
   they are unique; this package refuses a duplicate within one call and AWS
   refuses one at apply (`DuplicateListener`). TLS listeners are refused.
2. **Two stacks, one relationship each.** `NewSharedNLB` is the NLB's own
   stack: the NLB, its security group with egress to the VPC only, and no
   ingress. `Attach` is the box's stack: its target group, its listener and
   the ingress rules for its port on the NLB's security group. Adding or
   removing a box never touches the NLB's stack. The group registers through
   `GroupArgs.TargetGroupARNs`, so no one needs permission to register
   instances, and the Auto Scaling service-linked role does it.
3. **The security group exists from the start.** An NLB created without one
   can never get one (`InvalidConfigurationRequest: You cannot set security
   groups on a Network Load Balancer which was created without any security
   groups`). One created with a group can have its set of groups replaced
   later, but never emptied.
4. **Only the NLB's group reaches the targets.** The box's own security group
   admits the target and health-check ports from the NLB's security group and
   from nothing else (`Front.TargetIngress`). With client IP preservation on,
   the target sees the client's real address, and the rule still matches: AWS
   evaluates the target's inbound rules against the NLB's group, not against
   the client's address. A client cannot reach the box around the NLB.
5. **Rules are separate resources.** Boxes in other stacks add rules to the
   NLB's group, so the group declares none inline, and each rule is an
   `aws.vpc` rule resource named after its listener.
6. **Registration waits for the listener.** `Front.TargetGroupARNs` resolves
   only after the listener exists; Auto Scaling refuses a target group that no
   listener serves.
7. **Deletion runs boxes first.** Deletion protection is on by default,
   because deleting an NLB silently deletes every listener on it. The NLB's
   security group cannot be deleted while a box's group still references it.
8. **A dedicated NLB is the same type.** `NewNLB` returns the same `Front`, so
   a box moves between a shared and a dedicated NLB without a change to its
   own resources.
9. **Target groups have generated names.** A replacement (a changed port or
   protocol) is created beside the old one and cannot reuse a fixed name.

## Consequences

- Clients learn the port from configuration, not DNS.
- The NLB's security group egress is the whole VPC range, not port by port:
  two boxes may share a target port, and two identical egress rules would
  collide. The target-side rule is the port fence.
- Weighted forwarding between target groups (blue/green across groups) is not
  offered yet.
- At most 50 listeners per NLB (a quota AWS does not raise).
