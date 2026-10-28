package datavolume_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/internal/testutil"
	"github.com/truvity/asg/module"
	"github.com/truvity/asg/modules/datavolume"
)

func render(t *testing.T, a datavolume.Args) testutil.Rendered {
	t.Helper()

	c, err := module.Collect(datavolume.New(a))
	if err != nil {
		t.Fatal(err)
	}

	// A single instance has no lifecycle hook.
	return testutil.RenderWith(t, c, bootstrap.Options{})
}

func TestRendersUnitScriptAndDropIns(t *testing.T) {
	r := render(t, datavolume.Args{MountPoint: "/var/lib/cache", Units: []string{"buildkitd.service"}})
	testutil.CheckSyntax(t, r)

	if conf := r.Files["/etc/asg/data-volume.conf"]; !strings.Contains(conf, "DEVICE='/dev/sdf'\n") ||
		!strings.Contains(conf, "MOUNT_POINT='/var/lib/cache'\n") || !strings.Contains(conf, "FS_TYPE='xfs'\n") {
		t.Errorf("conf:\n%s", conf)
	}

	unit := r.Files["/etc/systemd/system/"+datavolume.UnitName]
	if !strings.Contains(unit, "Type=oneshot") || !strings.Contains(unit, "RemainAfterExit=yes") || !strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Errorf("unit:\n%s", unit)
	}

	drop := r.Files["/etc/systemd/system/buildkitd.service.d/10-bootstrap.conf"]
	if !strings.Contains(drop, "Requires=asg-data-volume.service") || !strings.Contains(drop, "After=asg-data-volume.service") {
		t.Errorf("drop-in:\n%s", drop)
	}

	if !strings.Contains(r.CloudConfig, "xfsprogs") {
		t.Error("xfsprogs is not installed")
	}

	if strings.Contains(r.CloudConfig, "complete-lifecycle-action") {
		t.Error("the module must not need a lifecycle hook")
	}
}

func TestRefusals(t *testing.T) {
	for name, a := range map[string]datavolume.Args{
		"no mount point": {},
		"root":           {MountPoint: "/"},
		"relative":       {MountPoint: "data"},
		"traversal":      {MountPoint: "/a/../b"},
		"device":         {MountPoint: "/d", DeviceName: "/dev/xvda"},
		"volume id":      {MountPoint: "/d", VolumeID: "nope"},
		"fs":             {MountPoint: "/d", FSType: "ntfs"},
		"label":          {MountPoint: "/d", Label: "much too long a label"},
		"options":        {MountPoint: "/d", MountOptions: "rw; reboot"},
		"timeout":        {MountPoint: "/d", TimeoutSeconds: 3},
		"unit":           {MountPoint: "/d", Units: []string{"x; reboot"}},
	} {
		if _, err := datavolume.New(a).Contribute(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// box materialises the script with fake commands. blank is the output of
// blkid for the device ("" for a blank one).
type box struct {
	dir, calls string
	vars       []string
}

func setup(t *testing.T, a datavolume.Args, fsOnDevice string, link bool) box {
	t.Helper()

	r := render(t, a)
	dir := t.TempDir()
	testutil.Materialize(t, r, dir)

	devs := filepath.Join(dir, "dev")
	if err := os.MkdirAll(devs, 0o755); err != nil {
		t.Fatal(err)
	}

	if link {
		if err := os.WriteFile(filepath.Join(devs, "sdf"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	calls := filepath.Join(dir, "calls")
	bin := testutil.FakeBin(t, map[string]string{
		"blkid":      `case "$*" in *TYPE*) printf '%s' "$FAKE_FS" ;; *) exit 2 ;; esac; [ -n "$FAKE_FS" ] || exit 2`,
		"lsblk":      `echo disk`,
		"mkfs.xfs":   `echo "mkfs.xfs $*" >>` + calls,
		"mountpoint": `exit 1`,
		"mount":      `echo "mount $*" >>` + calls,
	})

	return box{dir: dir, calls: calls, vars: []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"ASG_DEV_DIR=" + devs,
		"ASG_DATAVOLUME_CONF=" + filepath.Join(dir, "etc/asg/data-volume.conf"),
		"FAKE_FS=" + fsOnDevice,
	}}
}

func (b box) run(t *testing.T) (string, string, error) {
	t.Helper()

	// The mount point lives under the temp dir, not /.
	conf := filepath.Join(b.dir, "etc/asg/data-volume.conf")
	raw, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}

	raw = []byte(strings.ReplaceAll(string(raw), "TIMEOUT_SECONDS=10", "TIMEOUT_SECONDS=2")) // the script's wait, not the unit's
	raw = []byte(strings.ReplaceAll(string(raw), "MOUNT_POINT='/var/lib/cache'", "MOUNT_POINT='"+filepath.Join(b.dir, "mnt")+"'"))
	if err := os.WriteFile(conf, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := testutil.Run(t, b.vars, filepath.Join(b.dir, "usr/local/lib/asg/data-volume.sh"))
	calls, _ := os.ReadFile(b.calls)

	return out, string(calls), err
}

var args = datavolume.Args{MountPoint: "/var/lib/cache", TimeoutSeconds: 10}

func TestBlankVolumeIsFormattedOnceAndMounted(t *testing.T) {
	_, calls, err := setup(t, args, "", true).run(t)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(calls, "mkfs.xfs -L data ") || !strings.Contains(calls, "mount -t xfs -o noatime ") {
		t.Errorf("calls:\n%s", calls)
	}
}

func TestFormattedVolumeIsNeverFormatted(t *testing.T) {
	_, calls, err := setup(t, args, "xfs", true).run(t)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(calls, "mkfs") || !strings.Contains(calls, "mount -t xfs") {
		t.Errorf("calls:\n%s", calls)
	}
}

func TestMissingDeviceFailsAfterTheTimeout(t *testing.T) {
	a := args
	a.TimeoutSeconds = 10

	out, calls, err := setup(t, a, "", false).run(t)
	if err == nil || !strings.Contains(out, "no device for /dev/sdf") || calls != "" {
		t.Errorf("err %v out %q calls %q", err, out, calls)
	}
}
