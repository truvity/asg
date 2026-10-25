// Package testutil renders contributions the way a box would receive them and
// runs the generated scripts against fake commands. It is test support.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"
)

// Lifecycle is the lifecycle every rendered test plan uses.
var Lifecycle = &bootstrap.Lifecycle{ASGName: "box", HookName: "box-launch"}

// Rendered is a rendered plan.
type Rendered struct {
	CloudConfig string
	Files       map[string]string
	RunCmd      []string
}

// Render adds the contribution to a plan with a lifecycle and renders it.
func Render(t *testing.T, c module.Contribution) Rendered {
	t.Helper()

	p, err := bootstrap.New(bootstrap.Options{Lifecycle: Lifecycle})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.AddTo(p); err != nil {
		t.Fatal(err)
	}

	r, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}

	var cfg struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
		RunCmd []any `yaml:"runcmd"`
	}

	if err := yaml.Unmarshal([]byte(r.CloudConfig), &cfg); err != nil {
		t.Fatal(err)
	}

	out := Rendered{CloudConfig: r.CloudConfig, Files: map[string]string{}}
	for _, f := range cfg.WriteFiles {
		out.Files[f.Path] = f.Content
	}

	for _, c := range cfg.RunCmd {
		if l, ok := c.([]any); ok && len(l) == 3 {
			out.RunCmd = append(out.RunCmd, l[2].(string))
		}
	}

	return out
}

// CheckSyntax runs bash -n over every script the plan writes or runs.
func CheckSyntax(t *testing.T, r Rendered) {
	t.Helper()

	bash := Bash(t)

	scripts := map[string]string{}

	for path, content := range r.Files {
		first, _, _ := strings.Cut(content, "\n")
		if strings.HasPrefix(first, "#!") && strings.Contains(first, "bash") {
			scripts[path] = content
		}
	}

	for i, s := range r.RunCmd {
		scripts["runcmd "+string(rune('a'+i))] = s
	}

	for name, s := range scripts {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", name, err, out)
		}
	}
}

// Bash returns the path of bash or skips the test.
func Bash(t *testing.T) string {
	t.Helper()

	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}

	return p
}

// Materialize writes the plan's files under dir, mirroring their paths.
func Materialize(t *testing.T, r Rendered, dir string) {
	t.Helper()

	for path, content := range r.Files {
		dest := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(dest, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// FakeBin writes executable scripts named by the map keys into a new
// directory and returns it, for the front of PATH.
func FakeBin(t *testing.T, scripts map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// FakeCurl answers the instance metadata calls from the environment:
// FAKE_STATE is the target lifecycle state ("-" for none).
const FakeCurl = `case "$*" in
  *api/token*) echo tok ;;
  *target-lifecycle-state) [ "${FAKE_STATE:--}" = "-" ] && exit 22; echo "$FAKE_STATE" ;;
  *instance-id) echo i-0abc ;;
  *placement/region) echo eu-west-1 ;;
  *local-ipv4) echo 10.1.2.3 ;;
  *) exit 22 ;;
esac
`

// Run runs bash with args and returns the combined output and the error.
func Run(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()

	cmd := exec.Command(Bash(t), args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()

	return string(out), err
}
