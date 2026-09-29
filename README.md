# quick8s

A CLI that simplifies bootstrapping a Kubernetes cluster with [K3s](https://k3s.io) over SSH.

## Status

Early scaffold. Current scope: bootstrapping a K3s control plane on a remote
host identified by hostname or IP, optionally forming an HA (multi-node)
control plane and/or joining worker nodes to it.

## Usage

quick8s authenticates via your running SSH agent by default — no key path
needed, as long as the right key is loaded (`ssh-add -l` to check,
`ssh-add ~/.ssh/id_ed25519` to load one). `--ssh-user` defaults to your
local OS username (same as `ssh` itself), not root — pass it explicitly
if the remote account differs:

```
go run ./cmd/quick8s bootstrap <host-or-ip> \
  --ssh-user someuser \
  --k3s-version v1.30.4+k3s1
```

`--ssh-key <path>` is only needed as a fallback, e.g. against a host whose
key isn't in your agent; if that key is passphrase-protected, quick8s
prompts for it interactively. If `--k3s-version` is omitted, K3s installs
its latest stable release.

### K3s configuration file

Rather than growing quick8s's own flag set to cover every K3s server option,
`--config-file <path>` uploads a local [K3s config
file](https://docs.k3s.io/installation/configuration#configuration-file) to
`/etc/rancher/k3s/config.yaml` on the node before K3s installs and starts, so
K3s picks it up on its own on first boot:

```
go run ./cmd/quick8s bootstrap <host-or-ip> --config-file ./k3s-config.yaml
```

`k3s-config.yaml` uses K3s's own format — CLI flags become YAML keys (e.g.
`--tls-san` becomes `tls-san:`), repeatable flags become YAML lists. quick8s
doesn't interpret this file at all; it's copied byte-for-byte.

### Naming nodes: one target, or explicit --control-plane

For a single node, just name it — no flags needed:

```
go run ./cmd/quick8s bootstrap <host-or-ip>
```

For anything with more than one node, name **every** control plane
(including the first) with `--control-plane`. The first one given is the
one that initializes the cluster; a bare positional target and
`--control-plane`/`--worker` can't be mixed — quick8s will tell you to use
`--control-plane` instead if you try. This is deliberate: the single-node
shorthand only exists for the truly trivial case, so there's no hidden
"first argument is secretly a control plane too" rule to remember once
you're describing a real topology:

```
go run ./cmd/quick8s bootstrap --control-plane <cp1> --worker <worker1> --worker <worker2>
```

`--control-plane` and `--worker` both accept a comma-separated list as well
as being repeatable, so `--control-plane <cp1>,<cp2>,<cp3>` and
`--control-plane <cp1> --control-plane <cp2> --control-plane <cp3>` are
equivalent — mix whichever reads better. Note this means literal commas in
the value, not shell brace expansion: `--control-plane 192.168.1.{1..3}`
expands to three *space-separated* words before quick8s ever sees them
(which won't parse as three hosts), not `192.168.1.1,192.168.1.2,192.168.1.3`.

### Worker nodes

`--worker <host-or-ip>` (repeatable) joins a worker node to the cluster
after the control plane is up, using the cluster's node token fetched
straight from the initiating control plane over SSH — no need to pass it
around yourself. Workers are reached with the same
`--ssh-user`/`--ssh-port`/`--ssh-key` as the control plane, and install the
same K3s version.

### HA control plane

Give `--control-plane` more than once to form a highly-available cluster on
K3s's embedded etcd datastore instead of the single-node default (sqlite):

```
go run ./cmd/quick8s bootstrap \
  --control-plane <cp1> --control-plane <cp2> --control-plane <cp3> \
  --worker <worker1> --worker <worker2> --worker <worker3>
```

The first `--control-plane` initializes the cluster (`server
--cluster-init`); every other one joins it as an additional etcd member
(`server --server https://<cp1>:6443`). Use an **odd number of control
plane nodes** (3, 5, ...) for etcd to tolerate a node failure — an even
number doesn't add fault tolerance over one fewer node. `--config-file`,
when set, is pushed to every control plane node (not workers).

Every control plane node's K3s install is given `--tls-san` and `--node-ip`
pinned to the exact address quick8s dials it on. Without this, K3s's
auto-detected address can end up being the only cert SAN and the only
advertised node-ip, which silently breaks external `kubectl` access (the
kubeconfig quick8s hands back wouldn't actually work from your laptop) and,
on multi-homed hosts, control-plane-to-control-plane joins outright (TLS
verification fails on connect). The resulting kubeconfig points at the first
`--control-plane`; if you want it to keep working after that node goes
down, add the other control plane addresses as `tls-san` entries in a
`--config-file` yourself — quick8s won't guess a VIP for you.

**Verification status:** verified end-to-end against a real 6-node cluster
(3 control planes + 3 workers, `kubectl get nodes` showing all six `Ready`
with correct roles) — including confirming the kubeconfig actually works
from outside any node (plain `kubectl` from a laptop, not `k3s kubectl` over
SSH). Earlier attempts at this same test failed with a confusing etcd
bootstrap mismatch that turned out to be a colima gotcha, not a K3s or
quick8s issue: `colima delete -f` (no `-d`) leaves the VM's disk on disk, so
a same-named profile's "fresh" VM silently resumes on old state. See
CLAUDE.md if you hit this.

Other commands:

```
go run ./cmd/quick8s kubeconfig <host-or-ip>     # fetch just the kubeconfig, printed to stdout
go run ./cmd/quick8s teardown <host-or-ip>       # uninstall K3s (prompts for confirmation; -y to skip)
```

## Testing

`hack/e2e-test.sh` (macOS only) runs the full lifecycle — bootstrap,
kubeconfig, teardown — against a real, disposable VM. It uses
[colima](https://github.com/abiosoft/colima) with `--network-address
--network-mode bridged` to give the VM a real LAN IP via DHCP, so the test
goes over an actual network hop rather than a forwarded loopback port, same
as a real target node. Bridged mode (not the default "shared" mode) matters
even for a single VM: shared-mode colima profiles get addresses that look
like they're on the same subnet but aren't mutually routable, which would
silently break any future multi-node scenario added to this harness.
Requires `colima`, `jq`, and `go`.

```
make e2e
```

The VM is destroyed on exit regardless of pass/fail. Set `KEEP_VM=1` to leave
it running for debugging, and `COLIMA_PROFILE=<name>` to use a different
colima profile than the default `quick8s-e2e`.

If a run gets killed, crashes, or is left up via `KEEP_VM=1`, the VM is
stranded under its own profile — easy to miss since `colima list` (no
profile flag) only shows the default one. `make e2e-clean` destroys it
directly (respects `COLIMA_PROFILE` the same way).

## Layout

- `cmd/quick8s` — entrypoint, wires up the Cobra root command
- `internal/cli` — Cobra command definitions (flag parsing only)
- `internal/config` — resolved run options, shared between flags and (future) config files
- `internal/node` — target node model and SSH client
- `internal/k3s` — K3s install orchestration and preflight checks
- `hack/` — dev/test scripts (e2e harness)

## License

[Apache License 2.0](LICENSE)
