# asg

`github.com/truvity/asg` is a Pulumi Go library for running a service as an
EC2 Auto Scaling group: a machine, or a few, built from user-data and
replaced rather than repaired.

- **`asg.New`** registers the group and its launch template, an optional warm
  pool of stopped instances, a launch lifecycle hook whose default result is
  ABANDON (an instance that never finished booting is replaced, not put in
  service half configured), an IAM role and instance profile with the least
  privilege the user-data needs, and a security group.
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

## Use

```go
import (
    "github.com/truvity/asg"
    "github.com/truvity/asg/bootstrap"
)
```

Build a `bootstrap.Plan`, render it, and pass the result to `asg.New` with
the group, instance, IAM and network arguments. `Compat` lets an existing
group move onto this library without Pulumi replacing anything: it keeps the
component type, the logical names and the layout the group was created with.
See the package documentation and the decisions under `docs/decisions/`.

## Status

`v0.x`: the API may change in a minor release. v0.1.0 is the code that was
`pkg/appliance` and `pkg/bootstrap` in
[truvity/aws-structure](https://github.com/truvity/aws-structure), moved
without a behaviour change.

## Development

```sh
devbox shell
just check      # build, test, lint, leak-canary
```

Tests use Pulumi mocks and compare the registered resources, the rendered
user-data and the policies with goldens; they never reach an AWS account.

## License

MIT, see [LICENSE](LICENSE).
