#!/bin/sh
# Creates a self-signed code-signing identity in the login keychain so that
# macOS microphone (and other TCC) permissions granted to mirrin persist
# across rebuilds. Unsigned/ad-hoc binaries are keyed by content hash, which
# changes every build; a certificate-based requirement does not.
set -e
NAME=${1:-mirrin-dev}
if security find-identity -v -p codesigning 2>/dev/null | grep -q "\"$NAME\""; then
  echo "identity $NAME already exists"; exit 0
fi
TMP=$(mktemp -d)
cat > "$TMP/cs.cnf" <<CNF
[req]
distinguished_name = dn
x509_extensions = ext
prompt = no
[dn]
CN = $NAME
[ext]
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, codeSigning
basicConstraints = critical, CA:false
CNF
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$TMP/cs.key" -out "$TMP/cs.crt" -days 3650 -config "$TMP/cs.cnf" 2>/dev/null
openssl pkcs12 -export -legacy -inkey "$TMP/cs.key" -in "$TMP/cs.crt" -out "$TMP/cs.p12" -passout pass:mirrin -name "$NAME" 2>/dev/null
security import "$TMP/cs.p12" -k ~/Library/Keychains/login.keychain-db -P mirrin -T /usr/bin/codesign -A
security add-trusted-cert -r trustRoot -p codeSign -k ~/Library/Keychains/login.keychain-db "$TMP/cs.crt"
rm -rf "$TMP"
security find-identity -v -p codesigning | grep "$NAME" && echo "done: run make install"
