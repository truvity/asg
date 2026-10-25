package bootstrap

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Default limits. EC2 allows 16 KiB of raw user-data.
const (
	DefaultLimit     = 16384
	DefaultSoftLimit = 12 * 1024
	DefaultObjectDir = "config"
)

type (
	// Lifecycle names the Auto Scaling lifecycle hook that a failing generated
	// step abandons, so the group replaces the instance.
	Lifecycle struct {
		ASGName  string
		HookName string
	}

	// ObjectStore says where objects the instance fetches are kept. Keys are
	// "<Prefix>/<Dir>/<sha256>.<ext>".
	ObjectStore struct {
		Bucket string
		Prefix string
		// Dir defaults to "config".
		Dir string
	}

	// Options configure a plan.
	Options struct {
		// Lifecycle, when set, makes a failing generated step abandon the
		// hook before it exits.
		Lifecycle *Lifecycle
		// Objects, when set, allows files to be delivered as S3 objects.
		Objects *ObjectStore
		// Limit is the hard limit on the user-data in bytes. Default 16384.
		Limit int
		// SoftLimit is where compression, then spilling to objects, start.
		// Default 12288.
		SoftLimit int
		Gzip      Gzip
	}

	// Plan collects what the fragments add. It is not safe for concurrent use.
	Plan struct {
		opts     Options
		cur      string
		seq      int
		packages map[string]bool
		files    map[string]*fileItem
		unitSet  map[string]string
		ctl      map[string]ctlItem
		steps    []stepItem
		snips    []snippet
		consume  map[string]bool
		startAt  map[string]bool
		params   map[string]bool
	}

	fileItem struct {
		File
		by    string
		bytes []byte
	}

	ctlItem struct{ enable, start bool }

	// snippet is a script that runs on every boot from bootstrap-secrets.
	snippet struct{ name, body string }

	stepItem struct {
		Phase  Phase
		Order  int
		Seq    int
		Name   string
		Script string
	}
)

var (
	pathRe    = regexp.MustCompile(`^/[A-Za-z0-9_./@+-]+$`)
	modeRe    = regexp.MustCompile(`^0?[0-7]{3,4}$`)
	ownerRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+(:[A-Za-z0-9_.-]+)?$`)
	pkgRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9+.:_-]*$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	paramRe   = regexp.MustCompile(`^/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	unitRe    = regexp.MustCompile(`^[A-Za-z0-9_@:.-]+$`)
	envVarRe  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	bucketRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	keyPartRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	hookRe    = regexp.MustCompile(`^[A-Za-z0-9 _.:/@+=,-]+$`)
	oneLineRe = regexp.MustCompile(`^[^\r\n]+$`)
)

// New validates opts and returns an empty plan.
func New(opts Options) (*Plan, error) {
	if opts.Limit == 0 {
		opts.Limit = DefaultLimit
	}
	if opts.SoftLimit == 0 {
		opts.SoftLimit = DefaultSoftLimit
	}
	var errs []error
	if opts.Limit < 0 || opts.SoftLimit < 0 || opts.SoftLimit > opts.Limit {
		errs = append(errs, fmt.Errorf("options: want 0 <= SoftLimit (%d) <= Limit (%d)", opts.SoftLimit, opts.Limit))
	}
	if opts.Gzip < GzipAuto || opts.Gzip > GzipAlways {
		errs = append(errs, fmt.Errorf("options.Gzip %d: unknown", opts.Gzip))
	}
	if l := opts.Lifecycle; l != nil {
		if !hookRe.MatchString(l.ASGName) || !hookRe.MatchString(l.HookName) {
			errs = append(errs, errors.New("options.Lifecycle: ASGName and HookName are required names"))
		}
	}
	if o := opts.Objects; o != nil {
		cp := *o
		if cp.Dir == "" {
			cp.Dir = DefaultObjectDir
		}
		cp.Prefix = strings.Trim(cp.Prefix, "/")
		opts.Objects = &cp
		if !bucketRe.MatchString(cp.Bucket) {
			errs = append(errs, fmt.Errorf("options.Objects.Bucket %q: not a bucket name", cp.Bucket))
		}
		if cp.Prefix != "" && !validKeyPart(cp.Prefix) {
			errs = append(errs, fmt.Errorf("options.Objects.Prefix %q: want letters, digits and _ . - between slashes", cp.Prefix))
		}
		if !validKeyPart(cp.Dir) {
			errs = append(errs, fmt.Errorf("options.Objects.Dir %q: want letters, digits and _ . - between slashes", cp.Dir))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return &Plan{
		opts:     opts,
		packages: map[string]bool{},
		files:    map[string]*fileItem{},
		unitSet:  map[string]string{},
		ctl:      map[string]ctlItem{},
		params:   map[string]bool{},
		consume:  map[string]bool{},
		startAt:  map[string]bool{},
	}, nil
}

func validKeyPart(s string) bool {
	if !keyPartRe.MatchString(s) {
		return false
	}
	for _, el := range strings.Split(s, "/") {
		if el == "." || el == ".." {
			return false
		}
	}

	return true
}

// Add runs the fragment against the plan. Items added before an error stay in
// the plan; discard the plan on error.
func (p *Plan) Add(frags ...Fragment) error {
	var errs []error
	for _, f := range frags {
		if f == nil {
			errs = append(errs, errors.New("nil fragment"))

			continue
		}
		p.cur = f.Name()
		if p.cur == "" {
			errs = append(errs, errors.New("a fragment has no name"))
		}
		if err := f.Add(p); err != nil {
			errs = append(errs, fmt.Errorf("fragment %q: %w", p.cur, err))
		}
		p.cur = ""
	}

	return errors.Join(errs...)
}

// HasLifecycle reports whether Options.Lifecycle is set, that is whether the
// rendered plan completes the launch lifecycle hook with a ready service.
func (p *Plan) HasLifecycle() bool { return p.opts.Lifecycle != nil }

// AddItems adds items directly, as a fragment named "plan".
func (p *Plan) AddItems(items ...Item) error { return p.Add(Compose("plan", items...)) }

// AddSSMParameters records parameter names the plan's scripts read, for the
// instance role's policy, without adding a step. Use it for parameters read
// by a [Step] or a [UnitFragment] script.
func (p *Plan) AddSSMParameters(names ...string) error {
	var errs []error
	for _, n := range names {
		if !paramRe.MatchString(n) || strings.Contains(n, "..") {
			errs = append(errs, fmt.Errorf("ssm parameter %q: want an absolute name like /estate/token", n))

			continue
		}
		p.params[n] = true
	}

	return errors.Join(errs...)
}

func (p *Plan) who() string {
	if p.cur == "" {
		return "plan"
	}

	return p.cur
}

func normMode(m, def string) (string, error) {
	if m == "" {
		return def, nil
	}
	if !modeRe.MatchString(m) {
		return "", fmt.Errorf("mode %q: want octal such as 0644", m)
	}
	if len(m) == 3 {
		m = "0" + m
	}

	return m, nil
}

func checkPath(what, v string) error {
	if !pathRe.MatchString(v) || strings.Contains(v, "//") {
		return fmt.Errorf("%s %q: want an absolute path of letters, digits and _ . / @ + -", what, v)
	}
	for _, el := range strings.Split(v, "/") {
		if el == "." || el == ".." {
			return fmt.Errorf("%s %q: no . or .. elements", what, v)
		}
	}
	if strings.HasSuffix(v, "/") {
		return fmt.Errorf("%s %q: names a directory", what, v)
	}

	return nil
}

func (p *Plan) addPackage(i Package) error {
	if !pkgRe.MatchString(i.Name) {
		return fmt.Errorf("package %q: not a package name", i.Name)
	}
	p.packages[i.Name] = true

	return nil
}

func (p *Plan) addFile(i File) error {
	if err := checkPath("file path", i.Path); err != nil {
		return err
	}
	if strings.HasPrefix(i.Path, "/run/") {
		return fmt.Errorf("file %s: /run is empty after a reboot and write_files runs once; use an SSMSecret, or a PhaseBoot step", i.Path)
	}
	mode, err := normMode(i.Mode, "0644")
	if err != nil {
		return fmt.Errorf("file %s: %w", i.Path, err)
	}
	i.Mode = mode
	if i.Owner == "" {
		i.Owner = "root:root"
	}
	if !ownerRe.MatchString(i.Owner) {
		return fmt.Errorf("file %s: owner %q: want user or user:group", i.Path, i.Owner)
	}
	var raw []byte
	switch i.Encoding {
	case "":
		raw = []byte(i.Content)
	case "base64":
		if raw, err = base64.StdEncoding.DecodeString(i.Content); err != nil {
			return fmt.Errorf("file %s: content is not base64: %w", i.Path, err)
		}
	default:
		return fmt.Errorf("file %s: encoding %q: want \"\" or \"base64\"", i.Path, i.Encoding)
	}
	switch i.Delivery {
	case DeliveryAuto, DeliveryInline:
	case DeliveryObject:
		if p.opts.Objects == nil {
			return fmt.Errorf("file %s: DeliveryObject needs Options.Objects", i.Path)
		}
		if i.Defer {
			return fmt.Errorf("file %s: Defer applies to inline files only", i.Path)
		}
	default:
		return fmt.Errorf("file %s: unknown delivery %d", i.Path, i.Delivery)
	}
	if err := checkNoSecret("file "+i.Path, string(raw)); err != nil {
		return err
	}
	if prev, ok := p.files[i.Path]; ok {
		return fmt.Errorf("path %s is written by both %q and %q", i.Path, prev.by, p.who())
	}
	p.files[i.Path] = &fileItem{File: i, by: p.who(), bytes: raw}

	return nil
}

func checkDownload(what, rawURL, sum string) error {
	var errs []error
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(rawURL, " \t\r\n'\"\\`"):
		errs = append(errs, fmt.Errorf("%s: url %q: want https without credentials or whitespace", what, rawURL))
	case u.RawQuery != "" && strings.Contains(strings.ToLower(u.RawQuery), "signature"):
		errs = append(errs, fmt.Errorf("%s: url carries a signature; a presigned URL expires and is a credential", what))
	}
	if !shaRe.MatchString(sum) {
		errs = append(errs, fmt.Errorf("%s: sha256 %q: want 64 lower-case hex digits", what, sum))
	}

	return errors.Join(errs...)
}

func (p *Plan) addArtifact(i Artifact) error {
	what := "artifact " + i.Dest
	if err := errors.Join(checkPath("artifact dest", i.Dest), checkDownload(what, i.URL, i.SHA256)); err != nil {
		return err
	}
	if strings.HasPrefix(i.Dest, "/run/") {
		return fmt.Errorf("%s: /run is empty after a reboot and an artifact is installed once", what)
	}
	def := "0644"
	if i.Run {
		def = "0755"
	}
	mode, err := normMode(i.Mode, def)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	p.addStepItem(PhaseInstall, -200, "artifact "+i.Dest, p.artifactScript(i.URL, i.SHA256, i.Dest, mode, i.Run))

	return nil
}

func (p *Plan) addBinary(i Binary) error {
	what := "binary " + i.Name
	if !oneLineRe.MatchString(i.Name) {
		return errors.New("binary: a one-line name is required")
	}
	errs := []error{checkPath(what+" dest", i.Dest)}
	if len(i.PerArch) == 0 {
		errs = append(errs, fmt.Errorf("%s: PerArch is empty", what))
	}
	for _, a := range sortedKeys(i.PerArch) {
		if a != "amd64" && a != "arm64" {
			errs = append(errs, fmt.Errorf("%s: architecture %q: want amd64 or arm64", what, a))

			continue
		}
		errs = append(errs, checkDownload(what+" "+a, i.PerArch[a].URL, i.PerArch[a].SHA256))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	p.addStepItem(PhaseInstall, -200, what, p.binaryScript(i))

	return nil
}

func (p *Plan) addUnit(i Unit) error {
	name := i.Name
	if name == "" || !unitRe.MatchString(name) {
		return fmt.Errorf("unit %q: not a unit name", i.Name)
	}
	if !strings.Contains(name, ".") {
		name += ".service"
	}
	if i.Content == "" && (i.DropIn || (!i.Enable && !i.Start)) {
		return fmt.Errorf("unit %s: nothing to write and nothing to enable or start", name)
	}
	if i.Content != "" {
		path := "/etc/systemd/system/" + name
		if i.DropIn {
			path += ".d/10-bootstrap.conf"
		} else if prev, ok := p.unitSet[name]; ok {
			return fmt.Errorf("unit %s is defined by both %q and %q", name, prev, p.who())
		}
		if err := p.addFile(File{Path: path, Content: i.Content, Delivery: DeliveryInline}); err != nil {
			return fmt.Errorf("unit %s: %w", name, err)
		}
		if !i.DropIn {
			p.unitSet[name] = p.who()
		}
	}
	c := p.ctl[name]
	c.enable = c.enable || i.Enable
	c.start = c.start || i.Start
	p.ctl[name] = c

	return nil
}

func (p *Plan) addSSMSecret(i SSMSecret) error {
	var errs []error
	if !paramRe.MatchString(i.Parameter) || strings.Contains(i.Parameter, "..") {
		errs = append(errs, fmt.Errorf("ssm secret %q: want an absolute parameter name like /estate/token", i.Parameter))
	}
	errs = append(errs, checkPath("ssm secret dest", i.Dest))
	if !strings.HasPrefix(i.Dest, "/run/") || strings.Count(i.Dest, "/") < 3 {
		errs = append(errs, fmt.Errorf("ssm secret dest %q: want a file in a directory below /run, which is tmpfs", i.Dest))
	}
	mode := i.Mode
	if mode == "" {
		mode = "0600"
	}
	if mode != "0600" && mode != "0400" {
		errs = append(errs, fmt.Errorf("ssm secret %s: mode %q: want 0600 or 0400", i.Parameter, mode))
	}
	if i.Owner != "" && !ownerRe.MatchString(i.Owner) {
		errs = append(errs, fmt.Errorf("ssm secret %s: owner %q: want user or user:group", i.Parameter, i.Owner))
	}
	switch i.Format {
	case FormatRaw:
		if i.Var != "" {
			errs = append(errs, fmt.Errorf("ssm secret %s: Var applies to FormatEnv", i.Parameter))
		}
	case FormatEnv:
		if !envVarRe.MatchString(i.Var) {
			errs = append(errs, fmt.Errorf("ssm secret %s: Var %q: want an upper-case variable name", i.Parameter, i.Var))
		}
	default:
		errs = append(errs, fmt.Errorf("ssm secret %s: unknown format %d", i.Parameter, i.Format))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	i.Mode = mode
	if i.FirstBootOnly && len(i.Units) > 0 {
		return fmt.Errorf("ssm secret %s: Units need the every-boot loader; drop FirstBootOnly", i.Parameter)
	}
	var units []string
	for _, u := range i.Units {
		n, err := unitName(u)
		if err != nil {
			return fmt.Errorf("ssm secret %s: %w", i.Parameter, err)
		}
		units = append(units, n)
	}
	p.params[i.Parameter] = true
	if i.FirstBootOnly {
		p.addStepItem(PhaseInstall, -100, "secret "+i.Parameter, p.prelude(true)+secretBody(i))

		return nil
	}
	p.addSnippet("secret "+i.Parameter, secretBody(i), units, false)

	return nil
}

func unitName(n string) (string, error) {
	if !unitRe.MatchString(n) {
		return "", fmt.Errorf("unit %q: not a unit name", n)
	}
	if !strings.Contains(n, ".") {
		n += ".service"
	}

	return n, nil
}

// addSnippet registers a script for bootstrap-secrets.service. consumers get
// a drop-in ordering them after it; start says the loader also starts them
// (a unit nothing else starts on a reboot).
func (p *Plan) addSnippet(name, body string, consumers []string, start bool) {
	p.snips = append(p.snips, snippet{name: name, body: body})
	for _, u := range consumers {
		p.consume[u] = true
		if start {
			p.startAt[u] = true
		}
	}
}

func (p *Plan) addStep(i Step) error {
	var errs []error
	if !oneLineRe.MatchString(i.Name) {
		errs = append(errs, errors.New("step: a one-line name is required"))
	}
	if strings.TrimSpace(i.Script) == "" {
		errs = append(errs, fmt.Errorf("step %q: empty script", i.Name))
	}
	if i.Phase != PhaseInstall && i.Phase != PhaseBoot {
		errs = append(errs, fmt.Errorf("step %q: unknown phase %d", i.Name, i.Phase))
	}
	errs = append(errs, checkNoSecret("step "+i.Name, i.Script))
	if err := errors.Join(errs...); err != nil {
		return err
	}
	p.addStepItem(i.Phase, i.Order, i.Name, i.Script)

	return nil
}

func (p *Plan) addStepItem(ph Phase, order int, name, script string) {
	p.seq++
	p.steps = append(p.steps, stepItem{Phase: ph, Order: order, Seq: p.seq, Name: name, Script: script})
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}
