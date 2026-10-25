# Contributing

Thanks for looking. This repository holds a Pulumi Go library for EC2 Auto
Scaling groups built from user-data.

## Before you open a pull request

```sh
devbox shell
just check      # build, test, lint, leak-canary
```

`just check` is exactly what CI runs on a pull request. `just vuln` runs on
a schedule in its own workflow.

## What belongs here

Mechanism. Anything that would differ between two users (an account id, a
region, a name, a policy statement) is an input and never a default.
`hack/leak-canary.sh` catches the mechanical cases; review catches the rest.

## Rules of the road

- A change to what a component registers updates its goldens in the same
  pull request, and the pull request says why the diff is intended.
- Moving an existing group onto a new version must not replace its group,
  role, security group or instance profile: use `Compat`, never a rename.
- Tests never reach an AWS account.
