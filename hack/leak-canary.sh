#!/usr/bin/env bash
# This repository is public and its history cannot be unpublished — a
# rewrite changes the SHAs but not what was already fetched. So the rule
# ("mechanism only; particulars are caller inputs or org variables") is
# enforced mechanically rather than remembered.
#
# Add a pattern here the first time something new turns out to be a
# particular. Never add an exception without one.
set -uo pipefail

# The 12-digit patterns are anchored on word boundaries. Without them,
# `[0-9]{12}` also matches a 12-digit run that happens to fall inside a
# longer hex string -- and a nixpkgs commit SHA is exactly that. The
# devbox bump to 17de0b976395537756f30a3e78f2f06e5cec89ed contains
# `976395537756`, which failed this canary simultaneously in every repo
# that carries it, for a value that is neither a particular nor secret.
# `\b` keeps every real shape (bare, in an ARN, as an ECR host: each is
# bounded by a non-word character) and drops the hex-embedded ones.
#
# The parameter-store pattern excludes `/var/run/secrets/`, which is where
# Kubernetes mounts a pod's own credentials and is therefore in any manifest
# that reads one. A pattern that fires on the single most common path
# convention in the ecosystem does not make anyone safer: it teaches the next
# person to rename their mount to get past it, and the one after that to stop
# reading the output.
#
# Two patterns have a narrow exception, both BY PATH and both only for
# unit tests of the registry parser:
#
#   - `arn:aws`: a permission set attaches AWS-managed policies by ARN, and
#     the parser's tests must write that grammar down (`arn:aws:iam::aws:
#     policy/...` names a policy AWS publishes; it carries no account).
#   - the 12-digit account id: the parser validates account ids, so its
#     tests need values that are twelve digits. They are the documentation
#     placeholders 111122223333 and 444455556666, owned by no one.
#
# There is no exception in this repository: the tests need no ARN or
# account id, so every file, tests included, fails on a literal ARN or a
# twelve-digit run. An exception for a
# placeholder must not become a hiding place for the real value it
# resembles, so a new test directory does not inherit it.
test_allow='^$'

patterns=(
  '\b[0-9]{12}\b'                          # AWS account id
  'arn:aws'                            # any ARN (see test_allow)
  '\b[0-9]{12}\.dkr\.ecr\.'              # ECR registry host
  '\.svc\.cluster\.local'              # in-cluster DNS
  '(?<!/var/run)/secrets/'             # parameter-store paths, but not Kubernetes' own
  'truvity-[a-z0-9-]*-(ci-cache|artifacts|state)'   # S3 buckets
  '\.truvity\.(xyz|com|co)'            # internal hostnames
  'glpat-|ghp_|github_pat_'            # tokens, in case of an accident
)

fail=0

# Scan TRACKED FILES ONLY. The point of this canary is to stop particulars
# being committed, so git's index is exactly the right scope -- and a
# recursive walk of the working tree is not. It descended into generated,
# gitignored directories: .devbox/state.json carries a
# `nix_print_dev_env_hash` whose hex contains a 12-digit run, which matched
# the AWS-account-id pattern. That made the canary fail on a clean checkout
# for a value that is neither committed nor secret.
#
# This matters more than a nuisance: a canary that cries wolf is one people
# learn to skip, and this one is what stands between us and publishing
# particulars from a public repo.
mapfile -d '' tracked < <(git ls-files -z)

for p in "${patterns[@]}"; do
  # Exclude this script: it necessarily contains the patterns it bans.
  allow='^$'
  if [ "$p" = 'arn:aws' ] || [ "$p" = '\b[0-9]{12}\b' ]; then allow="$test_allow"; fi
  if hits=$(printf '%s\0' "${tracked[@]}" \
              | grep -zZv '^hack/leak-canary\.sh$' \
              | grep -zZvP "$allow" \
              | xargs -0 -r grep -InP "$p" 2>/dev/null); then
    echo "LEAK: pattern /$p/ matched — particulars belong in caller inputs or org variables:"
    echo "$hits" | head -5 | sed 's/^/    /'
    fail=1
  fi
done

if [ "$fail" = 0 ]; then
  echo "leak canary clean — ${#patterns[@]} patterns checked, no particulars found"
fi
exit $fail
