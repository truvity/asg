# asg

`github.com/truvity/asg` is a Pulumi Go library for running a service as an
EC2 Auto Scaling group: a machine, or a few, built from user-data and
replaced rather than repaired.

- **`asg.New`** registers the group and its launch template, an optional warm
  pool of stopped instances, a launch lifecycle hook whose default result is
  ABANDON (an instance that never finished booting is replaced, not put in
  service half configured), an IAM role and instance profile with the least
  privilege the user-data needs, and a security group.
- **`module`** and **`modules/`** are the extension mechanism: a module
  returns data (user-data fragments, IAM statements, SSM parameter names,
  ingress rules, the steps and checks of the on-box lifecycle) and never a
  resource, so turning one on changes the launch template and the policy, not
  the set of resources. Shipped: `watchdog` (replace an instance whose local
  checks keep failing), `ssh` (opkssh and a fixed host key), `selfregister`
  (a DNS record for the in-service instance) and `tunnel` (a Cloudflare
  tunnel connector that starts only in service).
- **`bootstrap`** composes the user-data from fragments: systemd units,
  files, the secrets an instance reads at boot (SSM parameters and S3
  objects, by exact name) and the readiness step that completes the
  lifecycle hook. The IAM policy is derived from it, so the instance can
  read exactly what its user-data names and nothing else.

The rules the component enforces, whatever the caller passes:

- IMDSv2 is always required;
- a bare `*` action is refused;
- no managed policy is attached by default;
- a warm pool without a lifecycle hook is refused;
- the image is pinned at deploy time, so a newer image is a visible diff.

## Who it is for

Anyone running a service on EC2 the replace-don't-repair way with Pulumi in
Go: a stateless box, or a small fleet, whose whole configuration is its
user-data, behind a load balancer, a tunnel or nothing at all.

## The model

One call, `asg.New`, registers an Auto Scaling group and everything it needs
as children of one component. The user-data is a `bootstrap.Plan`: files,
units and the secrets the instance reads at boot, by exact name. The IAM
policy is derived from the plan, so an instance can read what its user-data
names and nothing else. A launch lifecycle hook with a default of ABANDON
means an instance that never finished booting is replaced, never put in
service half configured.

## Install and a worked example

```sh
go get github.com/truvity/asg
```

```go
import (
    "github.com/truvity/asg"
    "github.com/truvity/asg/bootstrap"
)
```

Build a `bootstrap.Plan`, render it, and pass the result to `asg.New` with
the group, instance, IAM and network arguments. The package documentation
and `composed_test.go` show a complete box made of a plan and modules. `Compat` lets an existing group
move onto this library without Pulumi replacing anything: it keeps the
component type, the logical names and the layout the group was created with.

## Consumers

The status page box in truvity/observability (`pkg/statusbox/ec2`).

## Neighbours

[truvity/aws-structure](https://github.com/truvity/aws-structure) shapes the
AWS organization this runs in; the code here used to live there as
`pkg/appliance` and `pkg/bootstrap`.

## Documentation

The package documentation, and the decisions under
[`docs/decisions/`](docs/decisions/).

## The rule that makes this repository public

Mechanism only. Account ids, regions, names, images and policy statements
are the caller's; nothing here defaults to one estate's value, and the tests
use Pulumi mocks and never reach an AWS account.
[`hack/leak-canary.sh`](hack/leak-canary.sh) enforces the mechanical half
over tracked files and runs in the gate, with no exception.

## Status

`v0.x`: the API may change in a minor release, and every change is in the
[CHANGELOG](CHANGELOG.md). v0.1.0 is the code that was `pkg/appliance` and
`pkg/bootstrap` in truvity/aws-structure, moved without a behaviour change;
v0.2.0 adds the `module` package and the four modules above, and changes two
defaults (see the CHANGELOG).

## Development

```sh
devbox shell
just check      # build, test, lint, leak-canary
```

## Releasing

Releases are a `v*` tag, which the shared release workflow turns into the Go
module version and a GitHub Release. Automatic patch releases are not armed;
releases are hand-cut tags.

## Licence

MIT, see [LICENSE](LICENSE).
