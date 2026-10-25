# Changelog

Entries are written for someone deciding whether to bump: what changed for
them, and for anything breaking, what to do.

## v0.1.0

- **The module `github.com/truvity/asg`, moved from truvity/aws-structure v0.32.0.** The EC2 appliance component is the root package (`asg.New`, formerly `github.com/truvity/aws-structure/pkg/appliance`) and the user-data composer is `github.com/truvity/asg/bootstrap` (formerly `.../pkg/bootstrap`). No behaviour change: the resource sets, rendered user-data and policies match the goldens that moved with them byte for byte, so a consumer that only changes its import paths gets an empty preview.
- ADRs 0001 (bootstrap) and 0002 (the appliance component), carried over from aws-structure's 0003 and 0004.
