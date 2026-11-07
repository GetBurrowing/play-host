# Native session process ownership

Native mode requires rootless Podman with the systemd cgroup manager and an active
user systemd manager for the dedicated runtime account. The infrastructure operator
provisions that account's user manager/linger policy and UDP pool. `systemd_run` in
the native config may name its executable; the default is `systemd-run`.

Every session launch uses a separate `burrow-native-<session UUID>.scope` in
`app.slice`, created with `systemd-run --user --scope --collect`. The command inside
that scope is `podman --cgroup-manager=systemd run -d` with the ordinary immutable
image, ownership labels, private mounts and resource limits. Arguments are passed
directly, without a shell. Missing scope permissions or an unavailable user manager
fails the launch; there is no unscoped fallback.

Detached containers alone are insufficient: rootless conmon, slirp and port helpers
can inherit the launching service's cgroup. Stopping that service then kills their
network while the container still appears running. A separate session scope keeps
those helpers outside the host service. Do not substitute `KillMode=process`.

The provider still inspects exact owner/session/generation labels before removing a
container. It never stops arbitrary scope units: a scope is collected when its
processes exit, and rootless infrastructure helpers can be shared. Host restarts
preserve the state directory and session credential files, recover the same live
container, then reconcile it with core. Test actual inbound UDP, outbound HTTPS
control and roster freshness across a host-service stop; `State.Running=true`
alone is not a network or recovery check.
