#!/bin/sh
set -e

if [ -f /etc/https_proxy/config.yaml ]; then
  chmod 600 /etc/https_proxy/config.yaml
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload || true
  systemctl enable --now https_proxy.service || true
fi
