set -euo pipefail
# The control host: Postgres, the orchestrator, the node binaries it serves to
# joining nodes, Caddy with the wildcard certificate, and a firewall that keeps
# the orchestrator and Postgres off the internet. Rendered by apply.go and run
# as root over ssh; every step can run again.
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
# ssh forwards the operator's locale, which a fresh host may not have.
export LC_ALL=C.UTF-8
say() { printf '   %s\n' "$*"; }

say "packages"
echo 'iptables-persistent iptables-persistent/autosave_v4 boolean false' | debconf-set-selections
echo 'iptables-persistent iptables-persistent/autosave_v6 boolean false' | debconf-set-selections
apt-get update -qq
apt-get install -y -qq curl ca-certificates tar postgresql caddy iptables-persistent{{if eq .TLS.Mode "cloudflare"}} certbot python3-certbot-dns-cloudflare{{end}} >/dev/null

say "release {{.Tag}}"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
base64 -d > "$stage/SHA256SUMS" <<'SUMS'
{{.SumsB64}}
SUMS
files="orchestrator-linux-amd64 sandboxd-fleet-linux-amd64 envd-linux-amd64 pilldrops-join-linux-amd64"
for f in $files; do curl -fsSL {{sq .ReleaseBase}}"$f" -o "$stage/$f"; done
(cd "$stage" && grep -E " \*?($(echo "$files" | tr ' ' '|'))\$" SHA256SUMS | sha256sum -c --quiet -)
curl -fsSL {{sq .SourceURL}} -o "$stage/source.tar.gz"
rm -rf /opt/pilldrops/src && mkdir -p /opt/pilldrops/src
tar -xzf "$stage/source.tar.gz" -C /opt/pilldrops/src --strip-components=1

# The orchestrator serves these to nodes under /bin/; a node's install.sh
# checks them against this SHA256SUMS.
install -d -m 0755 /opt/pilldrops/bin /opt/pilldrops/node-bin
install -m 0755 "$stage/orchestrator-linux-amd64" /opt/pilldrops/bin/orchestrator
install -m 0644 "$stage/sandboxd-fleet-linux-amd64" /opt/pilldrops/node-bin/sandboxd-linux
install -m 0644 "$stage/envd-linux-amd64" /opt/pilldrops/node-bin/envd
install -m 0644 "$stage/pilldrops-join-linux-amd64" /opt/pilldrops/node-bin/pilldrops-join-linux
(cd /opt/pilldrops/node-bin && sha256sum sandboxd-linux envd pilldrops-join-linux > SHA256SUMS)

say "firewall"
# Before the orchestrator starts: it and Postgres listen on every address,
# IPv6 included, and only loopback and the nodes (over IPv4) may reach them.
for ipt in iptables ip6tables; do
  $ipt -N PILLDROPS-IN 2>/dev/null || true
  $ipt -F PILLDROPS-IN
  $ipt -A PILLDROPS-IN -i lo -j RETURN
done
{{- range .Nodes}}
iptables -A PILLDROPS-IN -s {{.PrivateIP}} -p tcp --dport 5432 -j RETURN
{{- end}}
for ipt in iptables ip6tables; do
  $ipt -A PILLDROPS-IN -p tcp --dport 9090 -j DROP
  $ipt -A PILLDROPS-IN -p tcp --dport 5432 -j DROP
  $ipt -C INPUT -j PILLDROPS-IN 2>/dev/null || $ipt -I INPUT -j PILLDROPS-IN
done
netfilter-persistent save >/dev/null 2>&1

say "postgres"
pgconf=$(ls -d /etc/postgresql/*/main | tail -1)
echo "listen_addresses = 'localhost,{{.Control.PrivateIP}}'" > "$pgconf/conf.d/pilldrops.conf"
sed -i '/# pilldrops-begin/,/# pilldrops-end/d' "$pgconf/pg_hba.conf"
{
  echo '# pilldrops-begin'
{{- range .Nodes}}
  echo 'hostssl pilldrops pilldrops_node {{.PrivateIP}}/32 scram-sha-256'
{{- end}}
  echo '# pilldrops-end'
} >> "$pgconf/pg_hba.conf"
systemctl restart postgresql
sudo -u postgres psql -q -v ON_ERROR_STOP=1 <<'SQL'
SELECT 'CREATE ROLE pilldrops LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pilldrops') \gexec
ALTER ROLE pilldrops PASSWORD '{{.Secrets.POSTGRES_PASSWORD}}';
SELECT 'CREATE DATABASE pilldrops OWNER pilldrops' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'pilldrops') \gexec
SQL

say "orchestrator"
install -d -m 0700 /etc/pilldrops
umask 077
cat > /etc/pilldrops/orchestrator.env <<'ENV'
PORT=9090
SANDBOXD_DATABASE_URL=postgres://pilldrops:{{.Secrets.POSTGRES_PASSWORD}}@127.0.0.1:5432/pilldrops?sslmode=require
ORCHESTRATOR_API_KEY={{.Secrets.ORCHESTRATOR_API_KEY}}
ORCHESTRATOR_TOKEN={{.Secrets.ORCHESTRATOR_TOKEN}}
NODE_KEY_SECRET={{.Secrets.NODE_KEY_SECRET}}
ORG_ASSERT_KEY={{.Secrets.ORG_ASSERT_KEY}}
VAULT_SECRET_KEY={{.Secrets.VAULT_SECRET_KEY}}
SANDBOXD_SECRET_KEY={{.Secrets.SANDBOXD_SECRET_KEY}}
NODE_BOOTSTRAP_SECRET={{.Secrets.SANDBOXD_SECRET_KEY}}
NODE_BOOTSTRAP_DSN=postgres://pilldrops_node:{{.Secrets.NODE_DB_PASSWORD}}@{{.Control.PrivateIP}}:5432/pilldrops?sslmode=require
NODE_BIN_DIR=/opt/pilldrops/node-bin
PORTAL_SANDBOX_DOMAIN={{.Domain}}
TRUSTED_PROXY_CIDRS=127.0.0.1/32
ORCHESTRATOR_ARTIFACT_DIR=/var/lib/pilldrops/artifacts
ENV
umask 022
cat > /etc/systemd/system/pilldrops-orchestrator.service <<'UNIT'
[Unit]
Description=PillDrops control plane
After=network-online.target postgresql.service
Wants=network-online.target
Requires=postgresql.service

[Service]
EnvironmentFile=/etc/pilldrops/orchestrator.env
ExecStart=/opt/pilldrops/bin/orchestrator
Restart=on-failure
RestartSec=3
DynamicUser=yes
StateDirectory=pilldrops
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable pilldrops-orchestrator >/dev/null 2>&1
systemctl restart pilldrops-orchestrator
i=0
until curl -fs --max-time 2 -o /dev/null http://127.0.0.1:9090/health; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then journalctl -u pilldrops-orchestrator -n 30 --no-pager >&2; exit 1; fi
  sleep 2
done

# Runs after the orchestrator has migrated the schema, which it grants on.
{ echo "\\set node_password '{{.Secrets.NODE_DB_PASSWORD}}'"; cat /opt/pilldrops/src/scripts/postgres-node-role.sql; } |
  sudo -u postgres PGOPTIONS='-c client_min_messages=warning' psql -q -d pilldrops >/dev/null

# Claim the console over loopback before Caddy publishes the domain: an
# unclaimed one lets its first visitor create the administrator.
umask 077
printf '%s' {{sq .AdminEmail}} > "$stage/email"
printf '%s' '{{.Secrets.ADMIN_PASSWORD}}' > "$stage/password"
umask 022
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9090/dashboard/setup \
  --data-urlencode "email@$stage/email" --data-urlencode "password@$stage/password" \
  --data-urlencode "confirm@$stage/password")
case "$code" in
  2??|3??) say "console claimed for "{{sq .AdminEmail}} ;;
  409) ;; # claimed on an earlier run
  *) echo "claiming the console failed with HTTP $code" >&2; exit 1 ;;
esac

say "certificate"
install -d -m 0750 -o root -g caddy /etc/caddy/certs
{{- if eq .TLS.Mode "cloudflare"}}
install -d -m 0700 /etc/letsencrypt
umask 077
printf 'dns_cloudflare_api_token = %s\n' {{sq .CloudflareToken}} > /etc/letsencrypt/pilldrops-cloudflare.ini
umask 022
install -d /etc/letsencrypt/renewal-hooks/deploy
cat > /etc/letsencrypt/renewal-hooks/deploy/pilldrops-caddy.sh <<'HOOK'
#!/bin/sh
# Caddy runs as caddy and cannot read /etc/letsencrypt/live.
install -m 0640 -o root -g caddy /etc/letsencrypt/live/pilldrops/fullchain.pem /etc/caddy/certs/fullchain.pem
install -m 0640 -o root -g caddy /etc/letsencrypt/live/pilldrops/privkey.pem /etc/caddy/certs/privkey.pem
systemctl reload caddy 2>/dev/null || true
HOOK
chmod 0755 /etc/letsencrypt/renewal-hooks/deploy/pilldrops-caddy.sh
certbot certonly -n -q --agree-tos -m {{sq .TLS.Email}} --cert-name pilldrops --keep-until-expiring \
  --dns-cloudflare --dns-cloudflare-credentials /etc/letsencrypt/pilldrops-cloudflare.ini \
  -d {{sq .Domain}} -d {{sq (print "*." .Domain)}}
/etc/letsencrypt/renewal-hooks/deploy/pilldrops-caddy.sh
{{- else}}
base64 -d > /etc/caddy/certs/fullchain.pem <<'PEM'
{{.CertB64}}
PEM
umask 077
base64 -d > /etc/caddy/certs/privkey.pem <<'PEM'
{{.KeyB64}}
PEM
umask 022
chown root:caddy /etc/caddy/certs/fullchain.pem /etc/caddy/certs/privkey.pem
chmod 0640 /etc/caddy/certs/fullchain.pem /etc/caddy/certs/privkey.pem
{{- end}}

say "caddy"
cat > /etc/caddy/Caddyfile <<'CADDY'
{
	on_demand_tls {
		ask http://127.0.0.1:9090/v1/domains/check
	}
}

{{.Domain}}, *.{{.Domain}} {
	tls /etc/caddy/certs/fullchain.pem /etc/caddy/certs/privkey.pem
	reverse_proxy 127.0.0.1:9090
}

# Customer domains: a certificate per host, only for hosts the platform approves.
:443 {
	tls {
		on_demand
	}
	reverse_proxy 127.0.0.1:9090
}
CADDY
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 ||
  { caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile; exit 1; }
systemctl enable caddy >/dev/null 2>&1
systemctl restart caddy
