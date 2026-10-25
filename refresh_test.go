package asg_test

import (
	"strings"
	"testing"

	"github.com/truvity/asg"
)

func refreshOf(t *testing.T, r *asg.RefreshArgs) map[string]any {
	t.Helper()

	a := baseArgs(t)
	a.Refresh = r

	g := mustRun(t, "router", a).find(t, "aws:autoscaling/group:Group", "router")

	return g["instanceRefresh"].(map[string]any)
}

// The Auto Scaling API starts a refresh on every launch template change when
// the group has a refresh block, so a launch_template trigger is redundant
// and Pulumi warns about it. The default is no trigger.
func TestRefreshHasNoTriggersByDefault(t *testing.T) {
	r := refreshOf(t, &asg.RefreshArgs{})

	if _, ok := r["triggers"]; ok {
		t.Errorf("triggers %v, want none", r["triggers"])
	}

	if r["strategy"] != "Rolling" {
		t.Errorf("strategy %v", r["strategy"])
	}

	p := r["preferences"].(map[string]any)
	for _, k := range []string{"maxHealthyPercentage", "skipMatching", "autoRollback"} {
		if _, ok := p[k]; ok {
			t.Errorf("preference %s is set by default", k)
		}
	}

	if p["instanceWarmup"] != "300" || p["minHealthyPercentage"] != float64(0) {
		t.Errorf("preferences %v", p)
	}
}

func TestRefreshPreferencesAndExplicitTriggers(t *testing.T) {
	r := refreshOf(t, &asg.RefreshArgs{
		MinHealthyPercentage: 100, MaxHealthyPercentage: 200, SkipMatching: true, AutoRollback: true, Triggers: []string{"launch_template"},
	})

	p := r["preferences"].(map[string]any)
	if p["maxHealthyPercentage"] != float64(200) || p["skipMatching"] != true || p["autoRollback"] != true || p["minHealthyPercentage"] != float64(100) {
		t.Errorf("preferences %v", p)
	}

	if tr := r["triggers"].([]any); len(tr) != 1 || tr[0] != "launch_template" {
		t.Errorf("triggers %v", tr)
	}
}

func TestRefreshRefusals(t *testing.T) {
	for _, bad := range []int{50, 99, 201} {
		a := baseArgs(t)
		a.Refresh = &asg.RefreshArgs{MaxHealthyPercentage: bad}

		if _, err := run(t, "router", a); err == nil || !strings.Contains(err.Error(), "MaxHealthyPercentage") {
			t.Errorf("MaxHealthyPercentage %d: error %v", bad, err)
		}
	}
}
