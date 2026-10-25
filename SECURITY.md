# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via
[GitHub Security Advisories](https://github.com/truvity/asg/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

| Version | Supported |
|---------|-----------|
| latest  | yes       |
| older   | no        |

## Design notes relevant to a reviewer

- The component grants an instance only what its user-data names: SSM
  parameters and S3 objects by exact name, and the two Auto Scaling actions
  the lifecycle scripts call, scoped to the group. A bare `*` action is
  refused; anything else is a statement the caller adds and reviews.
- IMDSv2 is always required.
- It holds no credentials. AWS credentials belong to the Pulumi program that
  calls it.
- Tests run against Pulumi mocks and never touch an AWS account, so no test
  credential exists to leak.
