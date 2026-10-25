package bootstrap

// Fragment contributes items to a [Plan]. Name identifies the fragment in
// errors (and in the conflict message when two fragments collide).
type Fragment interface {
	Name() string
	Add(*Plan) error
}

// Item is one thing a fragment can add. The item types of this package are
// the only implementations.
type Item interface {
	addTo(*Plan) error
}

// Delivery says how a [File] reaches the instance.
type Delivery int

const (
	// DeliveryAuto (the zero value) is inline unless the size gates move it
	// to an object.
	DeliveryAuto Delivery = iota
	// DeliveryInline always puts the content in the user-data.
	DeliveryInline
	// DeliveryObject always puts the content in an S3 object fetched at boot.
	// It needs [Options].Objects.
	DeliveryObject
)

// Phase says when a [Step] runs.
type Phase int

const (
	// PhaseInstall (the zero value) runs on the first boot only (runcmd, in
	// the final stage, after packages and files).
	PhaseInstall Phase = iota
	// PhaseBoot runs on every boot (bootcmd, in the earliest stage: network
	// and installed packages may not be there yet, so the script waits and
	// retries for what it needs).
	PhaseBoot
)

// Gzip is the compression policy.
type Gzip int

const (
	// GzipAuto (the zero value) compresses when the text is over the soft
	// limit.
	GzipAuto Gzip = iota
	// GzipNever never compresses.
	GzipNever
	// GzipAlways always compresses.
	GzipAlways
)

// RunMode says whether a [UnitFragment] also loads its token and starts the
// unit.
type RunMode int

const (
	// RunAuto (the zero value) installs, writes the unit, loads the token and
	// starts the unit, all on the first boot.
	RunAuto RunMode = iota
	// RunManual installs and writes the unit and leaves LoadToken and Start
	// to the caller (a later fragment or step).
	RunManual
)

type (
	// Package is a distribution package to install (cloud-init "packages").
	Package struct {
		Name string
	}

	// File is a file to create. Content is the file's text; with Encoding
	// "base64" it is the base64 of the bytes.
	File struct {
		// Path is absolute. Required.
		Path string
		// Mode is octal, such as "0644". Default "0644".
		Mode string
		// Owner is "user" or "user:group". Default "root:root".
		Owner string
		// Content is the file's content. May be empty.
		Content string
		// Encoding is "" (text) or "base64".
		Encoding string
		// Delivery defaults to DeliveryAuto.
		Delivery Delivery
		// Defer writes the file in the final stage, after users and packages
		// exist, instead of the early one. Inline files only.
		Defer bool
	}

	// Artifact is a file downloaded at first boot, checked against SHA256 and
	// installed at Dest. A mismatch after five attempts stops the boot.
	Artifact struct {
		// URL is https, without credentials. Required.
		URL string
		// SHA256 is the lower-case hex digest of the downloaded bytes.
		// Required.
		SHA256 string
		// Dest is an absolute path. Required.
		Dest string
		// Mode is octal. Default "0755" with Run, else "0644".
		Mode string
		// Run executes Dest after installing it.
		Run bool
	}

	// Source is one build of a [Binary].
	Source struct {
		URL    string
		SHA256 string
	}

	// Binary is a program with one pinned build per CPU architecture; the
	// instance installs the build for its own architecture. PerArch is keyed
	// "amd64" and "arm64"; an instance of an architecture without an entry
	// stops the boot.
	Binary struct {
		// Name is used in messages.
		Name string
		// Dest is the absolute install path. Mode is 0755.
		Dest    string
		PerArch map[string]Source
	}

	// Unit is a systemd unit file, or a drop-in for one, and what to do with
	// it. A Unit with empty Content only enables and starts a unit that
	// already exists.
	Unit struct {
		// Name is the unit name; ".service" is added when there is no
		// suffix.
		Name    string
		Content string
		// DropIn writes Content to /etc/systemd/system/<name>.d/10-bootstrap.conf
		// instead of replacing the unit.
		DropIn bool
		Enable bool
		Start  bool
	}

	// SSMSecret reads one SecureString parameter into a file under /run
	// (tmpfs, so it never reaches the disk). The value is never in the
	// user-data; only the parameter name is, and it is listed in
	// [Rendered].SSMParameters.
	//
	// /run is empty after every stop and start (a warm-pool resume is one),
	// so by default the secret is loaded on EVERY boot: the composer writes a
	// script and a systemd oneshot unit, bootstrap-secrets.service, that runs
	// after the network is online. FirstBootOnly is the explicit opt-in to a
	// single load in the first-boot steps.
	SSMSecret struct {
		// Parameter is an absolute parameter name such as /estate/token.
		Parameter string
		// Dest is a file in a directory below /run. The directory is mode
		// 0700.
		Dest string
		// Mode is 0600 (default) or 0400.
		Mode string
		// Owner is "user" or "user:group". Default: root.
		Owner string
		// Format is FormatRaw (the value, no newline) or FormatEnv (one
		// NAME="value" line; Var names it).
		Format Format
		Var    string
		// Units are the systemd units that read Dest. Each gets a drop-in
		// that makes it require and start after bootstrap-secrets.service,
		// which in turn is ordered before it. Not with FirstBootOnly.
		Units []string
		// FirstBootOnly loads the secret once, in the first-boot steps, and
		// not again after a reboot.
		FirstBootOnly bool
	}

	// Step is a shell script, run by bash under "set -e". Steps run in the
	// order (Phase, Order, registration). Generated steps use these orders:
	// object fetch -1000, artifacts and binaries -200, secrets -100, systemd
	// daemon-reload -50, unit enable and start 100; a step of order 0 runs
	// after the downloads and secrets and before the units start.
	Step struct {
		// Name is a one-line label, written as a comment in the script.
		Name   string
		Phase  Phase
		Order  int
		Script string
	}
)

// Format is the layout of an [SSMSecret] file.
type Format int

const (
	// FormatRaw (the zero value) writes the parameter value alone.
	FormatRaw Format = iota
	// FormatEnv writes NAME="value", for a systemd EnvironmentFile.
	FormatEnv
)

func (i Package) addTo(p *Plan) error   { return p.addPackage(i) }
func (i File) addTo(p *Plan) error      { return p.addFile(i) }
func (i Artifact) addTo(p *Plan) error  { return p.addArtifact(i) }
func (i Binary) addTo(p *Plan) error    { return p.addBinary(i) }
func (i Unit) addTo(p *Plan) error      { return p.addUnit(i) }
func (i SSMSecret) addTo(p *Plan) error { return p.addSSMSecret(i) }
func (i Step) addTo(p *Plan) error      { return p.addStep(i) }

type group struct {
	name  string
	items []Item
}

func (g group) Name() string { return g.name }

func (g group) Add(p *Plan) error {
	for _, it := range g.items {
		if err := it.addTo(p); err != nil {
			return err
		}
	}

	return nil
}

// Compose is a fragment made of items.
func Compose(name string, items ...Item) Fragment { return group{name: name, items: items} }
