package asg_test

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestTargetGroupARNsRegisterTheGroup(t *testing.T) {
	a := baseArgs(t)
	a.Group.HealthCheckType = "ELB"
	a.Group.GracePeriodSeconds = 600
	a.Group.TargetGroupARNs = []pulumi.StringInput{pulumi.String("tg-one"), pulumi.String("tg-two")}

	g := mustRun(t, "router", a).find(t, "aws:autoscaling/group:Group", "router")

	arns, _ := g["targetGroupArns"].([]any)
	if len(arns) != 2 || arns[0] != "tg-one" || arns[1] != "tg-two" {
		t.Errorf("targetGroupArns: %v", g["targetGroupArns"])
	}

	if g["healthCheckType"] != "ELB" || g["healthCheckGracePeriod"] != float64(600) {
		t.Errorf("ELB health with its grace period: %v %v", g["healthCheckType"], g["healthCheckGracePeriod"])
	}
}

func TestNoTargetGroupsLeavesTheGroupAlone(t *testing.T) {
	g := mustRun(t, "router", baseArgs(t)).find(t, "aws:autoscaling/group:Group", "router")
	if _, ok := g["targetGroupArns"]; ok {
		t.Errorf("targetGroupArns set without being asked: %v", g["targetGroupArns"])
	}
}

func TestNilTargetGroupARNIsRefused(t *testing.T) {
	a := baseArgs(t)
	a.Group.TargetGroupARNs = []pulumi.StringInput{nil}

	_, err := run(t, "router", a)
	if err == nil || !strings.Contains(err.Error(), "TargetGroupARNs[0] is nil") {
		t.Errorf("error: %v", err)
	}
}
