set -euo pipefail
# One node: set the host up for its runtime, allow the node port only from the
# control host, join the fleet with a one-time token, and give it the preview
# domain. A node that has already joined gets no token and only the firewall
# and domain steps, which also finish a join that stopped part way, plus the
# new binaries when it is being upgraded.
# Rendered by apply.go and run as root over ssh.
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
# ssh forwards the operator's locale, which a fresh host may not have.
export LC_ALL=C.UTF-8
# Package installs wait on the dpkg lock: a fresh host is often still running unattended-upgrades.
say() { printf '   %s\n' "$*"; }
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

{{- if .Token}}

say "packages"
apt-get -o DPkg::Lock::Timeout=300 update -qq
apt-get -o DPkg::Lock::Timeout=300 install -y -qq curl ca-certificates tar iptables >/dev/null

say "release {{.Tag}}"
base64 -d > "$stage/SHA256SUMS" <<'SUMS'
{{.SumsB64}}
SUMS
files="envd-linux-amd64 pilldrops-linux-amd64 source.tar.gz"
for f in $files; do curl -fsSL {{sq .ReleaseBase}}"$f" -o "$stage/$f"; done
(cd "$stage" && grep -E " \*?($(echo "$files" | tr ' ' '|'))\$" SHA256SUMS | sha256sum -c --quiet -)
src=/opt/pilldrops/src
rm -rf "$src" && mkdir -p "$src/bin"
tar -xzf "$stage/source.tar.gz" -C "$src" --strip-components=1
install -m 0755 "$stage/pilldrops-linux-amd64" "$src/bin/pilldrops-linux"

{{- if eq .Runtime "firecracker"}}
say "firecracker (kernel, networking, base template; the first run takes a while)"
(cd "$src" && BUILD_DIR="$src" TEMPLATES=base bash scripts/provision-node.sh) >/dev/null
{{- else}}
say "docker"
command -v docker >/dev/null 2>&1 || apt-get -o DPkg::Lock::Timeout=300 install -y -qq docker.io >/dev/null
systemctl enable --now docker >/dev/null 2>&1
bash "$src/scripts/setup-docker-egress.sh" >/dev/null
for u in sandboxd.service sandbox-egress-restore.service; do
  install -m 0644 "$src/packaging/$u" "/etc/systemd/system/$u"
done
# The docker runtime bind-mounts both into every sandbox.
install -D -m 0755 "$stage/envd-linux-amd64" /opt/pilldrops/bin/envd
install -D -m 0755 "$stage/pilldrops-linux-amd64" /opt/pilldrops/bin/pilldrops-linux
systemctl daemon-reload
systemctl enable sandbox-egress-restore.service >/dev/null 2>&1
{{- end}}
{{- end}}

say "firewall"
# The node listens on every address, IPv6 included; only the control host,
# over IPv4, may reach it.
iptables -N PILLDROPS-NODE 2>/dev/null || true
iptables -F PILLDROPS-NODE
iptables -A PILLDROPS-NODE -i lo -j RETURN
iptables -A PILLDROPS-NODE -s {{.ControlIP}} -p tcp --dport 18080 -j RETURN
iptables -A PILLDROPS-NODE -p tcp --dport 18080 -j DROP
iptables -C INPUT -j PILLDROPS-NODE 2>/dev/null || iptables -I INPUT -j PILLDROPS-NODE
ip6tables -N PILLDROPS-NODE 2>/dev/null || true
ip6tables -F PILLDROPS-NODE
ip6tables -A PILLDROPS-NODE -i lo -j RETURN
ip6tables -A PILLDROPS-NODE -p tcp --dport 18080 -j DROP
ip6tables -C INPUT -j PILLDROPS-NODE 2>/dev/null || ip6tables -I INPUT -j PILLDROPS-NODE
# sandbox-egress-restore.service restores both files at boot, egress rules included.
mkdir -p /etc/iptables
iptables-save > /etc/iptables/rules.v4
ip6tables-save > /etc/iptables/rules.v6

{{- if .Token}}

say "join"
umask 077
printf '%s' {{sq .Token}} > "$stage/token"
umask 022
curl -fsSL {{sq .OrchestratorURL}}/install.sh -o "$stage/join.sh"
JOIN_TOKEN_FILE="$stage/token" ORCHESTRATOR_URL={{sq .OrchestratorURL}} NODE_ZONE={{sq .Zone}} \
  ADVERTISE_URL={{sq .AdvertiseURL}} sh "$stage/join.sh" >/dev/null
{{- end}}

restart=
{{- if .Upgrade}}

say "upgrade to {{.Tag}}"
base64 -d > "$stage/SHA256SUMS" <<'SUMS'
{{.SumsB64}}
SUMS
files="sandboxd-fleet-linux-amd64 envd-linux-amd64 pilldrops-linux-amd64"
for f in $files; do curl -fsSL {{sq .ReleaseBase}}"$f" -o "$stage/$f"; done
(cd "$stage" && grep -E " \*?($(echo "$files" | tr ' ' '|'))\$" SHA256SUMS | sha256sum -c --quiet -)
# install replaces each file, so the running binaries are untouched until the restart.
install -m 0755 "$stage/sandboxd-fleet-linux-amd64" /opt/pilldrops/bin/sandboxd-linux
install -m 0755 "$stage/envd-linux-amd64" /opt/pilldrops/bin/envd
install -m 0755 "$stage/pilldrops-linux-amd64" /opt/pilldrops/bin/pilldrops-linux
restart=1
{{- end}}

# The join writes the node's identity; previews also need the domain, which
# it does not carry.
if ! grep -qx 'SANDBOXD_SANDBOX_DOMAIN={{.Domain}}' /etc/sandboxd.env; then
  sed -i '/^SANDBOXD_SANDBOX_DOMAIN=/d' /etc/sandboxd.env
  echo 'SANDBOXD_SANDBOX_DOMAIN={{.Domain}}' >> /etc/sandboxd.env
  restart=1
fi
# A restart pauses the node's sandboxes and resumes them when it is back.
[ -z "$restart" ] || systemctl restart sandboxd
