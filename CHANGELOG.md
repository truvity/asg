# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
follows [Semantic Versioning](https://semver.org/); until v1 a minor
release may change the API.

## [Unreleased]

## [v0.1.0] - 2026-10-25

### Added

- The module `github.com/truvity/asg`: the EC2 appliance component (`asg.New`,
  formerly `github.com/truvity/aws-structure/pkg/appliance`) and the
  user-data composer (`github.com/truvity/asg/bootstrap`, formerly
  `github.com/truvity/aws-structure/pkg/bootstrap`), moved from
  truvity/aws-structure v0.32.0 without a behaviour change. The resource
  sets, the rendered user-data and the policies are byte-for-byte the same
  (the goldens under `testdata/` moved with them), so a consumer that only
  changes its import paths gets an empty preview.
- ADRs 0001 (bootstrap) and 0002 (the appliance component), carried over
  from aws-structure's ADRs 0003 and 0004.

[Unreleased]: https://github.com/truvity/asg/compare/v0.1.0...HEAD
[v0.1.0]: https://github.com/truvity/asg/releases/tag/v0.1.0
