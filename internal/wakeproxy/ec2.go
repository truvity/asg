package wakeproxy

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EC2API is the subset of the EC2 client that [NewEC2] uses.
type EC2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(ctx context.Context, in *ec2.StartInstancesInput, opts ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, in *ec2.StopInstancesInput, opts ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

type awsCloud struct{ api EC2API }

// NewEC2 returns a [Cloud] backed by the EC2 API.
func NewEC2(api EC2API) Cloud { return awsCloud{api: api} }

func (a awsCloud) Find(ctx context.Context, key, value string) ([]Instance, error) {
	in := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + key), Values: []string{value}},
		{Name: aws.String("instance-state-name"), Values: []string{StatePending, StateRunning, StateStopping, StateStopped}},
	}}

	var out []Instance

	for {
		page, err := a.api.DescribeInstances(ctx, in)
		if err != nil {
			return nil, err
		}

		for _, r := range page.Reservations {
			for k := range r.Instances {
				i := &r.Instances[k]
				inst := Instance{ID: aws.ToString(i.InstanceId), PrivateIP: aws.ToString(i.PrivateIpAddress), Tags: map[string]string{}}
				if i.State != nil {
					inst.State = string(i.State.Name)
				}

				for _, t := range i.Tags {
					inst.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
				}

				out = append(out, inst)
			}
		}

		if page.NextToken == nil {
			return out, nil
		}

		in.NextToken = page.NextToken
	}
}

func (a awsCloud) Start(ctx context.Context, id string) error {
	if _, err := a.api.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
		return fmt.Errorf("start %s: %w", id, err)
	}

	return nil
}

func (a awsCloud) Stop(ctx context.Context, id string) error {
	if _, err := a.api.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}}); err != nil {
		return fmt.Errorf("stop %s: %w", id, err)
	}

	return nil
}

func (a awsCloud) Tag(ctx context.Context, id, key, value string) error {
	_, err := a.api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{id},
		Tags:      []types.Tag{{Key: aws.String(key), Value: aws.String(value)}},
	})
	if err != nil {
		return fmt.Errorf("tag %s: %w", id, err)
	}

	return nil
}
