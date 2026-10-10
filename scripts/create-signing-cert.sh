#!/usr/bin/env bash
# Creates a self-signed code-signing identity in the login keychain.
#
# Why: macOS privacy grants (Full Disk Access, Automation) are bound to a
# binary's code signature. Ad-hoc signatures change on every build, so grants
# are lost after each rebuild. Signing with a stable certificate keeps the
# designated requirement (identifier + certificate) constant.
set -euo pipefail

NAME="${1:-imsgd Local Code Signing}"
KEYCHAIN="${KEYCHAIN:-$HOME/Library/Keychains/login.keychain-db}"

if security find-certificate -c "$NAME" "$KEYCHAIN" >/dev/null 2>&1; then
  echo "Certificate \"$NAME\" already exists in $KEYCHAIN"
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/req.cnf" <<CNF
[req]
distinguished_name = dn
x509_extensions    = ext
prompt             = no
[dn]
CN = $NAME
[ext]
basicConstraints = critical,CA:false
keyUsage         = critical,digitalSignature
extendedKeyUsage = critical,codeSigning
CNF

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -config "$tmp/req.cnf" \
  -keyout "$tmp/key.pem" -out "$tmp/cert.pem" 2>/dev/null

# OpenSSL 3 defaults to PKCS#12 algorithms macOS `security` cannot import.
legacy=""
if openssl pkcs12 -help 2>&1 | grep -q -- '-legacy'; then legacy="-legacy"; fi
openssl pkcs12 -export $legacy -inkey "$tmp/key.pem" -in "$tmp/cert.pem" \
  -name "$NAME" -out "$tmp/id.p12" -passout pass:imsgd

security import "$tmp/id.p12" -k "$KEYCHAIN" -P imsgd -T /usr/bin/codesign

echo
echo "Created signing identity \"$NAME\". It is self-signed, so"
echo "'security find-identity -v' will not list it as trusted; codesign can still use it."
security find-identity -p codesigning "$KEYCHAIN" | grep "$NAME" || true
