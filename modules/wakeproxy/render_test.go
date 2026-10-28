package wakeproxy_test

import (
	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"
)

// renderErr adds the contribution to a plan and renders it, for the errors
// the plan raises.
func renderErr(c module.Contribution) (*bootstrap.Rendered, error) {
	p, err := bootstrap.New(bootstrap.Options{Lifecycle: &bootstrap.Lifecycle{ASGName: "box", HookName: "box-launch"}})
	if err != nil {
		return nil, err
	}

	if err := c.AddTo(p); err != nil {
		return nil, err
	}

	return p.Render()
}
