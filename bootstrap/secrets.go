package bootstrap

import (
	"errors"
	"fmt"
	"regexp"
)

// secretPatterns are high-confidence shapes of credentials. They are a
// tripwire for the accident (a pasted key, a token in a script), not a proof
// of absence: the supported way to give an instance a secret is an
// [SSMSecret]. The expressions are written so this file does not itself
// contain a token-shaped literal.
var secretPatterns = []struct {
	what string
	re   *regexp.Regexp
}{
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"an AWS access key id", regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{"an AWS secret access key", regexp.MustCompile(`(?i)aws_secret_access_key["']?\s*[:=]\s*["']?[A-Za-z0-9/+]{20,}`)},
	{"a GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`)},
	{"a GitHub token", regexp.MustCompile(`\bgithub_pa[t]_[A-Za-z0-9_]{30,}`)},
	{"a GitLab token", regexp.MustCompile(`\bglpa[t]-[A-Za-z0-9_-]{20,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
}

// checkNoSecret returns an error naming where when text looks like a
// credential. The matched text is never part of the message.
func checkNoSecret(where, text string) error {
	var errs []error
	for _, s := range secretPatterns {
		if s.re.MatchString(text) {
			errs = append(errs, fmt.Errorf("%s contains what looks like %s; user-data is readable by anything on the instance and by "+
				"whoever can describe it: use an SSMSecret", where, s.what))
		}
	}

	return errors.Join(errs...)
}
