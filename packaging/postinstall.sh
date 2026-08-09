#!/bin/sh
set -e

service_user=https_proxy
service_group=https_proxy

if ! getent group "$service_group" >/dev/null 2>&1; then
  groupadd --system "$service_group"
fi

if ! id -u "$service_user" >/dev/null 2>&1; then
  nologin_shell=$(command -v nologin || printf '%s' /usr/sbin/nologin)
  useradd --system --gid "$service_group" --no-create-home \
    --home-dir /nonexistent --shell "$nologin_shell" "$service_user"
fi

install -d -o root -g "$service_group" -m 0750 /etc/https_proxy
install -d -o root -g "$service_group" -m 0750 /etc/https_proxy/certs
if [ -f /etc/https_proxy/config.yaml ]; then
  chown root:"$service_group" /etc/https_proxy/config.yaml
  chmod 0640 /etc/https_proxy/config.yaml
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload || true
  systemctl enable --now https_proxy.service || true
fi
