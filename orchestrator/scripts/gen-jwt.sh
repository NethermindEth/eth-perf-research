#!/bin/sh
# Generate a fresh 32-byte JWT on tmpfs. Idempotent within a compose-up cycle.
set -eu

JWT_PATH="${JWT_PATH:-/run/jwt/jwt.hex}"
JWT_DIR="$(dirname "$JWT_PATH")"

mkdir -p "$JWT_DIR"
if [ -s "$JWT_PATH" ]; then
  echo "gen-jwt: existing JWT at $JWT_PATH; not regenerating"
  exit 0
fi

# Prefer openssl if available; fall back to /dev/urandom + xxd or od.
if command -v openssl >/dev/null 2>&1; then
  openssl rand -hex 32 > "$JWT_PATH"
elif command -v xxd >/dev/null 2>&1; then
  head -c 32 /dev/urandom | xxd -p -c 64 > "$JWT_PATH"
else
  head -c 32 /dev/urandom | od -An -v -tx1 | tr -d ' \n' > "$JWT_PATH"
fi

chmod 600 "$JWT_PATH"
echo "gen-jwt: wrote 64-hex-char JWT to $JWT_PATH"
