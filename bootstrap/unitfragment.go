package bootstrap

import (
	"errors"
	"fmt"
	"path"
)

// UnitFragment is a service installed, configured and started from pieces of
// shell and unit text: the shape that truvity/edge's instanceconnector
// returns, so a consumer converts with a field copy and neither module
// imports the other.
//
//	Install    bash that downloads and installs the program
//	Unit       the systemd unit file text, written to UnitPath
//	LoadToken  bash that reads a secret from SSM into a file under /run
//	Start      bash that daemon-reloads and starts (or enables) the unit
type UnitFragment struct {
	Install   string
	Unit      string
	UnitPath  string
	LoadToken string
	Start     string
	// SSMParameters are the parameter names LoadToken reads, for the role
	// policy.
	SSMParameters []string
}

type unitFragment struct {
	name string
	run  RunMode
	f    UnitFragment
}

// Fragment turns f into a [Fragment]. With [RunAuto] the first boot runs
// Install, writes the unit and runs Start, and LoadToken runs on EVERY boot
// from bootstrap-secrets.service (the token file is under /run, which a
// reboot empties). The unit gets a drop-in that requires and follows that
// service, and the service starts the unit after each load, as the unit has
// no [Install] section of its own. With [RunManual] only Install and the unit
// file, and the caller adds the rest.
func (f UnitFragment) Fragment(name string, run RunMode) Fragment {
	return unitFragment{name: name, run: run, f: f}
}

func (u unitFragment) Name() string { return u.name }

func (u unitFragment) Add(p *Plan) error {
	var errs []error
	if u.f.Unit != "" {
		errs = append(errs, p.addFile(File{Path: u.f.UnitPath, Content: u.f.Unit, Delivery: DeliveryInline}))
	}
	if u.f.Install != "" {
		errs = append(errs, p.addStep(Step{Name: u.name + " install", Order: -200, Script: u.f.Install}))
	}
	if u.run == RunAuto {
		if u.f.LoadToken != "" {
			errs = append(errs, u.loadToken(p))
		}
		if u.f.Start != "" {
			errs = append(errs, p.addStep(Step{Name: u.name + " start", Order: 100, Script: u.f.Start}))
		}
	}
	errs = append(errs, p.AddSSMParameters(u.f.SSMParameters...))

	return errors.Join(errs...)
}

func (u unitFragment) loadToken(p *Plan) error {
	if err := checkNoSecret(u.name+" load token", u.f.LoadToken); err != nil {
		return err
	}
	dir, file := path.Split(u.f.UnitPath)
	if dir != "/etc/systemd/system/" || file == "" {
		return fmt.Errorf("unit fragment %s: LoadToken runs from bootstrap-secrets.service, which orders itself around the unit; "+
			"UnitPath %q must be /etc/systemd/system/<unit>", u.name, u.f.UnitPath)
	}
	p.addSnippet(u.name+" load token", u.f.LoadToken, []string{file}, u.f.Start != "")

	return nil
}
