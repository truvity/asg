package asg

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/truvity/asg/module"
)

// PolicyVersion is the IAM policy language version of every document.
const PolicyVersion = "2012-10-17"

// Statement is one IAM policy statement. The JSON field order is fixed, and
// Action and Resource are always arrays, so a document is byte-stable. It is
// defined in [module], so a module can contribute statements without Pulumi.
type Statement = module.Statement

// PolicyContext is what a policy callback may use.
type PolicyContext struct {
	Partition string
	Region    string
	Account   string
	Names     Resolved
	// Bucket is Args.Objects.Bucket.
	Bucket string
	// SSMParameters and ObjectKeys are every parameter and object key the
	// appliance reads: the user-data's, Args.IAM's and the modules'.
	SSMParameters []string
	ObjectKeys    []string
}

// PolicyInput is the input of [BootstrapStatements].
type PolicyInput struct {
	Partition string
	Region    string
	Account   string
	// Group is the Auto Scaling group name. Empty: no group statement.
	Group string
	// Bucket holds ObjectKeys.
	Bucket        string
	SSMParameters []string
	ObjectKeys    []string
}

// Statement ids of [BootstrapStatements].
const (
	SidReadParameters = "ReadParameters"
	SidReadObjects    = "ReadObjects"
	SidOwnGroup       = "OwnGroup"
)

// ParameterARN is the ARN of an SSM parameter named like /estate/token.
func ParameterARN(partition, region, account, name string) string {
	return fmt.Sprintf("arn:%s:ssm:%s:%s:parameter%s", partition, region, account, name)
}

// ObjectARN is the ARN of an S3 object.
func ObjectARN(partition, bucket, key string) string {
	return fmt.Sprintf("arn:%s:s3:::%s/%s", partition, bucket, key)
}

// GroupARN is the ARN of an Auto Scaling group.
func GroupARN(partition, region, account, group string) string {
	return fmt.Sprintf("arn:%s:autoscaling:%s:%s:autoScalingGroup:*:autoScalingGroupName/%s", partition, region, account, group)
}

// BootstrapStatements are the statements the bootstrap scripts need and no
// more: ssm:GetParameter on exactly the parameters, s3:GetObject on exactly
// the object keys and, when Group is set, the two Auto Scaling actions
// bootstrap-ready.service and the failure path call, on that group. A list
// that is empty yields no statement; the lists are sorted and de-duplicated.
func BootstrapStatements(in PolicyInput) []Statement {
	var out []Statement

	if params := sortedUnique(in.SSMParameters); len(params) > 0 {
		arns := make([]string, len(params))
		for i, p := range params {
			arns[i] = ParameterARN(in.Partition, in.Region, in.Account, p)
		}

		out = append(out, Statement{Sid: SidReadParameters, Effect: "Allow", Action: []string{"ssm:GetParameter"}, Resource: arns})
	}

	if keys := sortedUnique(in.ObjectKeys); len(keys) > 0 {
		arns := make([]string, len(keys))
		for i, k := range keys {
			arns[i] = ObjectARN(in.Partition, in.Bucket, k)
		}

		out = append(out, Statement{Sid: SidReadObjects, Effect: "Allow", Action: []string{"s3:GetObject"}, Resource: arns})
	}

	if in.Group != "" {
		out = append(out, Statement{
			Sid:      SidOwnGroup,
			Effect:   "Allow",
			Action:   []string{"autoscaling:CompleteLifecycleAction", "autoscaling:SetInstanceHealth"},
			Resource: []string{GroupARN(in.Partition, in.Region, in.Account, in.Group)},
		})
	}

	return out
}

// Document renders statements as a policy document, indented by two spaces.
// A statement without an Effect is an Allow.
func Document(statements []Statement) (string, error) {
	st := make([]Statement, len(statements))
	for i, s := range statements {
		if s.Effect == "" {
			s.Effect = "Allow"
		}

		st[i] = s
	}

	b, err := json.MarshalIndent(struct {
		Version   string
		Statement []Statement
	}{PolicyVersion, st}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal policy: %w", err)
	}

	return strings.TrimSpace(string(b)), nil
}

// AssumeRolePolicy lets EC2 assume the instance role.
func AssumeRolePolicy(partition string) string {
	suffix := ""
	if partition == "aws-cn" {
		suffix = ".cn"
	}

	b, err := json.MarshalIndent(map[string]any{
		"Version": PolicyVersion,
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]any{"Service": "ec2.amazonaws.com" + suffix},
			"Action":    "sts:AssumeRole",
		}},
	}, "", "  ")
	if err != nil { // a map of strings cannot fail to marshal
		panic(err)
	}

	return string(b)
}
