#!/bin/sh
# Best-effort shred on compose-down. tmpfs backing makes this belt-and-suspenders.
set -eu

JWT_PATH="${JWT_PATH:-/run/jwt/jwt.hex}"

if [ ! -e "$JWT_PATH" ]; then
  exit 0
fi

if command -v shred >/dev/null 2>&1; then
  shred -u "$JWT_PATH" 2>/dev/null || rm -f "$JWT_PATH"
else
  rm -f "$JWT_PATH"
fi

echo "shred-jwt: removed $JWT_PATH"
