// Package datavolume is a module that formats (once) and mounts a persistent
// EBS data volume, the one asg.Args.Data attaches to a single instance, before
// the services that use it start.
//
// The volume is attached after the instance exists and by a resource of its
// own, so the box may boot before the device does; and a stop and start, or a
// replacement of the instance, brings the same volume back. The module
// therefore writes a systemd oneshot unit, asg-data-volume.service, that
// runs on EVERY boot and does three things, each safe to repeat:
//
//  1. wait until the device is there (up to TimeoutSeconds);
//  2. make a file system on it only if the device has no signature at all
//     (a volume that was formatted is never touched, and a device that
//     carries a partition table is refused, not wiped);
//  3. mount it at MountPoint.
//
// The device is found by the name it was attached under (asg.DataVolumeArgs
// DeviceName, default /dev/sdf), because the volume id is an output of the
// deploy and is not known when the user-data is rendered. On a Nitro instance
// the volume appears as /dev/nvmeXn1; the box maps it back to the name with
// the udev links of the Amazon Linux image, then with ebsnvme-id, and with
// VolumeID set (when the caller knows the id) by the volume's serial number.
//
// The module needs no IAM statement. It does not use the launch lifecycle, so
// it composes with a plan rendered without bootstrap.Options.Lifecycle, as
// asg.SingleArgs requires.
package datavolume

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"
)

// Defaults.
const (
	DefaultDeviceName     = "/dev/sdf"
	DefaultFSType         = "xfs"
	DefaultMountOptions   = "noatime"
	DefaultTimeoutSeconds = 600

	// UnitName is the oneshot unit. A service that needs the volume orders
	// itself after it (Args.Units does that).
	UnitName = "asg-data-volume.service"
)

const (
	scriptPath = module.LibDir + "/data-volume.sh"
	confPath   = "/etc/asg/data-volume.conf"
)

var (
	deviceRe  = regexp.MustCompile(`^/dev/sd[f-p]$`)
	pathRe    = regexp.MustCompile(`^/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	volumeRe  = regexp.MustCompile(`^vol-[0-9a-f]{8,17}$`)
	optionsRe = regexp.MustCompile(`^[a-z0-9_=,.:-]+$`)
	labelRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,12}$`)
	unitRe    = regexp.MustCompile(`^[A-Za-z0-9_@:.-]+\.service$`)
)

// Args configure the module.
type Args struct {
	// MountPoint is an absolute directory, created when missing. Required.
	MountPoint string
	// DeviceName is the name the volume is attached under, /dev/sdf to
	// /dev/sdp: asg.DataVolumeArgs.DeviceName. Default /dev/sdf.
	DeviceName string
	// VolumeID, when set, finds the device by the volume's serial number
	// instead of by DeviceName (Nitro instances only).
	VolumeID string
	// FSType is xfs (default) or ext4.
	FSType string
	// Label is the file system label, at most 12 characters. Default "data".
	Label string
	// MountOptions are the mount options. Default "noatime".
	MountOptions string
	// TimeoutSeconds is how long to wait for the device after boot. Default
	// 600. The unit fails when the device never appears, and so do the
	// Units that require it.
	TimeoutSeconds int
	// Units are service names (such as buildkitd.service) that require the
	// mount: each gets a drop-in with Requires= and After= the data volume
	// unit. The drop-in is the bootstrap one, so these units must not have
	// another bootstrap drop-in.
	Units []string
}

type mod struct{ a Args }

// New returns the data volume module.
func New(a Args) module.Module { return mod{a: a} }

func (mod) Name() string { return "datavolume" }

func (a Args) withDefaults() Args {
	if a.DeviceName == "" {
		a.DeviceName = DefaultDeviceName
	}

	if a.FSType == "" {
		a.FSType = DefaultFSType
	}

	if a.Label == "" {
		a.Label = "data"
	}

	if a.MountOptions == "" {
		a.MountOptions = DefaultMountOptions
	}

	if a.TimeoutSeconds == 0 {
		a.TimeoutSeconds = DefaultTimeoutSeconds
	}

	return a
}

func (a Args) validate() error {
	var errs []error

	if !pathRe.MatchString(a.MountPoint) || strings.Contains(a.MountPoint, "..") || a.MountPoint == "/" {
		errs = append(errs, fmt.Errorf("MountPoint %q: want an absolute directory such as /var/lib/cache", a.MountPoint))
	}

	if !deviceRe.MatchString(a.DeviceName) {
		errs = append(errs, fmt.Errorf("DeviceName %q: want /dev/sdf to /dev/sdp", a.DeviceName))
	}

	if a.VolumeID != "" && !volumeRe.MatchString(a.VolumeID) {
		errs = append(errs, fmt.Errorf("VolumeID %q: want vol-<hex>", a.VolumeID))
	}

	if a.FSType != "xfs" && a.FSType != "ext4" {
		errs = append(errs, fmt.Errorf("FSType %q: want xfs or ext4", a.FSType))
	}

	if !labelRe.MatchString(a.Label) {
		errs = append(errs, fmt.Errorf("label %q: want 1-12 of letters, digits, _ and -", a.Label))
	}

	if !optionsRe.MatchString(a.MountOptions) {
		errs = append(errs, fmt.Errorf("MountOptions %q: want comma separated mount options", a.MountOptions))
	}

	if a.TimeoutSeconds < 10 {
		errs = append(errs, fmt.Errorf("TimeoutSeconds %d: want at least 10", a.TimeoutSeconds))
	}

	for _, u := range a.Units {
		if !unitRe.MatchString(u) {
			errs = append(errs, fmt.Errorf("units entry %q: want a service name such as buildkitd.service", u))
		}
	}

	return errors.Join(errs...)
}

func (m mod) Contribute() (module.Contribution, error) {
	a := m.a.withDefaults()
	if err := a.validate(); err != nil {
		return module.Contribution{}, err
	}

	conf := fmt.Sprintf("# Rendered by github.com/truvity/asg/modules/datavolume. No secret.\n"+
		"DEVICE=%s\nVOLUME_ID=%s\nMOUNT_POINT=%s\nFS_TYPE=%s\nLABEL=%s\nMOUNT_OPTIONS=%s\nTIMEOUT_SECONDS=%d\n",
		module.ShellQuote(a.DeviceName), module.ShellQuote(a.VolumeID), module.ShellQuote(a.MountPoint),
		module.ShellQuote(a.FSType), module.ShellQuote(a.Label), module.ShellQuote(a.MountOptions), a.TimeoutSeconds)

	// The unit must not be stopped early at shutdown by a mount it did not
	// make: RemainAfterExit keeps it "active" for the services that require it.
	unit := "[Unit]\nDescription=Wait for, format once and mount the persistent data volume\n" +
		"After=local-fs.target\n\n" +
		fmt.Sprintf("[Service]\nType=oneshot\nRemainAfterExit=yes\nTimeoutStartSec=%d\nExecStart=%s\n\n[Install]\nWantedBy=multi-user.target\n",
			a.TimeoutSeconds+60, scriptPath)

	items := []bootstrap.Item{
		bootstrap.Package{Name: map[string]string{"xfs": "xfsprogs", "ext4": "e2fsprogs"}[a.FSType]},
		bootstrap.File{Path: scriptPath, Mode: "0755", Delivery: bootstrap.DeliveryInline, Content: script},
		bootstrap.File{Path: confPath, Mode: "0644", Delivery: bootstrap.DeliveryInline, Content: conf},
		bootstrap.Unit{Name: UnitName, Content: unit, Enable: true, Start: true},
	}

	for _, u := range a.Units {
		items = append(items, bootstrap.Unit{
			Name: u, DropIn: true,
			Content: "[Unit]\nRequires=" + UnitName + "\nAfter=" + UnitName + "\n",
		})
	}

	return module.Contribution{Fragments: []bootstrap.Fragment{bootstrap.Compose("datavolume", items...)}}, nil
}

const script = `#!/bin/bash
# asg-data-volume: wait for the persistent data volume, make a file system on
# it only if it has no signature, and mount it. Runs on every boot and is safe
# to repeat. Generated by github.com/truvity/asg/modules/datavolume.
set -euo pipefail

DEV_DIR="${ASG_DEV_DIR:-/dev}"
# shellcheck disable=SC1090
. "${ASG_DATAVOLUME_CONF:-/etc/asg/data-volume.conf}"

log() { printf 'asg-data-volume: %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

name="${DEVICE#/dev/}"
serial="${VOLUME_ID//-/}"

# find_device prints the block device the volume is attached as.
find_device() {
  local d m
  if [ -n "$serial" ]; then
    for d in "$DEV_DIR"/nvme*n1; do
      [ -e "$d" ] || continue
      [ "$(lsblk -dno SERIAL "$d" 2>/dev/null | tr -d ' ')" = "$serial" ] && { echo "$d"; return 0; }
    done
    return 1
  fi
  # The image's udev rules link the attached name to the NVMe device (or the
  # Xen-style device keeps the name, or its xvd twin).
  for d in "$DEV_DIR/$name" "$DEV_DIR/xvd${name#sd}"; do
    [ -e "$d" ] && { readlink -f "$d"; return 0; }
  done
  # No link: ask each NVMe EBS device for the name it was attached under.
  if command -v ebsnvme-id >/dev/null 2>&1; then
    for d in "$DEV_DIR"/nvme*n1; do
      [ -e "$d" ] || continue
      m="$(ebsnvme-id -b "$d" 2>/dev/null || true)"
      [ "${m#/dev/}" = "$name" ] && { echo "$d"; return 0; }
    done
  fi
  return 1
}

dev=""
deadline=$((SECONDS + TIMEOUT_SECONDS))
while [ "$SECONDS" -lt "$deadline" ]; do
  if dev="$(find_device)"; then break; fi
  dev=""
  sleep 2
done
[ -n "$dev" ] || die "no device for $DEVICE after ${TIMEOUT_SECONDS}s: is the volume attached?"
log "$DEVICE is $dev"

if mountpoint -q "$MOUNT_POINT"; then
  log "$MOUNT_POINT is already mounted"
  exit 0
fi

fs="$(blkid -o value -s TYPE "$dev" 2>/dev/null || true)"
if [ -z "$fs" ]; then
  # No file system signature. Make one only on a device that is entirely
  # blank: not on one with a partition table or partitions.
  parts="$(lsblk -nro TYPE "$dev" 2>/dev/null || true)"
  if [ -n "$(blkid -o value -s PTTYPE "$dev" 2>/dev/null || true)" ] || [[ "$parts" == *part* ]]; then
    die "$dev has a partition table but no file system: refusing to format it"
  fi
  log "$dev is blank: making a $FS_TYPE file system labelled $LABEL"
  case "$FS_TYPE" in
    xfs) mkfs.xfs -L "$LABEL" "$dev" ;;
    ext4) mkfs.ext4 -q -L "$LABEL" "$dev" ;;
    *) die "unknown file system type $FS_TYPE" ;;
  esac
  fs="$FS_TYPE"
elif [ "$fs" != "$FS_TYPE" ]; then
  log "$dev already holds a $fs file system, not $FS_TYPE: mounting it as it is"
fi

mkdir -p "$MOUNT_POINT"
mount -t "$fs" -o "$MOUNT_OPTIONS" "$dev" "$MOUNT_POINT"
log "mounted $dev at $MOUNT_POINT"
`
