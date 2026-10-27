package asg_test

import (
	"testing"
)

// A program that tagged only its instances keeps its launch template as it
// is: the tag specifications are exactly the types it names, in its order.
func TestLaunchTemplateTagTypesKeepAnInstanceOnlyProgram(t *testing.T) {
	a := baseArgs(t)
	a.Compat.LaunchTemplateTagTypes = []string{"instance"}

	lt := mustRun(t, "router", a).find(t, "aws:ec2/launchTemplate:LaunchTemplate", "router")

	specs := lt["tagSpecifications"].([]any)
	if len(specs) != 1 || specs[0].(map[string]any)["resourceType"] != "instance" {
		t.Errorf("tag specifications %v, want only instance", specs)
	}
}

// Without the knob the launch template tags instances and volumes, as before
// (the defaults golden holds the rest).
func TestLaunchTemplateTagTypesDefault(t *testing.T) {
	lt := mustRun(t, "router", baseArgs(t)).find(t, "aws:ec2/launchTemplate:LaunchTemplate", "router")

	specs := lt["tagSpecifications"].([]any)
	if len(specs) != 2 || specs[0].(map[string]any)["resourceType"] != "instance" || specs[1].(map[string]any)["resourceType"] != "volume" {
		t.Errorf("tag specifications %v, want instance then volume", specs)
	}
}

// The trust policy a program already carries is kept byte for byte.
func TestAssumeRolePolicyIsKeptByteForByte(t *testing.T) {
	const own = `{"Statement":[{"Action":"sts:AssumeRole","Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"}}],"Version":"2012-10-17"}`

	a := baseArgs(t)
	a.Compat.AssumeRolePolicy = own

	role := mustRun(t, "router", a).find(t, "aws:iam/role:Role", "router")
	if role["assumeRolePolicy"] != own {
		t.Errorf("trust policy %v, want the program's own", role["assumeRolePolicy"])
	}
}
