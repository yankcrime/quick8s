#!/usr/bin/env bash
# End-to-end test harness for quick8s: spins up a real VM via colima (macOS),
# runs bootstrap -> kubeconfig -> teardown, then the same lifecycle through a
# quick8s.yaml with up -> up (no-op) -> down, against it over SSH on its real
# network address, and verifies each step actually did what it claims.
#
# Requires: colima, jq, go.
#
# Usage: hack/e2e-test.sh
# Env vars:
#   COLIMA_PROFILE   colima profile name to use (default: quick8s-e2e)
#   KEEP_VM          if set, skip destroying the VM on exit (for debugging)

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE="${COLIMA_PROFILE:-quick8s-e2e}"
QUICK8S="$ROOT_DIR/bin/quick8s"

log() { echo "==> $*" >&2; }
fail() { echo "FAIL: $*" >&2; exit 1; }

cleanup() {
  if [ -n "${KEEP_VM:-}" ]; then
    log "KEEP_VM set, leaving colima profile '$PROFILE' running"
    return
  fi
  log "Destroying colima profile '$PROFILE'..."
  # -d/--data: without it, colima leaves the VM's disk (including anything
  # K3s wrote, e.g. etcd state) on disk under ~/.colima/_lima/_disks/<profile>
  # even after the instance itself is deleted - the next `colima start` for
  # the same profile silently resumes on that stale disk instead of a truly
  # fresh one. Always pass -d here.
  colima delete -f -d --profile "$PROFILE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

for bin in colima jq go; do
  command -v "$bin" >/dev/null 2>&1 || fail "$bin is required but not installed"
done

log "Building quick8s..."
(cd "$ROOT_DIR" && go build -o bin/quick8s ./cmd/quick8s)

log "Starting colima VM (profile: $PROFILE)..."
# --network-mode bridged: the default "shared" mode gives each colima
# profile its own isolated virtual network, so two VMs can't reach each
# other even though their addresses look like they're on the same subnet
# (192.168.64.0/24 for both, but no route between them). Bridged puts the VM
# on the real LAN via DHCP, which both is representative of a real target
# node and is required for any future multi-node (control-plane + worker)
# scenario in this harness.
colima start --profile "$PROFILE" --cpu 4 --memory 8 --network-address --network-mode bridged

IP="$(colima list --json --profile "$PROFILE" | jq -r '.address')"
[ -n "$IP" ] && [ "$IP" != "null" ] || fail "colima did not report a network address for profile '$PROFILE'"
log "VM address: $IP"

SSH_CONFIG="$(colima ssh-config --profile "$PROFILE")"
SSH_USER="$(awk '/^  User /{print $2}' <<<"$SSH_CONFIG")"
SSH_KEY="$(awk '/^  IdentityFile /{print $2}' <<<"$SSH_CONFIG" | tr -d '"')"
[ -n "$SSH_USER" ] && [ -n "$SSH_KEY" ] || fail "could not parse SSH user/key from 'colima ssh-config'"
log "SSH: $SSH_USER@$IP (key: $SSH_KEY)"

log "Waiting for SSH on the VM's real network address..."
for i in $(seq 1 30); do
  if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 \
       -i "$SSH_KEY" "$SSH_USER@$IP" true 2>/dev/null; then
    break
  fi
  [ "$i" -eq 30 ] && fail "SSH never became reachable on $IP"
  sleep 1
done

log "Running: quick8s bootstrap $IP"
KUBECONFIG_OUT="$("$QUICK8S" bootstrap "$IP" --ssh-user "$SSH_USER" --ssh-key "$SSH_KEY")"
grep -q "apiVersion: v1" <<<"$KUBECONFIG_OUT" || fail "bootstrap did not print a valid-looking kubeconfig"
log "bootstrap OK, kubeconfig looks valid"

log "Running: quick8s kubeconfig $IP"
KUBECONFIG_OUT2="$("$QUICK8S" kubeconfig "$IP" --ssh-user "$SSH_USER" --ssh-key "$SSH_KEY")"
grep -q "apiVersion: v1" <<<"$KUBECONFIG_OUT2" || fail "kubeconfig command did not print a valid-looking kubeconfig"
log "kubeconfig OK"

log "Running: quick8s teardown $IP"
"$QUICK8S" teardown "$IP" --ssh-user "$SSH_USER" --ssh-key "$SSH_KEY" -y
log "teardown OK"

log "Verifying K3s is actually gone..."
if "$QUICK8S" kubeconfig "$IP" --ssh-user "$SSH_USER" --ssh-key "$SSH_KEY" >/dev/null 2>&1; then
  fail "kubeconfig still fetchable after teardown"
fi
log "confirmed: K3s is gone"

CLUSTER_DIR="$(mktemp -d)"
trap 'rm -rf "$CLUSTER_DIR"; cleanup' EXIT
cat >"$CLUSTER_DIR/quick8s.yaml" <<EOF
cluster:
  ssh:
    user: $SSH_USER
    key: $SSH_KEY
  nodes:
    controlPlane:
      - $IP
k3s:
  server:
    node-label:
      - quick8s-e2e=true
EOF

log "Running: quick8s up (from a directory holding quick8s.yaml)"
KUBECONFIG_OUT3="$(cd "$CLUSTER_DIR" && "$QUICK8S" up)"
grep -q "apiVersion: v1" <<<"$KUBECONFIG_OUT3" || fail "up did not print a valid-looking kubeconfig"
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY" "$SSH_USER@$IP" \
  sudo cat /etc/rancher/k3s/config.yaml | grep -q "quick8s-e2e=true" || fail "up did not install k3s.server as the node's K3s config"
log "up OK, kubeconfig looks valid and K3s config was pushed"

log "Running: quick8s up again (should be a no-op)"
UP_AGAIN_ERR="$(cd "$CLUSTER_DIR" && "$QUICK8S" up 2>&1 >/dev/null)"
grep -q "nothing to do" <<<"$UP_AGAIN_ERR" || fail "second up was not a no-op: $UP_AGAIN_ERR"
log "second up OK, nothing to do"

log "Running: quick8s down -y"
(cd "$CLUSTER_DIR" && "$QUICK8S" down -y)
if "$QUICK8S" kubeconfig "$IP" --ssh-user "$SSH_USER" --ssh-key "$SSH_KEY" >/dev/null 2>&1; then
  fail "kubeconfig still fetchable after down"
fi
log "down OK, K3s is gone"

log "Running: quick8s down -y again (should skip the already-clean node)"
(cd "$CLUSTER_DIR" && "$QUICK8S" down -y)
log "second down OK"

log "PASS"
