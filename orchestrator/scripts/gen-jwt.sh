#!/bin/sh
# Generate a fresh 32-byte JWT on tmpfs. Idempotent within a compose-up cycle.
set -eu

JWT_PATH="${JWT_PATH:-/run/jwt/jwt.hex}"
JWT_DIR="$(dirname "$JWT_PATH")"

mkdir -p "$JWT_DIR"
if [ ! -s "$JWT_PATH" ]; then
  # Prefer openssl if available; fall back to /dev/urandom + xxd or od.
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32 > "$JWT_PATH"
  elif command -v xxd >/dev/null 2>&1; then
    head -c 32 /dev/urandom | xxd -p -c 64 > "$JWT_PATH"
  else
    head -c 32 /dev/urandom | od -An -v -tx1 | tr -d ' \n' > "$JWT_PATH"
  fi
  echo "gen-jwt: wrote 64-hex-char JWT to $JWT_PATH"
else
  echo "gen-jwt: existing JWT at $JWT_PATH; reapplying chown/chmod"
fi

# Always (re)apply ownership + perms so a restart can fix a mismatched
# orchestrator UID or earlier 644 file. Owner = orchestrator user (uid 10000).
chown 10000:10000 "$JWT_PATH" 2>/dev/null || true
chmod 600 "$JWT_PATH"
