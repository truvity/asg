package bootstrap

import (
	"fmt"
	"strings"
)

// sq quotes s for a POSIX shell.
func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// prelude defines bs_die (print, abandon the lifecycle action when one is
// configured, exit 1) and, when the step or the hook needs the instance
// metadata, imds (an IMDSv2 lookup).
func (p *Plan) prelude(needIMDS bool) string {
	var b strings.Builder
	if needIMDS || p.opts.Lifecycle != nil {
		b.WriteString(`imds() {
  local t
  t="$(curl -fsS --connect-timeout 5 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 300' http://169.254.169.254/latest/api/token)" || return 1
  curl -fsS --connect-timeout 5 -H "X-aws-ec2-metadata-token: $t" "http://169.254.169.254/latest/meta-data/$1"
}
`)
	}
	b.WriteString("bs_die() {\n  echo \"bootstrap: $1\" >&2\n")
	if l := p.opts.Lifecycle; l != nil {
		fmt.Fprintf(&b, `  aws autoscaling complete-lifecycle-action --region "$(imds placement/region)" \
    --auto-scaling-group-name %s --lifecycle-hook-name %s \
    --lifecycle-action-result ABANDON --instance-id "$(imds instance-id)" >/dev/null 2>&1 || true
`, sq(l.ASGName), sq(l.HookName))
	}
	b.WriteString("  exit 1\n}\n")

	return b.String()
}

// download is the digest-checked download loop over the shell variables url,
// sum, dest and mode.
const download = `tmp="$(mktemp)" || bs_die "mktemp failed"
trap 'rm -f "$tmp"' EXIT
ok=0
for i in 1 2 3 4 5; do
  if curl -fsSL --connect-timeout 10 --max-time 300 -o "$tmp" "$url" \
    && printf '%s  %s\n' "$sum" "$tmp" | sha256sum -c - >/dev/null 2>&1; then
    ok=1
    break
  fi
  echo "bootstrap: download of $url failed or did not match its sha256 (attempt $i)" >&2
  sleep 3
done
[ "$ok" = 1 ] || bs_die "giving up on $url"
install -D -m "$mode" "$tmp" "$dest" || bs_die "cannot install $dest"
`

func (p *Plan) artifactScript(u, sum, dest, mode string, run bool) string {
	s := p.prelude(false) + fmt.Sprintf("url=%s sum=%s dest=%s mode=%s\n", sq(u), sq(sum), sq(dest), mode) + download
	if run {
		s += "\"$dest\" || bs_die \"$dest failed\"\n"
	}

	return s
}

func (p *Plan) binaryScript(b Binary) string {
	var s strings.Builder
	s.WriteString(p.prelude(false))
	s.WriteString(`case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) bs_die "unsupported architecture $(uname -m)" ;;
esac
case "$arch" in
`)
	for _, a := range sortedKeys(b.PerArch) {
		fmt.Fprintf(&s, "  %s) url=%s sum=%s ;;\n", a, sq(b.PerArch[a].URL), sq(b.PerArch[a].SHA256))
	}
	fmt.Fprintf(&s, "  *) bs_die %s ;;\nesac\n", sq("no build of "+b.Name+" for this architecture"))
	fmt.Fprintf(&s, "dest=%s mode=0755\n", sq(b.Dest))
	s.WriteString(download)

	return s.String()
}

// secretBody reads the parameter into i.Dest. It needs imds and bs_die.
func secretBody(i SSMSecret) string {
	var s strings.Builder
	fmt.Fprintf(&s, `param=%s dest=%s
umask 077
region="$(imds placement/region)" || bs_die "no region from the instance metadata"
install -d -m 0700 "$(dirname "$dest")" || bs_die "cannot create the directory of $dest"
tmp="$(mktemp "$(dirname "$dest")/.XXXXXX")" || bs_die "mktemp failed"
trap 'rm -f "$tmp"' EXIT
v=""
for i in 1 2 3 4 5; do
  if v="$(aws ssm get-parameter --region "$region" --name "$param" --with-decryption --query Parameter.Value --output text)" && [ -n "$v" ]; then
    break
  fi
  v=""
  sleep $((i * 3))
done
[ -n "$v" ] || bs_die "cannot read $param"
`, sq(i.Parameter), sq(i.Dest))
	switch i.Format {
	case FormatEnv:
		s.WriteString(`v=${v//\\/\\\\}
v=${v//\"/\\\"}
`)
		fmt.Fprintf(&s, "printf '%s=\"%%s\"\\n' \"$v\" > \"$tmp\"\n", i.Var)
	default:
		s.WriteString("printf '%s' \"$v\" > \"$tmp\"\n")
	}
	s.WriteString("v=\"\"\n")
	fmt.Fprintf(&s, "chmod %s \"$tmp\"\n", i.Mode)
	if i.Owner != "" {
		fmt.Fprintf(&s, "chown %s \"$tmp\"\n", sq(i.Owner))
	}
	s.WriteString("mv -f \"$tmp\" \"$dest\"\n")

	return s.String()
}

// Paths of the generated boot-time files.
const (
	secretsUnitName   = "bootstrap-secrets.service"
	secretsUnitPath   = "/etc/systemd/system/" + secretsUnitName
	secretsScriptPath = "/usr/local/lib/bootstrap/secrets.sh"
	secretsDropIn     = "20-bootstrap-secrets.conf"
	readyUnitName     = "bootstrap-ready.service"
	readyUnitPath     = "/etc/systemd/system/" + readyUnitName
	readyScriptPath   = "/usr/local/lib/bootstrap/ready.sh"
)

// bootLib is the head of the every-boot scripts: imds, bs_die (exported, for
// the bash -e blocks) and bs_abort. bs_abort first tries to ABANDON the
// pending launch lifecycle action, which has the group replace the instance.
// Auto Scaling runs the launch hook on a first boot into the group and, with
// a warm pool, again when the instance leaves the pool, so an action is
// pending on most boots that matter; when none is (a plain reboot), the call
// is refused and the instance is reported unhealthy instead.
func (p *Plan) bootLib() string {
	var s strings.Builder
	s.WriteString(`set -e
imds() {
  local t
  t="$(curl -fsS --connect-timeout 5 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 300' http://169.254.169.254/latest/api/token)" || return 1
  curl -fsS --connect-timeout 5 -H "X-aws-ec2-metadata-token: $t" "http://169.254.169.254/latest/meta-data/$1"
}
bs_die() {
  echo "bootstrap: $1" >&2
  exit 1
}
export -f imds bs_die
bs_abort() {
  echo "bootstrap: $1" >&2
`)
	if l := p.opts.Lifecycle; l != nil {
		fmt.Fprintf(&s, `  if ! aws autoscaling complete-lifecycle-action --region "$(imds placement/region)" \
    --auto-scaling-group-name %s --lifecycle-hook-name %s \
    --lifecycle-action-result ABANDON --instance-id "$(imds instance-id)" >/dev/null 2>&1; then
    aws autoscaling set-instance-health --region "$(imds placement/region)" \
      --instance-id "$(imds instance-id)" --health-status Unhealthy >/dev/null 2>&1 || true
  fi
`, sq(l.ASGName), sq(l.HookName))
	}
	s.WriteString("  exit 1\n}\n")

	return s.String()
}

// secretsScript is run on every boot by bootstrap-secrets.service. Each
// snippet runs in its own bash -e; a failure ends the script through
// bs_abort.
func (p *Plan) secretsScript() string {
	var s strings.Builder
	s.WriteString("#!/bin/bash\n# Generated by github.com/truvity/asg/bootstrap; run by bootstrap-secrets.service on every boot.\n")
	s.WriteString(p.bootLib())
	for n, sn := range p.snips {
		fmt.Fprintf(&s, "bash -e <<'BOOTSTRAP_EOF_%d' || bs_abort %s\n%sBOOTSTRAP_EOF_%d\n", n, sq(sn.name+" failed"), sn.body, n)
	}
	for _, u := range sortedKeys(p.startAt) {
		fmt.Fprintf(&s, "systemctl start --no-block %s\n", sq(u))
	}

	return s.String()
}

func (p *Plan) secretsUnit() string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Load boot-time secrets from SSM Parameter Store\nWants=network-online.target\nAfter=network-online.target\n")
	if c := sortedKeys(p.consume); len(c) > 0 {
		b.WriteString("Before=" + strings.Join(c, " ") + "\n")
	}
	b.WriteString("\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=" + secretsScriptPath + "\n\n[Install]\nWantedBy=multi-user.target\n")

	return b.String()
}

// readyUnits are the units the plan starts or enables: the ones that must be
// active before the instance reports ready.
func (p *Plan) readyUnits() []string {
	set := map[string]bool{}
	for u := range p.consume {
		set[u] = true
	}
	for u := range p.startAt {
		set[u] = true
	}
	for u, c := range p.ctl {
		if c.enable || c.start {
			set[u] = true
		}
	}

	return sortedKeys(set)
}

// readyScript completes the launch lifecycle action with CONTINUE once every
// unit of the plan is active. Sending it is best effort: a failure (no action
// pending on a plain reboot, or any other error) is logged and the script
// exits 0, so a missed CONTINUE on a real action falls to the hook's
// DefaultResult. It runs on every boot from bootstrap-ready.service.
func (p *Plan) readyScript() string {
	l := p.opts.Lifecycle
	var s strings.Builder
	s.WriteString("#!/bin/bash\n# Generated by github.com/truvity/asg/bootstrap; run by bootstrap-ready.service on every boot.\n")
	s.WriteString(p.bootLib())
	for _, u := range p.readyUnits() {
		fmt.Fprintf(&s, "systemctl is-active --quiet %[1]s || bs_abort %[2]s\n", sq(u), sq(u+" is not active"))
	}
	fmt.Fprintf(&s, `for i in 1 2 3; do
  if out="$(aws autoscaling complete-lifecycle-action --region "$(imds placement/region)" \
    --auto-scaling-group-name %s --lifecycle-hook-name %s \
    --lifecycle-action-result CONTINUE --instance-id "$(imds instance-id)" 2>&1)"; then
    exit 0
  fi
  echo "bootstrap: CONTINUE attempt $i: $out" >&2
  sleep $((i * 3))
done
echo "bootstrap: no CONTINUE sent; if an action was pending, the hook's DefaultResult applies" >&2
exit 0
`, sq(l.ASGName), sq(l.HookName))

	return s.String()
}

// readyUnit follows the secret loader and the plan's units. Wants= pulls the
// units in and After= holds the script until their start jobs have finished;
// the script then checks they are active, so a unit that failed is never
// reported ready.
func (p *Plan) readyUnit() string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Tell the Auto Scaling group this instance is ready\n")
	deps := p.readyUnits()
	if len(p.snips) > 0 {
		deps = append([]string{secretsUnitName}, deps...)
	}
	after := append([]string{"network-online.target"}, deps...)
	if len(deps) > 0 {
		b.WriteString("Wants=" + strings.Join(deps, " ") + "\n")
	}
	b.WriteString("After=" + strings.Join(after, " ") + "\n")
	b.WriteString("\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=" + readyScriptPath + "\n\n[Install]\nWantedBy=multi-user.target\n")

	return b.String()
}

const secretsConsumerDropIn = "[Unit]\nRequires=" + secretsUnitName + "\nAfter=" + secretsUnitName + "\n"

// object is a file delivered through S3.
type object struct {
	Key  string
	SHA  string
	Body []byte
	// dest, mode and owner are the installed file.
	dest, mode, owner string
}

func (p *Plan) fetchScript(objs []object) string {
	o := p.opts.Objects
	var s strings.Builder
	s.WriteString(p.prelude(true))
	fmt.Fprintf(&s, `bucket=%s
region="$(imds placement/region)" || bs_die "no region from the instance metadata"
bs_fetch() {
  local key="$1" sum="$2" dest="$3" mode="$4" owner="$5" tmp i
  tmp="$(mktemp)" || return 1
  for i in 1 2 3 4 5; do
    if aws s3 cp --only-show-errors --region "$region" "s3://$bucket/$key" "$tmp" \
      && printf '%%s  %%s\n' "$sum" "$tmp" | sha256sum -c - >/dev/null 2>&1; then
      install -D -m "$mode" "$tmp" "$dest" && chown "$owner" "$dest" && rm -f "$tmp" && return 0
      break
    fi
    sleep 3
  done
  rm -f "$tmp"
  return 1
}
`, sq(o.Bucket))
	for _, ob := range objs {
		fmt.Fprintf(&s, "bs_fetch %s %s %s %s %s || bs_die %s\n",
			sq(ob.Key), sq(ob.SHA), sq(ob.dest), ob.mode, sq(ob.owner), sq("cannot fetch "+ob.dest))
	}

	return s.String()
}
