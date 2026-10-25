package bootstrap_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/truvity/asg/bootstrap"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newPlan(t *testing.T, o bootstrap.Options) *bootstrap.Plan {
	t.Helper()
	p, err := bootstrap.New(o)
	must(t, err)

	return p
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", name+".golden")
	if *update {
		must(t, os.WriteFile(file, []byte(got), 0o644))
	}
	want, err := os.ReadFile(file)
	must(t, err)
	if string(want) != got {
		t.Errorf("%s differs from the golden (run go test -update to accept):\n%s", name, got)
	}
}

// filler is incompressible-ish text: chained digests in hex.
func filler(n int) string {
	var b strings.Builder
	h := sha256.Sum256([]byte("seed"))
	for b.Len() < n {
		b.WriteString(hex.EncodeToString(h[:]))
		b.WriteByte('\n')
		h = sha256.Sum256(h[:])
	}

	return b.String()[:n]
}

func fullPlan(t *testing.T, o bootstrap.Options) *bootstrap.Plan {
	t.Helper()
	p := newPlan(t, o)
	must(t, p.Add(
		bootstrap.Compose("base",
			bootstrap.Package{Name: "jq"},
			bootstrap.Package{Name: "awscli"},
			bootstrap.File{Path: "/etc/example/app.conf", Content: "listen = :8080\n"},
			bootstrap.File{Path: "/etc/example/tls.b64", Mode: "0640", Owner: "root:app", Encoding: "base64", Content: "aGVsbG8K"},
		),
		bootstrap.Compose("app",
			bootstrap.Package{Name: "jq"},
			bootstrap.Binary{Name: "app", Dest: "/usr/local/bin/app", PerArch: map[string]bootstrap.Source{
				"arm64": {URL: "https://example.com/app-arm64", SHA256: digest},
				"amd64": {URL: "https://example.com/app-amd64", SHA256: strings.Repeat("ab", 32)},
			}},
			bootstrap.Artifact{URL: "https://example.com/tool.sh", SHA256: digest, Dest: "/opt/tool/run.sh", Run: true},
			bootstrap.SSMSecret{
				Parameter: "/example/app/token", Dest: "/run/app/token.env", Format: bootstrap.FormatEnv, Var: "APP_TOKEN", Owner: "app",
				Units: []string{"app"},
			},
			bootstrap.Unit{Name: "app", Content: "[Service]\nExecStart=/usr/local/bin/app\n", Enable: true, Start: true},
			bootstrap.Step{Name: "greet", Order: 10, Script: "echo hello\n"},
			bootstrap.Step{Name: "every boot", Phase: bootstrap.PhaseBoot, Script: "echo boot\n"},
		),
	))

	return p
}

func TestGoldenPlain(t *testing.T) {
	r, err := fullPlan(t, bootstrap.Options{}).Render()
	must(t, err)
	golden(t, "plain", r.CloudConfig)
	if r.Gzipped || string(r.UserData) != r.CloudConfig {
		t.Error("a small plan must not be compressed")
	}
	if got := strings.Join(r.SSMParameters, ","); got != "/example/app/token" {
		t.Errorf("ssm parameters = %q", got)
	}
}

func TestGoldenLifecycle(t *testing.T) {
	p := fullPlan(t, bootstrap.Options{Lifecycle: &bootstrap.Lifecycle{ASGName: "example-asg", HookName: "example-hook"}})
	r, err := p.Render()
	must(t, err)
	golden(t, "lifecycle", r.CloudConfig)
	if !strings.Contains(r.CloudConfig, "--lifecycle-action-result ABANDON") {
		t.Error("no ABANDON in a plan with a lifecycle")
	}
}

func TestGoldenSpill(t *testing.T) {
	o := bootstrap.Options{
		Objects: &bootstrap.ObjectStore{Bucket: "example-config", Prefix: "/appliance/"},
		Limit:   9000, SoftLimit: 5000, Lifecycle: &bootstrap.Lifecycle{ASGName: "example-asg", HookName: "example-hook"},
	}
	p := fullPlan(t, o)
	big := filler(14000)
	must(t, p.AddItems(bootstrap.File{Path: "/etc/example/big.json", Content: big}, bootstrap.File{Path: "/etc/example/small.txt", Content: "small content\n"}))
	r, err := p.Render()
	must(t, err)
	golden(t, "spill", r.CloudConfig)
	if len(r.Objects) != 1 || string(r.Objects[0].Content) != big {
		t.Fatalf("want the big file as the one object, got %d objects", len(r.Objects))
	}
	sum := sha256.Sum256([]byte(big))
	if want := "appliance/config/" + hex.EncodeToString(sum[:]) + ".json"; r.Objects[0].Key != want {
		t.Errorf("key = %q, want %q", r.Objects[0].Key, want)
	}
	if !strings.Contains(r.CloudConfig, "/etc/example/small.txt") || strings.Contains(r.CloudConfig, big[:64]) {
		t.Error("the small file must stay inline and the big one must leave the user-data")
	}
	if r.Size > o.Limit {
		t.Errorf("size %d over the limit", r.Size)
	}
}

func TestExplicitObjectDelivery(t *testing.T) {
	p := newPlan(t, bootstrap.Options{Objects: &bootstrap.ObjectStore{Bucket: "example-config"}})
	must(t, p.AddItems(bootstrap.File{Path: "/etc/x", Content: "x", Delivery: bootstrap.DeliveryObject}))
	r, err := p.Render()
	must(t, err)
	if len(r.Objects) != 1 || !strings.HasPrefix(r.Objects[0].Key, "config/") || !strings.HasSuffix(r.Objects[0].Key, ".bin") {
		t.Errorf("objects = %+v", r.Objects)
	}
	if err := newPlan(t, bootstrap.Options{}).AddItems(bootstrap.File{Path: "/etc/x", Delivery: bootstrap.DeliveryObject}); err == nil {
		t.Error("an object file without an object store must be refused")
	}
}

func TestGzip(t *testing.T) {
	p := newPlan(t, bootstrap.Options{})
	must(t, p.AddItems(bootstrap.File{Path: "/etc/example/log.txt", Content: strings.Repeat("a line of text\n", 1500)}))
	r, err := p.Render()
	must(t, err)
	golden(t, "gzip-plain", r.CloudConfig)
	if !r.Gzipped || !bytes.HasPrefix(r.UserData, []byte{0x1f, 0x8b}) {
		t.Fatal("a plan over the soft limit must be gzipped")
	}
	zr, err := gzip.NewReader(bytes.NewReader(r.UserData))
	must(t, err)
	back, err := io.ReadAll(zr)
	must(t, err)
	if string(back) != r.CloudConfig {
		t.Error("gunzip does not give back the cloud-config")
	}
	if len(r.CloudConfig) <= bootstrap.DefaultLimit || r.Size > bootstrap.DefaultLimit {
		t.Errorf("want text over and compressed under the limit, got %d and %d", len(r.CloudConfig), r.Size)
	}

	never := newPlan(t, bootstrap.Options{Gzip: bootstrap.GzipNever})
	must(t, never.AddItems(bootstrap.File{Path: "/etc/example/log.txt", Content: strings.Repeat("a line of text\n", 1500)}))
	if _, err := never.Render(); err == nil {
		t.Error("over the limit with GzipNever and no objects must fail")
	}

	always := fullPlan(t, bootstrap.Options{Gzip: bootstrap.GzipAlways})
	ra, err := always.Render()
	must(t, err)
	if !ra.Gzipped {
		t.Error("GzipAlways did not compress")
	}
}

func TestOverLimit(t *testing.T) {
	p := newPlan(t, bootstrap.Options{Objects: &bootstrap.ObjectStore{Bucket: "example-config"}, Limit: 600, SoftLimit: 300})
	must(t, p.AddItems(bootstrap.File{Path: "/etc/a", Content: filler(2000), Delivery: bootstrap.DeliveryInline}))
	if _, err := p.Render(); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("an inline file over the limit must fail, got %v", err)
	}
}

func TestDeterministic(t *testing.T) {
	for i := 0; i < 20; i++ {
		a, err := fullPlan(t, bootstrap.Options{}).Render()
		must(t, err)
		b, err := fullPlan(t, bootstrap.Options{}).Render()
		must(t, err)
		if !bytes.Equal(a.UserData, b.UserData) {
			t.Fatal("two renders of the same plan differ")
		}
		again, err := fullPlan(t, bootstrap.Options{}).Render()
		must(t, err)
		if !bytes.Equal(a.UserData, again.UserData) {
			t.Fatal("a third render differs")
		}
	}
	p := fullPlan(t, bootstrap.Options{})
	a, _ := p.Render()
	b, _ := p.Render()
	if !bytes.Equal(a.UserData, b.UserData) {
		t.Error("rendering the same plan twice differs")
	}
}

func TestConflicts(t *testing.T) {
	cases := map[string][]bootstrap.Fragment{
		"path": {
			bootstrap.Compose("one", bootstrap.File{Path: "/etc/x", Content: "a"}),
			bootstrap.Compose("two", bootstrap.File{Path: "/etc/x", Content: "a"}),
		},
		"unit": {
			bootstrap.Compose("one", bootstrap.Unit{Name: "svc", Content: "[Service]\n"}),
			bootstrap.Compose("two", bootstrap.Unit{Name: "svc.service", Content: "[Service]\n"}),
		},
		"unit versus file": {
			bootstrap.Compose("one", bootstrap.Unit{Name: "svc", Content: "[Service]\n"}),
			bootstrap.Compose("two", bootstrap.File{Path: "/etc/systemd/system/svc.service", Content: "x"}),
		},
		"drop-in twice": {
			bootstrap.Compose("one", bootstrap.Unit{Name: "svc", DropIn: true, Content: "[Service]\n"}),
			bootstrap.Compose("two", bootstrap.Unit{Name: "svc", DropIn: true, Content: "[Service]\n"}),
		},
	}
	for name, frags := range cases {
		err := newPlan(t, bootstrap.Options{}).Add(frags...)
		if err == nil || !strings.Contains(err.Error(), `"one"`) || !strings.Contains(err.Error(), `"two"`) {
			t.Errorf("%s: want a conflict naming both fragments, got %v", name, err)
		}
	}
	p := newPlan(t, bootstrap.Options{})
	must(t, p.Add(
		bootstrap.Compose("one", bootstrap.Unit{Name: "svc", Content: "[Service]\n"}),
		bootstrap.Compose("two", bootstrap.Unit{Name: "svc", DropIn: true, Content: "[Service]\n"}),
		bootstrap.Compose("three", bootstrap.Package{Name: "jq"}),
		bootstrap.Compose("four", bootstrap.Package{Name: "jq"}),
	))
}

func TestRefusals(t *testing.T) {
	mips := map[string]bootstrap.Source{"mips": {URL: "https://example.com/x", SHA256: digest}}
	bad := map[string]bootstrap.Item{
		"relative path":         bootstrap.File{Path: "etc/x"},
		"dotdot":                bootstrap.File{Path: "/etc/../x"},
		"bad mode":              bootstrap.File{Path: "/etc/x", Mode: "rw"},
		"bad base64":            bootstrap.File{Path: "/etc/x", Encoding: "base64", Content: "!!"},
		"unknown encoding":      bootstrap.File{Path: "/etc/x", Encoding: "gz"},
		"http artifact":         bootstrap.Artifact{URL: "http://example.com/x", SHA256: digest, Dest: "/x"},
		"credentials in url":    bootstrap.Artifact{URL: "https://user:pw@example.com/x", SHA256: digest, Dest: "/x"},
		"presigned url":         bootstrap.Artifact{URL: "https://example.com/x?X-Amz-Signature=1", SHA256: digest, Dest: "/x"},
		"no digest":             bootstrap.Artifact{URL: "https://example.com/x", Dest: "/x"},
		"upper-case digest":     bootstrap.Artifact{URL: "https://example.com/x", SHA256: strings.ToUpper(digest), Dest: "/x"},
		"binary without builds": bootstrap.Binary{Name: "x", Dest: "/x"},
		"binary arch":           bootstrap.Binary{Name: "x", Dest: "/x", PerArch: mips},
		"secret outside /run":   bootstrap.SSMSecret{Parameter: "/example/p", Dest: "/etc/p"},
		"secret in /run itself": bootstrap.SSMSecret{Parameter: "/example/p", Dest: "/run/p"},
		"secret mode":           bootstrap.SSMSecret{Parameter: "/example/p", Dest: "/run/x/p", Mode: "0644"},
		"secret param":          bootstrap.SSMSecret{Parameter: "relative", Dest: "/run/x/p"},
		"env without var":       bootstrap.SSMSecret{Parameter: "/example/p", Dest: "/run/x/p", Format: bootstrap.FormatEnv},
		"raw with var":          bootstrap.SSMSecret{Parameter: "/example/p", Dest: "/run/x/p", Var: "X"},
		"empty step":            bootstrap.Step{Name: "x"},
		"multi-line step name":  bootstrap.Step{Name: "x\ny", Script: "true"},
		"package":               bootstrap.Package{Name: "Bad Name"},
		"empty unit":            bootstrap.Unit{Name: "x"},
	}
	for name, it := range bad {
		if err := newPlan(t, bootstrap.Options{}).AddItems(it); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	for name, o := range map[string]bootstrap.Options{
		"soft over hard": {Limit: 100, SoftLimit: 200},
		"bucket":         {Objects: &bootstrap.ObjectStore{Bucket: "Bad_Bucket"}},
		"prefix":         {Objects: &bootstrap.ObjectStore{Bucket: "example-config", Prefix: "a/../b"}},
		"lifecycle":      {Lifecycle: &bootstrap.Lifecycle{ASGName: "only"}},
	} {
		if _, err := bootstrap.New(o); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
}

func TestNoSecretsAnywhere(t *testing.T) {
	// Built from pieces so this file holds no token-shaped literal.
	secrets := map[string]string{
		"private key":    "-----BEGIN " + "RSA PRIVATE KEY-----\nabc\n",
		"aws access key": "AK" + "IA" + strings.Repeat("A", 16),
		"github token":   "gh" + "p_" + strings.Repeat("a", 36),
		"slack token":    "xo" + "xb-" + "1234567890-abcdef",
		"aws secret":     "aws_secret_access_key = " + strings.Repeat("a", 40),
	}
	for name, s := range secrets {
		for where, it := range map[string]bootstrap.Item{
			"file":     bootstrap.File{Path: "/etc/x", Content: s},
			"b64 file": bootstrap.File{Path: "/etc/x", Encoding: "base64", Content: b64(s)},
			"step":     bootstrap.Step{Name: "x", Script: "echo " + s},
			"unit":     bootstrap.Unit{Name: "x", Content: s},
		} {
			if err := newPlan(t, bootstrap.Options{}).AddItems(it); err == nil {
				t.Errorf("%s in a %s: want a refusal", name, where)
			} else if strings.Contains(err.Error(), s) {
				t.Errorf("%s in a %s: the error repeats the secret", name, where)
			}
		}
		if err := newPlan(t, bootstrap.Options{}).Add(bootstrap.UnitFragment{LoadToken: s}.Fragment("x", bootstrap.RunAuto)); err == nil {
			t.Errorf("%s in a unit fragment: want a refusal", name)
		}
		if _, err := bootstrap.Verbatim("#cloud-config\n# "+s+"\n", 0); err == nil {
			t.Errorf("%s in verbatim user-data: want a refusal", name)
		}
	}
}

func TestSecretValueNeverInOutput(t *testing.T) {
	r, err := fullPlan(t, bootstrap.Options{}).Render()
	must(t, err)
	for _, want := range []string{"--with-decryption", "/example/app/token", "install -d -m 0700", "umask 077", "chmod 0600"} {
		if !strings.Contains(r.CloudConfig, want) {
			t.Errorf("secret step lacks %q", want)
		}
	}
	for _, bad := range []string{"set -x", "echo \"$v\"", "echo $v"} {
		if strings.Contains(r.CloudConfig, bad) {
			t.Errorf("a script that can print the secret: %q", bad)
		}
	}
}

func TestUnitFragment(t *testing.T) {
	f := bootstrap.UnitFragment{
		Install:       "install_it() { true; }\ninstall_it\n",
		Unit:          "[Service]\nExecStart=/usr/local/bin/conn\n",
		UnitPath:      "/etc/systemd/system/conn.service",
		LoadToken:     "umask 077\ntrue\n",
		Start:         "systemctl daemon-reload\nsystemctl start conn\n",
		SSMParameters: []string{"/example/conn/token", "/example/a"},
	}
	auto := newPlan(t, bootstrap.Options{})
	must(t, auto.Add(f.Fragment("conn", bootstrap.RunAuto)))
	ra, err := auto.Render()
	must(t, err)
	golden(t, "unitfragment-auto", ra.CloudConfig)
	if got := strings.Join(ra.SSMParameters, ","); got != "/example/a,/example/conn/token" {
		t.Errorf("parameters = %q", got)
	}
	doc := parse(t, ra.CloudConfig)
	runcmd := strings.Join(cmdScripts(doc["runcmd"]), "\n")
	loader := fileContent(t, doc, "/usr/local/lib/bootstrap/secrets.sh")
	if !strings.Contains(runcmd, "conn install") || !strings.Contains(runcmd, "conn start") || strings.Contains(runcmd, "umask 077") {
		t.Error("install and start run on the first boot; the token load must not")
	}
	if !strings.Contains(loader, "umask 077") || !strings.Contains(loader, "systemctl start --no-block 'conn.service'") {
		t.Errorf("the loader must load the token and start the unit on every boot:\n%s", loader)
	}
	if d := fileContent(t, doc, "/etc/systemd/system/conn.service.d/20-bootstrap-secrets.conf"); !strings.Contains(d, "Requires=bootstrap-secrets.service") {
		t.Errorf("drop-in = %q", d)
	}

	manual := newPlan(t, bootstrap.Options{})
	must(t, manual.Add(f.Fragment("conn", bootstrap.RunManual)))
	rm, err := manual.Render()
	must(t, err)
	if strings.Contains(rm.CloudConfig, "conn start") || strings.Contains(rm.CloudConfig, "bootstrap-secrets") ||
		!strings.Contains(rm.CloudConfig, "conn install") {
		t.Error("manual mode must install and write the unit only")
	}
	if len(rm.SSMParameters) != 2 {
		t.Error("manual mode must still report the parameters")
	}

	odd := f
	odd.UnitPath = "/opt/conn.service"
	if err := newPlan(t, bootstrap.Options{}).Add(odd.Fragment("x", bootstrap.RunAuto)); err == nil {
		t.Error("a token loader for a unit outside /etc/systemd/system must be refused")
	}
	if err := newPlan(t, bootstrap.Options{}).Add(bootstrap.UnitFragment{Unit: "[Service]\n"}.Fragment("x", bootstrap.RunAuto)); err == nil {
		t.Error("a unit without a path must be refused")
	}
}

func TestVerbatim(t *testing.T) {
	in := "#!/bin/bash\r\necho  odd   spacing\n\n\ttabs and no trailing newline"
	r, err := bootstrap.Verbatim(in, 0)
	must(t, err)
	if string(r.UserData) != in || r.Gzipped || len(r.Objects) != 0 || r.Size != len(in) {
		t.Error("verbatim must carry the text unchanged")
	}
	if _, err := bootstrap.Verbatim(strings.Repeat("x", 100), 50); err == nil {
		t.Error("verbatim over the limit must fail")
	}
}

func parse(t *testing.T, text string) map[string]any {
	t.Helper()
	var doc map[string]any
	must(t, yaml.Unmarshal([]byte(text), &doc))

	return doc
}

// cmdScripts returns the script of each bash -ec entry of a command list.
func cmdScripts(list any) []string {
	var out []string
	cmds, _ := list.([]any)
	for _, c := range cmds {
		if argv, ok := c.([]any); ok {
			out = append(out, argv[2].(string))
		}
	}

	return out
}

func fileContent(t *testing.T, doc map[string]any, path string) string {
	t.Helper()
	for _, f := range doc["write_files"].([]any) {
		if m := f.(map[string]any); m["path"] == path {
			return m["content"].(string)
		}
	}
	t.Fatalf("no write_files entry for %s", path)

	return ""
}

func b64(s string) string {
	var b bytes.Buffer
	enc := base64Encoder(&b)
	_, _ = enc.Write([]byte(s))
	_ = enc.Close()

	return b.String()
}

// allowlisted keys of the cloud-config modules the composer emits.
var (
	topKeys  = map[string]bool{"packages": true, "write_files": true, "bootcmd": true, "runcmd": true}
	fileKeys = map[string]bool{"path": true, "owner": true, "permissions": true, "encoding": true, "defer": true, "content": true}
)

func TestCloudConfigShape(t *testing.T) {
	_, haveBash := exec.LookPath("bash")
	opts := bootstrap.Options{
		Objects: &bootstrap.ObjectStore{Bucket: "example-config", Prefix: "appliance"},
		Limit:   9000, SoftLimit: 5000, Lifecycle: &bootstrap.Lifecycle{ASGName: "example-asg", HookName: "example-hook"},
	}
	for name, p := range map[string]*bootstrap.Plan{"plain": fullPlan(t, bootstrap.Options{}), "spill": fullPlan(t, opts)} {
		if name == "spill" {
			must(t, p.AddItems(bootstrap.File{Path: "/etc/example/big.json", Content: filler(14000)}))
		}
		r, err := p.Render()
		must(t, err)
		if !strings.HasPrefix(r.CloudConfig, "#cloud-config\n") {
			t.Fatalf("%s: no #cloud-config header", name)
		}
		var doc map[string]any
		must(t, yaml.Unmarshal([]byte(r.CloudConfig), &doc))
		for k := range doc {
			if !topKeys[k] {
				t.Errorf("%s: unexpected top-level key %q", name, k)
			}
		}
		for _, f := range doc["write_files"].([]any) {
			for k := range f.(map[string]any) {
				if !fileKeys[k] {
					t.Errorf("%s: unexpected write_files key %q", name, k)
				}
			}
		}
		if haveBash == nil {
			sh := fileContent(t, doc, "/usr/local/lib/bootstrap/secrets.sh")
			if out, err := exec.Command("bash", "-n", "-c", sh).CombinedOutput(); err != nil {
				t.Errorf("%s: secrets.sh does not parse: %v\n%s", name, err, out)
			}
		}
		for _, list := range []string{"bootcmd", "runcmd"} {
			cmds, _ := doc[list].([]any)
			if len(cmds) > 0 && cmds[0] != "set -e" {
				t.Errorf("%s: %s does not start with set -e", name, list)
			}
			for _, c := range cmds[min(1, len(cmds)):] {
				argv, ok := c.([]any)
				if !ok || len(argv) != 3 || argv[0] != "bash" {
					t.Fatalf("%s: %s entry %v is not a bash -ec triple", name, list, c)
				}
				if haveBash == nil {
					out, err := exec.Command("bash", "-n", "-c", argv[2].(string)).CombinedOutput()
					if err != nil {
						t.Errorf("%s: %s script does not parse: %v\n%s\n%s", name, list, err, out, argv[2])
					}
				}
			}
		}
	}
}

func base64Encoder(w io.Writer) io.WriteCloser { return base64.NewEncoder(base64.StdEncoding, w) }

func TestSecretsLoadOnEveryBoot(t *testing.T) {
	lc := &bootstrap.Lifecycle{ASGName: "example-asg", HookName: "example-hook"}
	doc := parse(t, mustRender(t, fullPlan(t, bootstrap.Options{Lifecycle: lc})).CloudConfig)

	// The default is every boot: nothing that reads SSM runs in the first-boot list.
	for _, list := range []string{"runcmd", "bootcmd"} {
		for _, s := range cmdScripts(doc[list]) {
			if strings.Contains(s, "get-parameter") {
				t.Errorf("a secret is read in %s, which does not run again after a stop and start:\n%s", list, s)
			}
		}
	}
	loader := fileContent(t, doc, "/usr/local/lib/bootstrap/secrets.sh")
	for _, want := range []string{
		"aws ssm get-parameter", "--with-decryption", "sleep $((i * 3))",
		"--lifecycle-action-result ABANDON", "set-instance-health", "--health-status Unhealthy",
	} {
		if !strings.Contains(loader, want) {
			t.Errorf("loader lacks %q", want)
		}
	}
	// Abandon is tried first and the health call is only its fallback: no marker decides.
	if ab, hl := strings.Index(loader, "ABANDON"), strings.Index(loader, "set-instance-health"); ab < 0 || hl < ab ||
		!strings.Contains(loader, "if ! aws autoscaling complete-lifecycle-action") || strings.Contains(loader, "secrets-loaded") {
		t.Errorf("failure path must try ABANDON, then fall back to unhealthy:\n%s", loader)
	}
	unit := fileContent(t, doc, "/etc/systemd/system/bootstrap-secrets.service")
	for _, want := range []string{
		"Type=oneshot", "RemainAfterExit=yes", "After=network-online.target", "Wants=network-online.target", "Before=app.service", "WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	d := fileContent(t, doc, "/etc/systemd/system/app.service.d/20-bootstrap-secrets.conf")
	if !strings.Contains(d, "Requires=bootstrap-secrets.service") || !strings.Contains(d, "After=bootstrap-secrets.service") {
		t.Errorf("consumer drop-in = %q", d)
	}
	if !strings.Contains(strings.Join(cmdScripts(doc["runcmd"]), "\n"), "systemctl enable --now bootstrap-secrets.service") {
		t.Error("the first boot must enable and start the loader")
	}
	// Without a lifecycle the loader does not call the Auto Scaling API.
	plain := fileContent(t, parse(t, mustRender(t, fullPlan(t, bootstrap.Options{})).CloudConfig), "/usr/local/lib/bootstrap/secrets.sh")
	if strings.Contains(plain, "autoscaling") {
		t.Error("autoscaling calls without Options.Lifecycle")
	}
}

func TestFirstBootOnlyIsExplicit(t *testing.T) {
	p := newPlan(t, bootstrap.Options{})
	must(t, p.AddItems(bootstrap.SSMSecret{Parameter: "/example/once", Dest: "/run/once/token", FirstBootOnly: true}))
	r := mustRender(t, p)
	doc := parse(t, r.CloudConfig)
	if !strings.Contains(strings.Join(cmdScripts(doc["runcmd"]), "\n"), "get-parameter") || strings.Contains(r.CloudConfig, "bootstrap-secrets") {
		t.Error("FirstBootOnly reads in the first-boot steps and builds no loader")
	}
	q := newPlan(t, bootstrap.Options{})
	if err := q.AddItems(bootstrap.SSMSecret{Parameter: "/example/p", Dest: "/run/x/p", FirstBootOnly: true, Units: []string{"x"}}); err == nil {
		t.Error("a unit cannot depend on a first-boot-only secret")
	}
}

func TestNothingUnderRunIsWrittenOnce(t *testing.T) {
	for name, it := range map[string]bootstrap.Item{
		"file":     bootstrap.File{Path: "/run/app/token", Content: "x"},
		"artifact": bootstrap.Artifact{URL: "https://example.com/x", SHA256: digest, Dest: "/run/app/x"},
	} {
		if err := newPlan(t, bootstrap.Options{}).AddItems(it); err == nil {
			t.Errorf("%s under /run: want a refusal, the directory is empty after a reboot", name)
		}
	}
}

func TestGeneratedPathCollision(t *testing.T) {
	p := fullPlan(t, bootstrap.Options{})
	must(t, p.AddItems(bootstrap.File{Path: "/usr/local/lib/bootstrap/secrets.sh", Content: "x"}))
	if _, err := p.Render(); err == nil {
		t.Error("a file at the loader's path must be refused")
	}
}

func mustRender(t *testing.T, p *bootstrap.Plan) *bootstrap.Rendered {
	t.Helper()
	r, err := p.Render()
	must(t, err)

	return r
}

func TestReadyUnit(t *testing.T) {
	lc := &bootstrap.Lifecycle{ASGName: "example-asg", HookName: "example-hook"}
	doc := parse(t, mustRender(t, fullPlan(t, bootstrap.Options{Lifecycle: lc})).CloudConfig)
	unit := fileContent(t, doc, "/etc/systemd/system/bootstrap-ready.service")
	for _, want := range []string{
		"Type=oneshot", "RemainAfterExit=yes", "WantedBy=multi-user.target",
		"Wants=bootstrap-secrets.service app.service", "After=network-online.target bootstrap-secrets.service app.service",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("ready unit lacks %q:\n%s", want, unit)
		}
	}
	script := fileContent(t, doc, "/usr/local/lib/bootstrap/ready.sh")
	for _, want := range []string{
		"systemctl is-active --quiet 'app.service' || bs_abort", "--lifecycle-action-result CONTINUE",
		`echo "bootstrap: CONTINUE attempt $i: $out" >&2`, "DefaultResult applies",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("ready script lacks %q:\n%s", want, script)
		}
	}
	// A failed CONTINUE is never fatal: no abort after the loop, and the script ends in exit 0.
	_, tail, _ := strings.Cut(script, "for i in 1 2 3")
	if strings.Contains(tail, "bs_abort") || strings.Contains(script, "No active Lifecycle Action") || !strings.HasSuffix(tail, "exit 0\n") {
		t.Errorf("CONTINUE must be logged and never fatal:\n%s", tail)
	}
	if !strings.Contains(strings.Join(cmdScripts(doc["runcmd"]), "\n"), "systemctl enable --now bootstrap-ready.service") {
		t.Error("the first boot must enable and start the ready unit")
	}

	// No lifecycle, no ready unit.
	text := mustRender(t, fullPlan(t, bootstrap.Options{})).CloudConfig
	if strings.Contains(text, "bootstrap-ready") {
		t.Error("a ready unit without Options.Lifecycle")
	}
	// A plan with only a lifecycle still reports ready.
	p := newPlan(t, bootstrap.Options{Lifecycle: lc})
	solo := parse(t, mustRender(t, p).CloudConfig)
	if u := fileContent(t, solo, "/etc/systemd/system/bootstrap-ready.service"); strings.Contains(u, "Wants=") {
		t.Errorf("an empty plan has nothing to wait for:\n%s", u)
	}
	// The unit of a UnitFragment is waited for, too.
	uf := bootstrap.UnitFragment{Unit: "[Service]\n", UnitPath: "/etc/systemd/system/conn.service", LoadToken: "true\n", Start: "true\n"}
	q := newPlan(t, bootstrap.Options{Lifecycle: lc})
	must(t, q.Add(uf.Fragment("conn", bootstrap.RunAuto)))
	if u := fileContent(t, parse(t, mustRender(t, q).CloudConfig), "/etc/systemd/system/bootstrap-ready.service"); !strings.Contains(u, "conn.service") {
		t.Errorf("ready unit does not wait for the connector unit:\n%s", u)
	}
	// The generated paths are reserved.
	r := newPlan(t, bootstrap.Options{Lifecycle: lc})
	must(t, r.AddItems(bootstrap.File{Path: "/usr/local/lib/bootstrap/ready.sh", Content: "x"}))
	if _, err := r.Render(); err == nil {
		t.Error("a file at the ready script's path must be refused")
	}
}

// TestSystemdVerify gives the generated units and drop-ins a real parser:
// systemd-analyze verify, over a root that holds the units and stub
// executables. Skipped without the binary.
func TestSystemdVerify(t *testing.T) {
	bin, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not installed")
	}
	lc := &bootstrap.Lifecycle{ASGName: "example-asg", HookName: "example-hook"}
	doc := parse(t, mustRender(t, fullPlan(t, bootstrap.Options{Lifecycle: lc})).CloudConfig)
	root := t.TempDir()
	var units []string
	for _, f := range doc["write_files"].([]any) {
		m := f.(map[string]any)
		p := m["path"].(string)
		full := filepath.Join(root, p)
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		must(t, os.WriteFile(full, []byte(m["content"].(string)), mode))
		if strings.HasSuffix(p, ".service") {
			units = append(units, p)
		}
	}
	// The base targets (sysinit.target, network-online.target) come from the host.
	sys := filepath.Join(root, "usr/lib/systemd")
	must(t, os.MkdirAll(sys, 0o755))
	must(t, os.Symlink("/usr/lib/systemd/system", filepath.Join(sys, "system")))
	stub := filepath.Join(root, "usr/local/bin/app")
	must(t, os.MkdirAll(filepath.Dir(stub), 0o755))
	must(t, os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755))
	if len(units) != 3 {
		t.Fatalf("want app, secrets and ready units, got %v", units)
	}
	out, err := exec.Command(bin, append([]string{"verify", "--root=" + root}, units...)...).CombinedOutput()
	if err != nil {
		t.Errorf("systemd-analyze verify: %v\n%s", err, out)
	}
}
