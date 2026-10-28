package wakeproxy

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/truvity/asg/module"
)

// Statement ids.
const (
	SidDescribe     = "DescribeInstances"
	SidStartStop    = "StartStopTargets"
	SidTag          = "TagActivity"
	SidKeyUse       = "UseTargetVolumeKeys"
	SidKeyGrant     = "GrantTargetVolumeKeys"
	activityTagKeys = "aws:TagKeys"
)

// Selector is the tag that finds one target instance.
type Selector struct{ TagKey, TagValue string }

// PolicyInput is the input of [Statements].
type PolicyInput struct {
	Partition string
	Region    string
	Account   string
	// Targets are the tags of the instances the proxy may start, stop and
	// tag. Each must be the tag of exactly the instances to wake: the
	// condition is the only fence, because ec2:StartInstances and
	// ec2:StopInstances take no narrower resource than an instance.
	Targets []Selector
	// ActivityTag is the one tag key the proxy may write. Default
	// wake-proxy/last-active.
	ActivityTag string
	// KMSKeyARNs are the customer-managed keys that encrypt the targets'
	// EBS volumes (the root volume and the data volume). Empty: the volumes
	// use an AWS-managed key, which needs no statement.
	//
	// A start of an instance whose volumes use a customer-managed key is a
	// request the caller makes of the key, on the volumes' behalf: without
	// kms:CreateGrant (for AWS resources) and the two data key actions the
	// instance never leaves "pending".
	KMSKeyARNs []string
}

var (
	partitionRe = regexp.MustCompile(`^aws(-[a-z]+)*$`)
	accountRe   = regexp.MustCompile(`^[0-9]{12}$`)
	regionRe    = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	tagKeyRe    = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,128}$`)
	tagValueRe  = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,256}$`)
)

// InstanceARN is the ARN pattern of every instance of the account in the
// region. Which of them the proxy may touch is a condition, not the ARN.
func InstanceARN(partition, region, account string) string {
	return fmt.Sprintf("arn:%s:ec2:%s:%s:instance/*", partition, region, account)
}

// Statements are the statements the proxy's role needs and no more:
//
//   - ec2:DescribeInstances on every resource (the action cannot be scoped);
//   - ec2:StartInstances and ec2:StopInstances on instances that carry a
//     target tag;
//   - ec2:CreateTags on those instances, for the activity tag key only;
//   - with KMSKeyARNs, kms:CreateGrant (only for AWS resources),
//     kms:Decrypt and kms:GenerateDataKeyWithoutPlaintext on those keys.
//
// There is one start/stop and one tag statement per distinct target tag, in
// sorted order, so the document is byte-stable.
func Statements(in PolicyInput) ([]module.Statement, error) {
	if in.ActivityTag == "" {
		in.ActivityTag = DefaultActivityTag
	}

	var errs []error

	if !partitionRe.MatchString(in.Partition) || !accountRe.MatchString(in.Account) || !regionRe.MatchString(in.Region) {
		errs = append(errs, fmt.Errorf("want a partition such as aws, a region and a 12-digit account (got %q, %q, %q)", in.Partition, in.Region, in.Account))
	}

	if len(in.Targets) == 0 {
		errs = append(errs, errors.New("targets: at least one target tag is required"))
	}

	if !tagKeyRe.MatchString(in.ActivityTag) {
		errs = append(errs, fmt.Errorf("ActivityTag %q: not a valid tag key", in.ActivityTag))
	}

	seen := map[Selector]bool{}

	var targets []Selector

	for _, t := range in.Targets {
		if !tagKeyRe.MatchString(t.TagKey) || !tagValueRe.MatchString(t.TagValue) {
			errs = append(errs, fmt.Errorf("target %+v: want a tag key and a tag value of letters, digits and _.:/=+@- (no wildcard)", t))

			continue
		}

		if t.TagKey == in.ActivityTag {
			errs = append(errs, fmt.Errorf("target tag %q is the activity tag", t.TagKey))
		}

		if !seen[t] {
			seen[t] = true
			targets = append(targets, t)
		}
	}

	for _, k := range in.KMSKeyARNs {
		if !strings.HasPrefix(k, "arn:") || !strings.Contains(k, ":kms:") || strings.ContainsAny(k, "*?") {
			errs = append(errs, fmt.Errorf("KMSKeyARNs entry %q: want one key ARN, no wildcard", k))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	sort.Slice(targets, func(a, b int) bool {
		if targets[a].TagKey != targets[b].TagKey {
			return targets[a].TagKey < targets[b].TagKey
		}

		return targets[a].TagValue < targets[b].TagValue
	})

	instances := InstanceARN(in.Partition, in.Region, in.Account)
	out := []module.Statement{{Sid: SidDescribe, Effect: "Allow", Action: []string{"ec2:DescribeInstances"}, Resource: []string{"*"}}}

	for i, t := range targets {
		tag := map[string][]string{"aws:ResourceTag/" + t.TagKey: {t.TagValue}}

		out = append(out,
			module.Statement{
				Sid: fmt.Sprintf("%s%d", SidStartStop, i+1), Effect: "Allow",
				Action:    []string{"ec2:StartInstances", "ec2:StopInstances"},
				Resource:  []string{instances},
				Condition: map[string]map[string][]string{"StringEquals": tag},
			},
			module.Statement{
				Sid: fmt.Sprintf("%s%d", SidTag, i+1), Effect: "Allow",
				Action:   []string{"ec2:CreateTags"},
				Resource: []string{instances},
				Condition: map[string]map[string][]string{
					"StringEquals":              tag,
					"ForAllValues:StringEquals": {activityTagKeys: {in.ActivityTag}},
				},
			})
	}

	if keys := uniqueSorted(in.KMSKeyARNs); len(keys) > 0 {
		out = append(out,
			module.Statement{
				Sid: SidKeyUse, Effect: "Allow",
				Action:   []string{"kms:Decrypt", "kms:GenerateDataKeyWithoutPlaintext"},
				Resource: keys,
			},
			module.Statement{
				Sid: SidKeyGrant, Effect: "Allow",
				Action:    []string{"kms:CreateGrant"},
				Resource:  keys,
				Condition: map[string]map[string][]string{"Bool": {"kms:GrantIsForAWSResource": {"true"}}},
			})
	}

	return out, nil
}

func uniqueSorted(in []string) []string {
	m := map[string]bool{}

	var out []string

	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}

	sort.Strings(out)

	return out
}
