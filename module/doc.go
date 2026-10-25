// Package module is the extension mechanism of github.com/truvity/asg: a
// [Module] returns plain data, a [Contribution], that the appliance and the
// bootstrap plan fold into the user-data, the instance role and the security
// group. A module never creates a resource, so turning one on changes the
// launch template version and the role's policy, never the set of resources
// and their logical names. The package imports the standard library and
// github.com/truvity/asg/bootstrap only, so a provider-neutral half of a
// service can return modules without importing Pulumi.
//
// # Phases on the box
//
// A fragment's files and install steps run on the first boot, warm-pool
// instances included. What must run only when the instance goes in service
// (start a daemon, read a secret that a stopped instance must not hold, turn
// a timer on) is an [Step] in [Contribution].InService. The library writes
// the steps as files, and one oneshot unit, asg-in-service.service, runs them
// on every boot: it reads the instance's target lifecycle state from the
// instance metadata and does nothing in the warm pool (Warmed:*). Then it runs
// the [ReadyCheck]s of the contributions until each passes. The bootstrap
// plan's own bootstrap-ready.service orders itself after that unit and
// completes the launch lifecycle hook only if it is active, so a launch is
// completed after its checks passed and abandoned when they did not. This
// needs bootstrap.Options.Lifecycle.
//
// A [WatchCheck] is a local health check for the watchdog module: a file the
// watchdog runs every interval, healthy when it exits 0.
//
// [Drain]s and [ScrapeTarget]s are data. The library does not run drains yet
// (it has no terminate hook), and [Contribution.AddTo] refuses a drain rather
// than ignore it. Scrape targets are for the caller (an agent configuration).
package module
