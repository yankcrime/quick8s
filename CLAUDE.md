# quick8s — notes for future sessions

A Go CLI that bootstraps K3s clusters over SSH. This file captures decisions
and patterns that aren't obvious from reading the code alone — read it before
making structural changes.

## Package layout and why

```
cmd/quick8s        entrypoint only, wires up the Cobra root command
internal/cli        Cobra command definitions — flag parsing + orchestration only
internal/node        Target (host/port/user/key) + Client (SSH connection wrapper)
internal/k3s         K3s-specific operations taking narrow, consumer-defined remote interfaces
internal/shell       literal POSIX shell argument quoting shared by node and k3s
hack/                dev/test scripts (currently just the e2e harness)
```

`internal/node` deliberately holds *both* the node model and the SSH client in
one package (not split into `node` + `sshclient`) — they're always used
together and splitting them added no real boundary.

Commands in `internal/cli` should stay thin: dial, call into `internal/k3s`,
print status to stderr / payload to stdout. Business logic belongs in
`internal/k3s`, not in the command's `RunE`.

## SSH conventions

- **Auth is agent-first, always.** `node.Dial` tries the running SSH agent
  unconditionally; `--ssh-key` is an explicit fallback on top, not a
  replacement. Don't make `--ssh-key` required for anything.
- **`--ssh-user` defaults to the local OS username** (`os/user.Current()`),
  mirroring `ssh(1)` — never default to `root`.
- A passphrase-protected `--ssh-key` prompts interactively on stderr via
  `golang.org/x/term`. This requires a real TTY on stdin — it will fail with
  `operation not supported by device` when run from a non-interactive
  sandbox/CI shell. That's expected, not a bug to fix.
- Host key checking is intentionally `InsecureIgnoreHostKey()` (see TODO in
  `internal/node/ssh.go`) — acceptable for bootstrapping fresh nodes, flagged
  as a known gap rather than silently accepted.

## Root-owned remote files

`Client.RunAsRoot` and `Client.WriteFileAsRoot` select privileges before
executing a command: run directly when the remote UID is zero, otherwise
use `sudo -n sh -c`. Never retry a failed operation under different
privileges. This works for root on minimal images without sudo, preserves
stdin for uploads, and avoids executing a failed uninstall twice.

`Run` and `RunAsRoot` return stdout only; stderr is attached to failures
without losing the underlying SSH error. This keeps diagnostics out of
kubeconfig and token payloads. `Preflight` checks root access through this
same capability and treats SSH failures separately from an absent binary.

Use `internal/shell.Quote` for literal shell arguments, not Go's `%q`.
K3s install operations share a private installer helper for quoting and
invocation. The SSH agent socket is owned by `Dial` and closes when the
handshake completes or any earlier step fails.

## stdout vs stderr — a cobra footgun to remember

`cmd.Println()` / `cmd.Print()` write to `OutOrStderr()`, **not** stdout —
confirmed in cobra's own source. This bit us once already (kubeconfig output
was silently going to stderr, so `quick8s bootstrap host > kubeconfig.yaml`
produced an empty file for a while before it was caught by the e2e harness's
`$()` capture).

The convention going forward:
- Status/progress messages → `cmd.PrintErrf`/`cmd.PrintErrln` (correct, these
  really do go to stderr).
- Actual command output/payload (kubeconfig, etc.) → `fmt.Fprintln(cmd.OutOrStdout(), ...)`,
  never `cmd.Println`.

This is what makes `quick8s bootstrap host > kubeconfig.yaml` work.

`internal/k3s` never prints. Operations that have something to narrate take
a `k3s.Progress` (a `func(string)`, nil-safe), and the CLI decides where it
goes: `indented(cmd)` for quick steps like `Preflight`, and `slowStep` for
installs. `slowStep` indents lines under a title, prints a "still waiting"
heartbeat after 10s of silence, and reports the elapsed time. The installer's
stdout is streamed live via `Client.StreamAsRoot`, with the `[INFO]` prefix
stripped. The last installer line, `systemd: Starting k3s`, blocks until K3s
reports ready, and that is where the heartbeat matters.
Payload writes return their errors. Cobra has `SilenceErrors` enabled so
`main` prints each returned error once.

## Design philosophy: don't grow the flag surface to mirror K3s

`--config-file` (a local K3s `config.yaml`, uploaded byte-for-byte to
`/etc/rancher/k3s/config.yaml` before install) exists specifically so quick8s
doesn't need a flag for every K3s server option. K3s reads that file itself,
independent of the install script — quick8s just needs to put it in place
before first start. Prefer this pattern over adding new passthrough flags for
K3s-specific behavior.

## Worker join: shared logic, two entry points

`internal/cli/workers.go` (`joinWorkers`/`joinWorker`) is deliberately
factored out of `bootstrap.go` and reused by `join.go`. Both `bootstrap
--worker <host>` (join at bootstrap time) and `join <cp> --worker <host>`
(join later, standalone) go through the same code path: dial worker →
Preflight → `k3s.JoinAgent`. If you add a third way to join workers, reuse
this rather than re-implementing the connect/preflight/join sequence.

The control plane's node token is fetched fresh over SSH each time
(`k3s.NodeToken`) rather than ever being passed around or persisted by
quick8s — it's sensitive and there's no need to store it.

## `bootstrap`'s argument shape: single-node shorthand, else fully explicit

`bootstrap` takes an *optional* positional `[target]` plus repeatable
`--control-plane`/`--worker` flags, with a deliberate asymmetry:

- **Bare `bootstrap <host>` (no flags) is single-node shorthand** — `<host>`
  becomes the sole control plane.
- **The moment more than one node is involved, every control plane -
  including the first - must go through `--control-plane`.** The first
  `--control-plane` given is the one that initializes the cluster
  (`ClusterInit`); a positional target combined with `--control-plane` or
  `--worker` is a hard error telling the user to use `--control-plane`
  instead.

This was a deliberate redesign (see git history around when `--control-plane`
was added) away from an earlier version where the positional target was
*implicitly* control-plane #1 even when `--control-plane`/`--worker` were
also given. That implicit version was flagged as confusing UX - "why isn't
the first control plane also named with --control-plane?" - and fixed by
making the *only* shorthand the truly trivial single-node case, so there's
no partial/implicit rule to remember once you're describing a real
multi-node topology. If you touch this validation logic
(`newBootstrapCmd`'s `RunE` in `bootstrap.go`), preserve this: don't let a
positional arg silently coexist with either repeatable flag.

## Control plane join mirrors worker join

`internal/cli/controlplanes.go` (`joinControlPlanes`/`joinControlPlane`) is
the same pattern as `workers.go`, one level up: each `--control-plane` host
beyond the first installs an additional K3s *server* node (not agent)
against the initiating node's embedded etcd, using `k3s.JoinControlPlane` /
`ControlPlaneOpts` (mirrors `JoinAgent`/`AgentOpts`, but installs role
`server --server <url>` instead of relying on `K3S_URL` to imply agent
mode). The initiating node needs `InstallOpts.ClusterInit` (`server
--cluster-init`) for etcd instead of the default sqlite backend —
`bootstrap` sets this automatically whenever more than one `--control-plane`
is given, so a single-control-plane bootstrap (with or without workers) is
untouched.

There's no standalone `join --control-plane` yet (only workers have that, via
`join.go`) — just not asked for yet. Follow the same
factor-out-of-bootstrap-into-a-shared-helper pattern if/when it's added.

## `--tls-san` / `--node-ip`: always pin the address quick8s uses

Every K3s server install (`Install` and `JoinControlPlane`) passes
`--tls-san <host>` and, when `<host>` is a literal IP (guarded by
`nodeIPFor` in `sshflags.go` — hostnames like `fair-dust.boxd.sh` can't go in
`--node-ip`, only in `--tls-san`), `--node-ip <host>` too, where `<host>` is
literally the address quick8s used to dial that node.

This exists because of a real bug, not speculative hardening: K3s's
auto-detected node-ip/advertise-address is not necessarily the address
anyone else reaches the node on (this is *guaranteed* true on any dual-homed
or NAT'd host, which includes every colima VM). Without pinning it,
**verified directly**: the kubeconfig quick8s hands back is unusable from
outside the node (`kubectl get nodes` fails TLS verification — the server
cert's only SAN was the wrong internal address), and control-plane-to-
control-plane joins fail outright the same way. `internal/k3s/agent.go`
(`AgentOpts.NodeIP`) applies the same fix to workers for consistency, though
the specific bug above was only ever demonstrated for control-plane certs.

If you ever see `x509: certificate is valid for ..., not <the address you
used>` in a K3s log, this is the class of bug — check that whatever
installed that node actually passed `--tls-san`/`--node-ip` for the address
in question.

## Destructive commands require confirmation

`teardown` prompts (`internal/cli/confirm.go`) before running the uninstall
script, skippable with `-y`/`--yes`. Any future destructive command (cluster
delete, node removal, etc.) should follow the same pattern rather than
executing silently.

## Testing: `hack/e2e-test.sh`

Real lifecycle test against a real, disposable VM (colima on macOS) — not
mocks. Two things worth knowing before touching it:

1. **`--network-mode bridged` is required**, not optional. colima's default
   "shared" mode gives each VM profile an address that *looks* like it's on
   a normal subnet (e.g. `192.168.64.0/24`) but is actually isolated
   per-profile — two shared-mode VMs cannot reach each other even though
   `ip route` looks normal. Bridged mode puts the VM on the real LAN via
   DHCP. This only matters once you have >1 VM (multi-node scenarios); it
   was invisible in single-node testing.
2. **`make e2e-clean`** exists because a killed/crashed run (or `KEEP_VM=1`)
   strands a VM under a non-default colima profile, which `colima list` (no
   `--profile` flag) won't show you — easy to lose track of. Reach for this
   before assuming "colima isn't running."
3. **`colima delete` always needs `-d`/`--data`.** Without it, colima
   deletes the VM instance but leaves its disk sitting at
   `~/.colima/_lima/_disks/<profile>/datadisk`. The next `colima start` for
   the *same profile name* silently resumes on that old disk instead of a
   genuinely fresh one - no warning, no indication anything is reused. This
   caused a real multi-hour debugging detour (see "Resolved" below) before
   being caught by manually inspecting `~/.colima/_lima/_disks/`. Both
   `hack/e2e-test.sh` and `make e2e-clean` pass `-d`; if you ever run
   `colima delete`/`colima start` by hand for a quick8s test profile, do the
   same, or the VM isn't actually fresh no matter what `colima list` claims.

If you extend the harness to cover multi-node scenarios, see the next
section first.

## Resolved: the "fresh" VM etcd mismatch was a colima disk-persistence bug, not K3s

`bootstrap --control-plane` (HA, embedded etcd) initially looked completely
broken: three separate attempts, including with fresh VMs and explicit
`--node-ip`, all hit an identical failure on the *first* control plane node
before a second node was even involved:

```
Failed to test etcd connection: this server is not a member of the etcd cluster.
Found [<node>=https://192.168.5.1:2380], expect: [<node>=https://192.168.1.105:2380]
```

Every setting in the failing run's own logs was already correct
(`listen-peer-urls`, `--advertise-address`, `--node-ip` all showed the
right, explicitly-pinned address) — yet etcd's own startup check
(`member-initialized: true`) found a pre-existing member record pointing at
a *different* interface's address, on a VM that was supposedly just created
fresh.

**Root cause, found by inspecting the filesystem directly:** `colima delete
-f` (without `-d`/`--data`) does not delete the VM's disk —
`~/.colima/_lima/_disks/<profile>/datadisk` survives, and a later `colima
start` for the *same profile name* silently resumes on it. Every "fresh" VM
across three test attempts was actually the same disk, accumulating etcd
state (and its stale peer-address record) from every prior attempt. This
was not a K3s bug, not a quick8s bug, and not a genuine environment
limitation — it was a gap in the delete command used in manual testing.
Fixed in `hack/e2e-test.sh` and `Makefile`'s `e2e-clean` (see the Testing
section above); this is also the likely explanation for the
harder-to-pin-down worker load-balancer symptom below, though that one
wasn't specifically re-verified with clean disks.

**With disks genuinely wiped between attempts, `--control-plane` works
first try:** a 3-control-plane + 3-worker cluster bootstraps cleanly in one
`bootstrap` call, all six nodes reach `Ready` with correct roles
(`control-plane,etcd` × 3, worker × 3) and correct `INTERNAL-IP`, and the
resulting kubeconfig was confirmed to work with plain `kubectl` run outside
any VM (not `k3s kubectl` over SSH) — proving the `--tls-san`/`--node-ip`
fix end-to-end, not just via node status.

If you ever see an etcd/cluster-membership mismatch, a node with an
unexpectedly *old* IP, or generally "this VM claims to be fresh but isn't"
on colima: check `~/.colima/_lima/_disks/` for a stale directory before
assuming it's a K3s or quick8s problem.

## Possible flake: K3s load-balancer on dual-NIC VMs (unclear if still real)

Before the disk-persistence bug above was found, a joining worker's
`k3s-agent` was once observed sitting in `activating` indefinitely, logging:

```
Waiting to retrieve agent configuration; server is not ready: .../serving-kubelet.key: <nil>
```

At the time this was traced to K3s's agent-side load balancer picking up a
spurious backend address from its supervisor bootstrap handshake
(`proxy.SupervisorAddresses()` in `pkg/agent/tunnel`), with the Kubernetes
`Endpoints`/`EndpointSlice` objects confirmed correct. It self-healed within
seconds in one run and stayed stuck 10+ minutes in another with identical
setup. **Given that the control-plane HA issue above turned out to be stale
disk state, not a genuine K3s timing race, this worker symptom deserves the
same suspicion** — it was never re-tested with confirmed-clean disks. If you hit
this again: check `~/.colima/_lima/_disks/` for staleness first, before
re-doing the K3s source tracing that produced the original (possibly
wrong) diagnosis above.

## Environment quirk: Bash tool auto-mode classifier outages

Not a quick8s or local-machine issue — this session hit a transient outage
in Claude Code's own server-side "auto mode" safety classifier that gates
Bash tool calls. Symptom: every Bash call fails with "server-side auto mode
classifier is temporarily unavailable," even for trivial commands, while
everything local (colima, SSH, the binary itself) is fine. It resolves on
its own; retrying after a pause works. Don't mistake this for a local
networking or colima problem.
