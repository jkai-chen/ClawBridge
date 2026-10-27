#!/bin/sh
# Tests deploy/agentd-check.sh. It creates users and files, so it runs only
# in a disposable container, as root:
#   docker run --rm -v "$PWD/deploy:/deploy:ro" debian:12-slim sh /deploy/agentd-check-test.sh
set -eu
[ "$(id -u)" = 0 ] && [ -f /.dockerenv ] || { echo "run in a disposable container, as root"; exit 2; }
check=/deploy/agentd-check.sh
etc=/etc/clawbridge-agentd

# A correct install (the parts that need no systemd).
groupadd --system cbagent-sock
useradd --system --create-home --shell /usr/sbin/nologin cbagent
useradd --system --no-create-home --shell /usr/sbin/nologin --groups cbagent-sock cbagentd
uid=$(id -u cbagent)
mkdir -p /var/lib/systemd/linger "/etc/systemd/system/user@$uid.service.d" /usr/local/libexec/clawbridge /var/lib/clawbridge-agentd/policy
touch /var/lib/systemd/linger/cbagent
cp /deploy/systemd/agentd/delegate.conf "/etc/systemd/system/user@$uid.service.d/delegate.conf"
printf '#!/bin/sh\n' > /usr/local/libexec/clawbridge/clawbridge-agentd
chmod 0755 /usr/local/libexec/clawbridge/clawbridge-agentd
install -d -m 0700 "$etc"
spec='CLAWBRIDGE_SERVER_URL=https://bridge.example.test
CLAWBRIDGE_AGENT_IMAGE=sha256:aaaa
CLAWBRIDGE_EGRESS_IMAGE=sha256:eeee
CLAWBRIDGE_EGRESS_POLICY_DIR=/var/lib/clawbridge-agentd/policy'
printf '%s\n' "$spec" > "$etc/proxy.env"
chmod 0644 "$etc/proxy.env"
(umask 077; printf '%s\nCLAWBRIDGE_AGENTD_DOCKER=/run/clawbridge-agentd/docker.sock\n' "$spec" > "$etc/agentd.env")
(umask 077; printf 'CLAWBRIDGE_LAUNCHER_TOKEN=cbl_TESTTOKENtesttoken0123456789\n' > "$etc/launcher.env")
chown cbagentd:cbagentd /var/lib/clawbridge-agentd/policy
chmod 0755 /var/lib/clawbridge-agentd/policy

out=$(sh "$check") || { echo "$out"; echo "the correct install failed"; exit 1; }
echo "$out" | grep -q 'all passed'
if echo "$out" | grep -q 'TESTTOKEN'; then echo "the token was printed"; exit 1; fi

# Each break must fail, with its own line, and never print the token.
expect_fail() { # what is broken, the FAIL line expected, then the commands that break and restore it
  what=$1 line=$2 breaks=$3 restores=$4
  sh -c "$breaks"
  if out=$(sh "$check"); then echo "not caught: $what"; exit 1; fi
  echo "$out" | grep -q "^FAIL  $line" || { echo "$out"; echo "wrong line for: $what"; exit 1; }
  if echo "$out" | grep -q 'TESTTOKEN'; then echo "the token was printed: $what"; exit 1; fi
  sh -c "$restores"
  sh "$check" >/dev/null || { echo "not restored: $what"; exit 1; }
  echo "ok: $what"
}
expect_fail "cbagentd in docker" "cbagentd is not in the docker group" \
  "groupadd -f docker; usermod -aG docker cbagentd" "gpasswd -d cbagentd docker >/dev/null"
expect_fail "cbagentd not in the socket group" "cbagentd is in cbagent-sock" \
  "gpasswd -d cbagentd cbagent-sock >/dev/null" "usermod -aG cbagent-sock cbagentd"
expect_fail "no linger" "cbagent lingers" \
  "rm /var/lib/systemd/linger/cbagent" "touch /var/lib/systemd/linger/cbagent"
expect_fail "delegation for everyone" "no delegate.conf for every user" \
  "mkdir -p /etc/systemd/system/user@.service.d; touch /etc/systemd/system/user@.service.d/delegate.conf" "rm -r /etc/systemd/system/user@.service.d"
expect_fail "launcher.env readable" "launcher.env is root 0600" \
  "chmod 0644 $etc/launcher.env" "chmod 0600 $etc/launcher.env"
expect_fail "the token in agentd.env" "the launcher token is only in launcher.env" \
  "echo CLAWBRIDGE_LAUNCHER_TOKEN=cbl_TESTTOKENtesttoken0123456789 >> $etc/agentd.env" "sed -i '/LAUNCHER_TOKEN/d' $etc/agentd.env"
expect_fail "two tokens" "launcher.env holds one launcher token" \
  "echo CLAWBRIDGE_LAUNCHER_TOKEN=cbl_TESTTOKENsecond0123456789xx >> $etc/launcher.env" "sed -i '2d' $etc/launcher.env"
expect_fail "a mint directory left" "no mint directory is left" \
  "mkdir $etc/mint" "rmdir $etc/mint"
expect_fail "a writable binary" "the binary is root 0755" \
  "chmod 0775 /usr/local/libexec/clawbridge/clawbridge-agentd" "chmod 0755 /usr/local/libexec/clawbridge/clawbridge-agentd"
expect_fail "the policy dir root's" "the policy directory is cbagentd 0755" \
  "chown root:root /var/lib/clawbridge-agentd/policy" "chown cbagentd:cbagentd /var/lib/clawbridge-agentd/policy"
expect_fail "a seal key in agentd's files" "no seal key in agentd's files" \
  "echo CLAWBRIDGE_MODEL_KEY_SEAL_KEY=x >> $etc/agentd.env" "sed -i '/SEAL_KEY/d' $etc/agentd.env"
expect_fail "different images" "CLAWBRIDGE_AGENT_IMAGE is the same" \
  "sed -i 's/sha256:aaaa/sha256:bbbb/' $etc/proxy.env" "sed -i 's/sha256:bbbb/sha256:aaaa/' $etc/proxy.env"
echo "agentd-check-test: all passed"
