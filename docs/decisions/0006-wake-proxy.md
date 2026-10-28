# 0006 — Wake proxy: stopped instances that start when somebody connects

**Status:** Accepted

## Context

Some boxes are big and idle most of the day: a remote build worker is the
typical one, one per CPU architecture, with a large persistent cache disk.
Paying for them around the clock is waste, but a build client cannot wait for
a person to start one.

`Args.Single` and `Args.Data` (0002 and the README) already give such a box
its shape: one plain instance whose run state Pulumi does not manage, with a
volume that outlives it. What is missing is whoever owns the run state: start
it when a client connects, stop it when nobody has for a while.

Measured: a stopped instance is running about 33 seconds after the start
request, and the service on it needs a few more seconds before its port
accepts. A typical client gives up after about 40 seconds, so a client that
connects to the sleeping box directly fails, and one that connects through
something that holds the connection open succeeds.

## Decision

1. **A small always-on proxy owns the builders' run state.** It is `cmd/wake-proxy`,
   an appliance in the existing Auto Scaling group mode (desired 1, one
   stopped warm-pool spare) behind a front NLB, on a very small instance
   type. The NLB's target group moves from the box to the proxy; the box is
   no longer a target of anything.
2. **Layer 4 pass-through.** The proxy accepts TCP, and when the target is
   reachable it pipes the bytes both ways with a half-close per direction.
   It never terminates TLS: mutual TLS stays end to end between the client
   and the service on the box, so the proxy holds no certificate and no key,
   and a compromised proxy still cannot read or forge a build.
3. **Wake on the first byte, hold the client.** A connection counts as
   activity, and may wake the target, only once the client has sent a byte.
   After accept the proxy waits for that byte for the route's
   `firstByteTimeout` (default 30 s); a client that closes or stays silent
   (a TCP health probe) is closed with no accounting, no tag write and no
   wake, and is only counted in `wake_proxy_connections_without_data_total`.
   The byte is replayed first on the upstream stream. A route with
   `wakeOn: "connect"` skips the wait and acts on accept, for protocols where
   the server speaks first. After the byte (or on accept) the proxy looks the target
   up by one tag (exactly one instance, running or stopped; none or several
   is an error that is logged and counted, and the client is closed). Then:
   `running` and the port accepts, splice; `stopped`, call
   `StartInstances`, poll `DescribeInstances` until `running`, dial the
   private address with short retries until the port accepts or the wake
   timeout (default 180 s) ends, and only then splice; `stopping`, wait until
   `stopped` and then start; `pending`, wait. The client's connection stays
   open the whole time, with whatever it already sent waiting in the socket
   buffer. The wake is single-flight per route: concurrent clients wait on
   the same wake and `StartInstances` is called once. A wake continues when
   a client gives up, so the client's retry finds the box up.
4. **Idle stop from a shared tag.** The proxy counts the open connections of
   each route. While any is open, and on the last close, it writes the time
   (RFC 3339, UTC) to a tag on the **target instance** (default
   `wake-proxy/last-active`), at most once a minute, and at the first
   connection after idleness (a woken box must not look idle to another
   proxy). A check every minute stops the target only if this proxy has no
   connection on the route **and** the tag is older than the idle timeout
   (default 20 minutes). The tag, not process memory, is the shared state, so
   two proxies (the old and the new one while the group replaces the proxy)
   never stop a box the other one is using. A target without the tag is left
   alone until the idle timeout has passed since this proxy started. The
   decision and the stop are made under the route's lock, so a connection is
   either counted before the stop is decided or finds the instance stopping
   and wakes it again.
5. **Pulumi never owns the run state.** `Single` creates no instance state
   resource, so a stopped builder is no diff and a replacement (new image)
   comes up running; the proxy stops it again after the idle timeout.
6. **Health says the proxy serves.** The health endpoint is `GET /healthz`
   on its own port and answers 200 whenever the process runs. It does not
   look at any target: a sleeping box is the normal state, and a health check
   that followed it would make the NLB take the proxy out whenever the box
   sleeps.
7. **Least privilege, in the role.** `ec2:DescribeInstances` cannot be
   restricted to a resource, so it is on `*`. `ec2:StartInstances` and
   `ec2:StopInstances` are allowed on instances only where
   `aws:ResourceTag/<key>` equals the target's tag value, and
   `ec2:CreateTags` the same, limited to the activity tag key with
   `aws:TagKeys`. `modules/wakeproxy.Statements` builds them, one pair of
   statements per target tag.
8. **The key trap.** If the box's EBS volumes (root or data) are encrypted
   with a customer-managed KMS key, starting the instance is a request the
   caller makes of the key on the volumes' behalf. Without `kms:CreateGrant`
   (condition `kms:GrantIsForAWSResource` true), `kms:Decrypt` and
   `kms:GenerateDataKeyWithoutPlaintext` on that key, `StartInstances`
   succeeds and the instance then fails to leave `pending`, with the reason
   only in the instance's state-transition message. `PolicyInput.KMSKeyARNs`
   takes the keys; a deployment that uses an AWS-managed key needs none.
   The statements could be narrowed further with `kms:ViaService`; they are
   not, to keep the trap's fix small and certain.
9. **Shipping the binary.** The repository had no way to ship a binary, so
   the release now builds `cmd/wake-proxy` for linux/amd64 and linux/arm64
   (goreleaser, static, `CGO_ENABLED=0`) and attaches the raw files, named
   `wake-proxy_<version>_linux_<arch>`, and `checksums.txt` to the GitHub
   release. `modules/wakeproxy` takes a `bootstrap.Binary` per architecture,
   that is a URL (`ReleaseURL`) **and the sha256 the consumer pins**: the
   box downloads on its first boot (warm pool included) and refuses a
   mismatch, as it does for every download. The module writes the JSON
   configuration, installs the unit and adds a ready check and a watchdog
   check on the health port. The unit has no `[Install]` section and is
   started by an in-service step, so a warm-pool spare never serves; it has a
   dynamic user, an empty capability set (only `CAP_NET_BIND_SERVICE` when a
   listen port is below 1024), `NoNewPrivileges`, a read-only system,
   `Restart=always`, and a stop timeout that exceeds the shutdown grace.
10. **Configuration is JSON.** The standard library reads it, the module
    renders it with `encoding/json` from the same type the binary reads
    (`internal/wakeconfig`), unknown fields are refused, and the module
    writes the defaults into the file so a later change of a default cannot
    change a running box. It holds no secret.
11. **Operations.** Logs are one JSON line per wake, stop and connection open
    and close, with durations. Metrics are Prometheus text on an optional
    separate listener: active connections, connections, connections without
    data, wakes, wake
    failures, wake duration (sum and count), stops, dial failures and
    resolve failures, per route. On SIGTERM the proxy stops accepting and
    lets open streams finish for the shutdown grace (default 30 s) before it
    closes them.

## Alternatives considered

- **Auto Scaling scale-to-zero with a warm pool.** Tested and rejected. A
  scale-in refills the pool with a new instance and a new disk, so the cache
  disk's identity is lost with every cycle; scale-in also waits out the
  target group's 900 second deregistration delay; and an instance that is
  replaced instead of stopped makes a persistent volume impossible to keep
  attached to "the" builder.
- **A Lambda that starts the instance** (from an event or a CloudWatch
  alarm). It cannot hold a client's TCP connection, so the client's first
  attempts fail until the box is up, and it needs a second component that
  decides when to stop. The proxy holds the connection and is the single
  owner of both transitions.
- **Always-on builders.** Simple and the fastest start, and the cost this is
  meant to remove.
- **A proxy that terminates TLS or speaks the protocol.** Gives routing by
  name and metrics per request, at the price of a certificate and key on the
  proxy and a protocol implementation to keep current.

## Failure modes

- **The proxy dies.** `Restart=always` restarts the process; the watchdog
  marks the instance unhealthy if the health port stays down, and the group
  replaces it. Open streams are cut and the client retries; the target keeps
  running and the idle clock continues from the tag.
- **Two proxies during a replacement.** Either may wake a box (the wake is
  idempotent: `StartInstances` on a pending instance is harmless); neither
  stops a box the other one has used within the idle timeout.
- **A tag write fails.** It is logged and retried at the next interval. The
  worst case is a box stopped early, while its connection is still open,
  only if the failures last for the whole idle timeout and the other proxy
  has no connection either; the proxy's own check never stops with a
  connection open.
- **The target never comes up** (a broken image, a missing key grant): the
  client is closed at the wake timeout and the wake is counted as failed; the
  next connection tries again.
- **Zero or several instances match the selector.** Nothing is started, the
  client is closed, the lookup failure is logged and counted. A replacement
  of the box in the middle of a wake can show two instances for a while; the
  clients retry.
- **A client that gives up during the wake** leaves the wake running: the
  retry meets a box that is up. Its connection is counted until the proxy
  notices the close, which is when it tries to splice.
- **The EC2 API throttles or fails.** Describe errors during a wake are
  retried until the wake timeout. A failed stop is retried at the next check.
- **A box that is stopped by hand or by the idle check while a client
  connects.** The state is read after the connection is counted, and a stop
  is decided under the same lock; the client sees `stopping` and waits for
  the box to stop and start again, within the wake timeout.

## Consequences

- The first build after a quiet period is 40 to 60 seconds slower; the
  clients need a retry loop that is longer than their own connect timeout.
  That is the caller's, not this library's.
- The proxy is one more always-on instance per environment (a nano-class
  one), and its security group must be allowed on the box's service port:
  `Args.Security.Ingress` on the box takes the proxy's group as a source (`Rule.SourceGroups`).
- The NLB listener's TCP idle timeout (default 350 s) must exceed the wake
  timeout, because the client's connection is quiet while the box wakes.
- The binary lives in this repository and its module graph; the AWS SDK
  packages are linked into `cmd/wake-proxy` only and are not imported by
  the library's packages.
- Cache disks keep their identity: the volume is the same across stops and,
  with `Single`, across replacements.
