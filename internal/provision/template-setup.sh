#!/bin/sh
# Runs INSIDE a container that is about to become a runner template (via `pve-runners template prepare`,
# or the image build in templates/).
# Installs the actions/runner tarball pushed to /tmp/actions-runner.tar.gz and a unit that runs exactly
# one JIT job, then powers the guest off so the host can destroy it.
set -eu

id runner >/dev/null 2>&1 || useradd --create-home --shell /bin/bash runner
rm -rf /opt/actions-runner
mkdir -p /opt/actions-runner /etc/gh-runner
tar -xzf /tmp/actions-runner.tar.gz -C /opt/actions-runner
rm -f /tmp/actions-runner.tar.gz
chown -R runner:runner /opt/actions-runner
/opt/actions-runner/bin/installdependencies.sh
chmod 700 /etc/gh-runner

cat > /etc/systemd/system/gh-runner.service <<'UNIT'
[Unit]
Description=GitHub Actions runner (one JIT job)
After=network-online.target
Wants=network-online.target
ConditionPathExists=/etc/gh-runner/jitconfig

[Service]
Type=simple
User=runner
WorkingDirectory=/opt/actions-runner
# /run/gh-runner, owned by the runner user, so it can delete the config once it has read it.
RuntimeDirectory=gh-runner
RuntimeDirectoryMode=0700
# Hand the single-use config to the runner user and remove the root copy before the job starts. The
# runner writes its logs to _diag; anything left there by root (an image build's checks) would make
# it crash on start, so the runner always gets a fresh one.
ExecStartPre=+/bin/sh -c 'install -o runner -m 0400 /etc/gh-runner/jitconfig /run/gh-runner/jit && rm -f /etc/gh-runner/jitconfig && rm -rf /opt/actions-runner/_diag'
# The runner reads ACTIONS_RUNNER_INPUT_JITCONFIG like --jitconfig; the environment keeps the credential
# out of the process list (ps), where an argument would show it to anyone looking.
ExecStart=/bin/sh -c 'ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat /run/gh-runner/jit); export ACTIONS_RUNNER_INPUT_JITCONFIG; rm -f /run/gh-runner/jit; exec ./run.sh'
# One job only: when the runner exits, for any reason, stop the guest so the host reclaims it.
ExecStopPost=+/bin/systemctl poweroff --no-block

[Install]
WantedBy=multi-user.target
UNIT

# daemon-reload needs a running systemd; image builds run in a plain container where `enable` alone
# is enough (it only writes symlinks).
if [ -d /run/systemd/system ]; then systemctl daemon-reload; fi
systemctl enable gh-runner.service
# Clones must not share identity.
truncate -s 0 /etc/machine-id
