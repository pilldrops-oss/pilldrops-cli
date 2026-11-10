set -euo pipefail
# One node: set the host up for its runtime, allow the node port only from the
# control host, join the fleet with a one-time token, and give it the preview
# domain. Rendered by apply.go and run as root over ssh.
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
# ssh forwards the operator's locale, which a fresh host may not have.
export LC_ALL=C.UTF-8
say() { printf '   %s\n' "$*"; }

say "packages"
apt-get update -qq
apt-get install -y -qq curl ca-certificates tar iptables >/dev/null

say "release {{.Tag}}"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
base64 -d > "$stage/SHA256SUMS" <<'SUMS'
{{.SumsB64}}
SUMS
files="envd-linux-amd64 pilldrops-linux-amd64"
for f in $files; do curl -fsSL {{sq .ReleaseBase}}"$f" -o "$stage/$f"; done
(cd "$stage" && grep -E " \*?($(echo "$files" | tr ' ' '|'))\$" SHA256SUMS | sha256sum -c --quiet -)
src=/opt/pilldrops/src
curl -fsSL {{sq .SourceURL}} -o "$stage/source.tar.gz"
rm -rf "$src" && mkdir -p "$src/bin"
tar -xzf "$stage/source.tar.gz" -C "$src" --strip-components=1
install -m 0755 "$stage/pilldrops-linux-amd64" "$src/bin/pilldrops-linux"

{{- if eq .Runtime "firecracker"}}
say "firecracker (kernel, networking, base template; the first run takes a while)"
(cd "$src" && BUILD_DIR="$src" TEMPLATES=base bash scripts/provision-node.sh) >/dev/null
{{- else}}
say "docker"
command -v docker >/dev/null 2>&1 || apt-get install -y -qq docker.io >/dev/null
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

say "firewall"
iptables -N PILLDROPS-NODE 2>/dev/null || true
iptables -F PILLDROPS-NODE
iptables -A PILLDROPS-NODE -i lo -j RETURN
iptables -A PILLDROPS-NODE -s {{.ControlIP}} -p tcp --dport 18080 -j RETURN
iptables -A PILLDROPS-NODE -p tcp --dport 18080 -j DROP
iptables -C INPUT -j PILLDROPS-NODE 2>/dev/null || iptables -I INPUT -j PILLDROPS-NODE
# sandbox-egress-restore.service restores this file at boot, egress rules included.
mkdir -p /etc/iptables && iptables-save > /etc/iptables/rules.v4

say "join"
umask 077
printf '%s' {{sq .Token}} > "$stage/token"
umask 022
curl -fsSL {{sq .OrchestratorURL}}/install.sh -o "$stage/join.sh"
JOIN_TOKEN_FILE="$stage/token" ORCHESTRATOR_URL={{sq .OrchestratorURL}} NODE_ZONE={{sq .Zone}} \
  ADVERTISE_URL={{sq .AdvertiseURL}} sh "$stage/join.sh" >/dev/null

# The join writes the node's identity; previews also need the domain, which
# it does not carry.
sed -i '/^SANDBOXD_SANDBOX_DOMAIN=/d' /etc/sandboxd.env
echo 'SANDBOXD_SANDBOX_DOMAIN={{.Domain}}' >> /etc/sandboxd.env
systemctl restart sandboxd
