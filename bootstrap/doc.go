// Package bootstrap composes the cloud-init user-data (#cloud-config) of an
// instance from fragments.
//
// A [Fragment] adds items to a [Plan]: packages, files, digest-pinned
// downloads, systemd units, secrets read from SSM Parameter Store at boot and
// free-form shell steps. [Plan.Render] turns the plan into one cloud-config,
// deterministically: the same fragments in any registration order that does
// not change the step order give the same bytes, and nothing depends on map
// iteration.
//
// What the composer refuses, because each is a bug that only shows on a
// booting machine:
//
//   - two fragments writing the same path or the same systemd unit;
//   - secret material anywhere in the user-data (private keys, cloud and VCS
//     access tokens). A secret is an [SSMSecret]: the parameter name is in the
//     user-data, the value never is, and the name is collected in
//     [Rendered].SSMParameters for the instance role's policy;
//   - an [Artifact] or [Binary] without a sha256, or one that is not https;
//   - user-data over the hard size limit.
//
// Size. EC2 user-data is limited to 16 KiB before base64. Past a soft limit
// the composer gzips the cloud-config (cloud-init reads gzip user-data
// natively); if that is still over, it moves [DeliveryAuto] files, largest
// first, to content-addressed objects ([Rendered].Objects) that a first-boot
// step fetches from S3, verifies against their sha256 and installs. The
// caller uploads the objects (this package has no cloud dependency) and gives
// the instance role s3:GetObject on their keys.
//
// Secrets load on every boot, from a generated oneshot unit, because /run is empty
// after a stop and start (see [SSMSecret]).
//
// Failure is closed. Generated steps run under "set -e"; a download that does
// not match its digest, or an object that cannot be fetched after five
// attempts, stops the boot sequence. With [Options].Lifecycle set the step
// first abandons the Auto Scaling lifecycle action, so the instance is
// replaced rather than left half-configured.
//
// [Verbatim] is a separate path for migrating a machine whose user-data is
// already rendered elsewhere: the text is carried unchanged, so adopting the
// composer changes nothing until the consumer moves a piece into a fragment.
//
// Mechanism only. Packages, URLs, digests, parameter names, buckets and
// groups are inputs. The package has no Pulumi or AWS import.
package bootstrap
