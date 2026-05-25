#!/usr/bin/env sh
set -eu

warn() {
  printf '%s\n' "$*" >&2
}

if rg -n '\b(session|turn|quota|billing|provider|model)\b' common/wsconn >/tmp/wsconn-advisory-business.txt; then
  warn 'warning: common/wsconn contains transport-boundary advisory business words:'
  sed -n '1,40p' /tmp/wsconn-advisory-business.txt >&2
fi

if rg -n 'response\.create|response\.completed|response\.failed' common/wsconn >/tmp/wsconn-advisory-protocol.txt; then
  warn 'warning: common/wsconn contains protocol event strings:'
  sed -n '1,40p' /tmp/wsconn-advisory-protocol.txt >&2
fi

if rg -n 'wsconn\.CloseCode\(' relay providers >/tmp/wsconn-advisory-closecode.txt; then
  warn 'warning: relay/providers contain direct wsconn.CloseCode casts; use wsconn.SanitizeWireCloseCode:'
  sed -n '1,40p' /tmp/wsconn-advisory-closecode.txt >&2
fi
