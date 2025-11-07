#!/bin/sh
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop https_proxy.service || true
  systemctl disable https_proxy.service || true
  systemctl daemon-reload || true
fi
