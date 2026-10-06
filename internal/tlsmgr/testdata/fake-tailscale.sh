#!/bin/sh
set -eu
case "$1" in
  status) cat status.json ;;
  cert)
    if [ -f fail-renew ]; then exit 1; fi
    [ "$2" = "--cert-file" ]
    [ "$4" = "--key-file" ]
    cp issued-cert.pem "$3"
    cp issued-key.pem "$5"
    printf 'issued\n' >> issues
    ;;
  *) exit 1 ;;
esac
