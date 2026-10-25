// Package front puts boxes behind a network load balancer.
//
// An internal NLB has no way to choose a target by name: TCP carries no SNI
// routing and a TLS listener would terminate the client's mutual TLS. The
// listener port is therefore the service's identity, and one NLB serves
// several boxes, each on a port of its own. The package has three entry
// points, split by who owns what:
//
//   - [NewSharedNLB] is the load balancer's own stack: the NLB, its security
//     group and nothing else. It is created with the security group because an
//     NLB created without one can never get one. The security group has no
//     ingress rule; every rule belongs to the box that needs it. The stack
//     publishes [SharedNLB], which a box's stack reads back.
//   - [Attach] is a box's stack: for each [Listener] a target group, a TCP
//     listener on the shared NLB and the ingress rules, scoped to that port,
//     on the NLB's security group. The group registers through
//     asg.GroupArgs.TargetGroupARNs, so no one needs permission to register
//     instances.
//   - [NewNLB] is the same for a box that wants an NLB of its own. It returns
//     the same [Front], so a box moves between a shared and a dedicated NLB
//     without a change to its own resources.
//
// A [Front] hands the box what it needs: [Front.TargetGroupARNs] for the group
// and [Front.TargetIngress] for the box's security group, which admits the
// target and health-check ports from the NLB's security group only. With an
// NLB security group that rule is what lets traffic in, even when client IP
// preservation makes the packets carry the client's address.
//
// The package creates TCP, UDP and TCP_UDP listeners only; a TLS listener is
// refused (passthrough keeps the box's own mutual TLS intact).
package front
