package bootstrap

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Header is the first line of every cloud-config.
const Header = "#cloud-config"

type (
	// Object is a file the caller must upload before the instance boots: put
	// Content at Key in Options.Objects.Bucket. The key is the content's
	// sha256, so an upload never overwrites different bytes.
	Object struct {
		Key     string
		SHA256  string
		Content []byte
	}

	// Rendered is the result of [Plan.Render] or [Verbatim].
	Rendered struct {
		// UserData is what goes in the instance's user-data: the cloud-config
		// text, or its gzip.
		UserData []byte
		// CloudConfig is the text, whether or not UserData is compressed.
		CloudConfig string
		// Gzipped says UserData is compressed.
		Gzipped bool
		// Objects to upload, sorted by key.
		Objects []Object
		// SSMParameters the instance reads, sorted; for the role policy.
		SSMParameters []string
		// Size is len(UserData).
		Size int
		// HasLifecycle says the plan was built with Options.Lifecycle, so the
		// instance completes an Auto Scaling launch hook when it is ready.
		// An appliance without a group (asg.SingleArgs) refuses such a plan.
		HasLifecycle bool
	}

	cloudConfig struct {
		Packages   []string    `yaml:"packages,omitempty"`
		WriteFiles []writeFile `yaml:"write_files,omitempty"`
		BootCmd    []any       `yaml:"bootcmd,omitempty"`
		RunCmd     []any       `yaml:"runcmd,omitempty"`
	}

	writeFile struct {
		Path        string `yaml:"path"`
		Owner       string `yaml:"owner"`
		Permissions string `yaml:"permissions"`
		Encoding    string `yaml:"encoding,omitempty"`
		Defer       bool   `yaml:"defer,omitempty"`
		Content     string `yaml:"content"`
	}
)

// Render composes the cloud-config. It does not change the plan, so it may be
// called again; the same plan renders the same bytes.
func (p *Plan) Render() (*Rendered, error) {
	spilled := map[string]bool{}
	candidates := p.spillOrder()
	for {
		text, objs, err := p.compose(spilled)
		if err != nil {
			return nil, err
		}
		out, gz, err := p.finalize(text)
		if err != nil {
			return nil, err
		}
		if len(out) <= p.opts.SoftLimit || p.opts.Objects == nil || len(candidates) == 0 {
			if len(out) > p.opts.Limit {
				return nil, p.tooBig(len(out), len(text))
			}

			return &Rendered{
				UserData: out, CloudConfig: text, Gzipped: gz, Objects: objs, SSMParameters: sortedKeys(p.params), Size: len(out),
				HasLifecycle: p.opts.Lifecycle != nil,
			}, nil
		}
		spilled[candidates[0]] = true
		candidates = candidates[1:]
	}
}

func (p *Plan) tooBig(size, textSize int) error {
	hint := "set Options.Objects so large files can be fetched from S3"
	if p.opts.Objects != nil {
		hint = "no file is left to move to an object"
	}

	return fmt.Errorf("user-data is %d bytes (cloud-config text %d), over the %d-byte limit; %s", size, textSize, p.opts.Limit, hint)
}

// spillOrder lists the DeliveryAuto files, largest first, then by path.
func (p *Plan) spillOrder() []string {
	var out []string
	for pth, f := range p.files {
		if f.Delivery == DeliveryAuto && len(f.bytes) > 0 {
			out = append(out, pth)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		la, lb := len(p.files[out[a]].bytes), len(p.files[out[b]].bytes)
		if la != lb {
			return la > lb
		}

		return out[a] < out[b]
	})

	return out
}

func (p *Plan) compose(spilled map[string]bool) (string, []Object, error) {
	var cfg cloudConfig
	var err error
	cfg.Packages = sortedKeys(p.packages)

	var wfs []writeFile
	var objs []object
	for _, pth := range sortedKeys(p.files) {
		f := p.files[pth]
		if f.Delivery == DeliveryObject || spilled[pth] {
			sum := sha256.Sum256(f.bytes)
			hexSum := hex.EncodeToString(sum[:])
			objs = append(objs, object{Key: p.objectKey(hexSum, pth), SHA: hexSum, Body: f.bytes, dest: pth, mode: f.Mode, owner: f.Owner})

			continue
		}
		wf := writeFile{Path: pth, Owner: f.Owner, Permissions: f.Mode, Defer: f.Defer, Content: f.Content}
		if f.Encoding == "base64" {
			wf.Encoding = "b64"
		}
		wfs = append(wfs, wf)
	}

	gen, err := p.generatedFiles()
	if err != nil {
		return "", nil, err
	}
	wfs = append(wfs, gen...)
	sort.Slice(wfs, func(a, b int) bool { return wfs[a].Path < wfs[b].Path })
	cfg.WriteFiles = wfs

	steps := append([]stepItem(nil), p.steps...)
	if len(objs) > 0 {
		steps = append(steps, stepItem{Phase: PhaseInstall, Order: -1000, Seq: -2, Name: "fetch objects", Script: p.fetchScript(objs)})
	}
	if hasUnitFiles(p.files) || len(p.snips) > 0 || p.opts.Lifecycle != nil {
		steps = append(steps, stepItem{Phase: PhaseInstall, Order: -50, Seq: -1, Name: "systemd daemon-reload", Script: "systemctl daemon-reload\n"})
	}
	if len(p.snips) > 0 {
		steps = append(steps, stepItem{
			Phase: PhaseInstall, Order: -40, Seq: -1, Name: "load boot-time secrets", Script: "systemctl enable --now " + secretsUnitName + "\n",
		})
	}
	if p.opts.Lifecycle != nil {
		steps = append(steps, stepItem{
			Phase: PhaseInstall, Order: 200, Seq: -1, Name: "report ready", Script: "systemctl enable --now " + readyUnitName + "\n",
		})
	}
	if s := p.unitControl(); s != "" {
		steps = append(steps, stepItem{Phase: PhaseInstall, Order: 100, Seq: -1, Name: "systemd units", Script: s})
	}
	sort.SliceStable(steps, func(a, b int) bool {
		x, y := steps[a], steps[b]
		if x.Phase != y.Phase {
			return x.Phase < y.Phase
		}
		if x.Order != y.Order {
			return x.Order < y.Order
		}

		return x.Seq < y.Seq
	})
	for _, st := range steps {
		cmd := []string{"bash", "-ec", "# " + st.Name + "\n" + st.Script}
		if st.Phase == PhaseBoot {
			cfg.BootCmd = append(cfg.BootCmd, cmd)
		} else {
			cfg.RunCmd = append(cfg.RunCmd, cmd)
		}
	}
	// A failing command must stop the sequence: cloud-init runs the list as
	// one sh script without errexit.
	if len(cfg.BootCmd) > 0 {
		cfg.BootCmd = append([]any{"set -e"}, cfg.BootCmd...)
	}
	if len(cfg.RunCmd) > 0 {
		cfg.RunCmd = append([]any{"set -e"}, cfg.RunCmd...)
	}

	var buf bytes.Buffer
	buf.WriteString(Header + "\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return "", nil, fmt.Errorf("encode cloud-config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", nil, fmt.Errorf("encode cloud-config: %w", err)
	}

	out := make([]Object, 0, len(objs))
	seen := map[string]bool{}
	for _, o := range objs {
		if seen[o.Key] {
			continue
		}
		seen[o.Key] = true
		out = append(out, Object{Key: o.Key, SHA256: o.SHA, Content: o.Body})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Key < out[b].Key })

	return buf.String(), out, nil
}

// generatedFiles are the secret loader, its unit and the consumers'
// drop-ins; a path the caller also writes is a conflict.
func (p *Plan) generatedFiles() ([]writeFile, error) {
	mk := func(path, mode, content string) writeFile {
		return writeFile{Path: path, Owner: "root:root", Permissions: mode, Content: content}
	}
	var out []writeFile
	if len(p.snips) > 0 {
		out = append(out, mk(secretsScriptPath, "0755", p.secretsScript()), mk(secretsUnitPath, "0644", p.secretsUnit()))
		for _, u := range sortedKeys(p.consume) {
			out = append(out, mk("/etc/systemd/system/"+u+".d/"+secretsDropIn, "0644", secretsConsumerDropIn))
		}
	}
	if p.opts.Lifecycle != nil {
		out = append(out, mk(readyScriptPath, "0755", p.readyScript()), mk(readyUnitPath, "0644", p.readyUnit()))
	}
	var errs []error
	for _, f := range out {
		if prev, ok := p.files[f.Path]; ok {
			errs = append(errs, fmt.Errorf("path %s is generated by the composer and also written by %q", f.Path, prev.by))
		}
	}

	return out, errors.Join(errs...)
}

func hasUnitFiles(files map[string]*fileItem) bool {
	for pth := range files {
		if strings.HasPrefix(pth, "/etc/systemd/system/") {
			return true
		}
	}

	return false
}

func (p *Plan) unitControl() string {
	var b strings.Builder
	for _, name := range sortedKeys(p.ctl) {
		c := p.ctl[name]
		switch {
		case c.enable && c.start:
			fmt.Fprintf(&b, "systemctl enable --now %s\n", sq(name))
		case c.enable:
			fmt.Fprintf(&b, "systemctl enable %s\n", sq(name))
		case c.start:
			fmt.Fprintf(&b, "systemctl start %s\n", sq(name))
		}
	}

	return b.String()
}

func (p *Plan) objectKey(sum, filePath string) string {
	o := p.opts.Objects
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(filePath)), ".")
	if ext == "" || len(ext) > 8 || strings.Trim(ext, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
		ext = "bin"
	}
	key := o.Dir + "/" + sum + "." + ext
	if o.Prefix != "" {
		key = o.Prefix + "/" + key
	}

	return key
}

// finalize applies the compression policy to the cloud-config text.
func (p *Plan) finalize(text string) ([]byte, bool, error) {
	switch p.opts.Gzip {
	case GzipNever:
		return []byte(text), false, nil
	case GzipAlways:
		gz, err := gzipBytes(text)

		return gz, true, err
	default:
		if len(text) <= p.opts.SoftLimit {
			return []byte(text), false, nil
		}
		gz, err := gzipBytes(text)
		if err != nil {
			return nil, false, err
		}
		if len(gz) < len(text) {
			return gz, true, nil
		}

		return []byte(text), false, nil
	}
}

func gzipBytes(text string) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if _, err := w.Write([]byte(text)); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}

	return buf.Bytes(), nil
}

// Verbatim carries user-data that is already rendered, byte for byte: no
// header is added, nothing is compressed or reordered. It is the first step
// of adopting the composer, when the output must equal what the machine runs
// today. It applies the secret and hard-size gates and nothing else. limit 0
// means [DefaultLimit].
func Verbatim(userData string, limit int) (*Rendered, error) {
	if limit == 0 {
		limit = DefaultLimit
	}
	var errs []error
	if len(userData) > limit {
		errs = append(errs, fmt.Errorf("user-data is %d bytes, over the %d-byte limit", len(userData), limit))
	}
	errs = append(errs, checkNoSecret("verbatim user-data", userData))
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return &Rendered{UserData: []byte(userData), CloudConfig: userData, Size: len(userData)}, nil
}
