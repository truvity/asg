package asg

import (
	"fmt"
	"slices"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ebs"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lb"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// roleTags are the appliance's tags and the role tag of Single.
func (b *builder) roleTags(name string) pulumi.StringMap {
	m := b.tags(name)
	m[b.a.Single.RoleTagKey] = pulumi.String(b.a.Single.RoleTagValue)

	return m
}

// subnet is the instance's subnet. With a data volume it is checked against
// the volume's zone when the value is known, and a mismatch fails the update
// before anything is created in the wrong zone.
func (b *builder) subnet() pulumi.StringInput {
	s := b.a.Single.SubnetID
	if b.a.Data == nil {
		return s
	}

	zone := b.a.Data.AvailabilityZone
	sub := ec2.LookupSubnetOutput(b.ctx, ec2.LookupSubnetOutputArgs{Id: s.ToStringOutput().ToStringPtrOutput()}, b.invokeOptions()...)

	return pulumi.All(sub.Id(), sub.AvailabilityZone()).ApplyT(func(v []any) (string, error) {
		id, az := v[0].(string), v[1].(string)
		if az != zone {
			return "", fmt.Errorf("appliance %q: subnet %s is in %s but Data.AvailabilityZone is %s: an EBS volume attaches only inside its zone", b.name, id, az, zone)
		}

		return id, nil
	}).(pulumi.StringOutput)
}

// single creates the one instance from the launch template, and the data
// volume and target group attachments around it.
func (b *builder) single() error {
	s := b.a.Single
	version := b.out.LaunchTemplate.LatestVersion.ApplyT(func(v int) string { return fmt.Sprint(v) }).(pulumi.StringOutput)

	// No instance state resource and no ignored attribute: a stopped instance
	// is simply what the provider reads back, and shows no diff. The launch
	// template version is explicit, so a new version is a diff, and a
	// launch template change replaces the instance, deleting the old one
	// first so that the data volume is detached when the new one attaches it.
	opts := b.child(KindInstance, pulumi.DeleteBeforeReplace(true), pulumi.ReplaceOnChanges([]string{"launchTemplate"}))

	inst, err := ec2.NewInstance(b.ctx, b.logicals.Instance, &ec2.InstanceArgs{
		LaunchTemplate: ec2.InstanceLaunchTemplateArgs{Id: b.out.LaunchTemplate.ID(), Version: version},
		SubnetId:       b.subnet(),
		Tags:           b.roleTags(b.r.Group),
	}, opts...)
	if err != nil {
		return fmt.Errorf("instance: %w", err)
	}

	b.out.Instance = inst

	if err := b.dataVolume(inst); err != nil {
		return err
	}

	for i, t := range s.TargetGroups {
		args := &lb.TargetGroupAttachmentArgs{TargetGroupArn: t.ARN, TargetId: inst.ID()}
		if t.Port > 0 {
			args.Port = pulumi.Int(t.Port)
		}

		att, err := lb.NewTargetGroupAttachment(b.ctx, b.logicals.TargetGroupAttachment(i), args, b.child(KindTargetAttach)...)
		if err != nil {
			return fmt.Errorf("target group attachment %d: %w", i, err)
		}

		b.out.TargetAttachments = append(b.out.TargetAttachments, att)
	}

	return nil
}

// dataVolume creates the persistent volume and attaches it to inst. The
// volume is protected and retained on delete. The attachment follows the
// instance: it is replaced with it, deleted first (the instance is stopped
// before the detach, so the file system is not cut off).
func (b *builder) dataVolume(inst *ec2.Instance) error {
	d := b.a.Data
	if d == nil {
		return nil
	}

	name := b.r.Base + "-data"
	args := &ebs.VolumeArgs{
		AvailabilityZone: pulumi.String(d.AvailabilityZone),
		Size:             pulumi.Int(d.SizeGiB),
		Type:             pulumi.String(d.Type),
		Encrypted:        pulumi.Bool(true),
		Tags:             b.roleTags(name),
	}

	if d.IOPS > 0 {
		args.Iops = pulumi.Int(d.IOPS)
	}

	if d.ThroughputMiBps > 0 {
		args.Throughput = pulumi.Int(d.ThroughputMiBps)
	}

	if d.KMSKeyID != "" {
		args.KmsKeyId = pulumi.String(d.KMSKeyID)
	}

	vol, err := ebs.NewVolume(b.ctx, b.logicals.DataVolume, args, b.child(KindDataVolume, pulumi.Protect(true), pulumi.RetainOnDelete(true))...)
	if err != nil {
		return fmt.Errorf("data volume: %w", err)
	}

	b.out.DataVolume = vol

	att, err := ec2.NewVolumeAttachment(b.ctx, b.logicals.DataAttachment, &ec2.VolumeAttachmentArgs{
		DeviceName:                  pulumi.String(d.DeviceName),
		InstanceId:                  inst.ID(),
		VolumeId:                    vol.ID(),
		StopInstanceBeforeDetaching: pulumi.Bool(true),
	}, slices.Concat(b.child(KindDataAttachment), []pulumi.ResourceOption{pulumi.DeleteBeforeReplace(true)})...)
	if err != nil {
		return fmt.Errorf("data volume attachment: %w", err)
	}

	b.out.DataAttachment = att

	return nil
}
