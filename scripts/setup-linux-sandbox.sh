#!/usr/bin/env bash
# Install and enable bubblewrap, the Linux workspace sandbox backend, on a
# disposable CI runner, then prove it can create the runner's profile. The
# tests' NANO_HARNESS_REQUIRE_SANDBOX gate is the authority; this probe only
# fails earlier with a clearer message. See docs/development.md#linux-sandbox.
set -euo pipefail

if [[ "$(uname -s)" != Linux ]]; then
  echo "setup-linux-sandbox: Linux only" >&2
  exit 1
fi

sudo apt-get update -q
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends bubblewrap
bwrap="$(command -v bwrap)"

# Ubuntu 23.10+ moves unconfined processes that create a user namespace into
# a profile without capabilities, which bwrap needs to build its mounts.
# Ubuntu's documented per-application fix is an unconfined profile granting
# userns to that one executable, the same shape as its shipped chrome profile.
restriction=/proc/sys/kernel/apparmor_restrict_unprivileged_userns
if [[ -r "$restriction" && "$(cat "$restriction")" == 1 ]]; then
  sudo tee /etc/apparmor.d/nano-harness-bwrap > /dev/null <<EOF
abi <abi/4.0>,
include <tunables/global>

profile nano-harness-bwrap $bwrap flags=(unconfined) {
  userns,
}
EOF
  sudo apparmor_parser -r /etc/apparmor.d/nano-harness-bwrap
  echo "setup-linux-sandbox: granted user namespaces to $bwrap through AppArmor"
fi

# The probe mirrors the runner's workspace-write mounts from runner.go.
if ! "$bwrap" --die-with-parent --unshare-pid --ro-bind / / --dev /dev --proc /proc --tmpfs /tmp -- /bin/true; then
  echo "setup-linux-sandbox: $bwrap cannot create the workspace sandbox profile" >&2
  exit 1
fi
echo "setup-linux-sandbox: $("$bwrap" --version) -> $bwrap"
