#!/bin/sh
# Serves 9P over WebSocket and the image files on port 8080, with images in
# R2. The Worker passes the settings in as environment variables.
# H9CACHE_STORE, if set, names another store in place of R2.
set -e
export AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
store="$H9CACHE_STORE"
if [ -z "$store" ]; then
	: "${R2_BUCKET:?R2_BUCKET is not set}"
	: "${R2_ACCOUNT_ID:?R2_ACCOUNT_ID is not set}"
	store="s3://$R2_BUCKET?endpoint=https://$R2_ACCOUNT_ID.r2.cloudflarestorage.com&region=auto&path-style=true"
fi

set -- serve --store "$store" --listen 'http!*!8080' \
	--disk-cache /var/cache/h9cache --disk-cache-mb "${DISK_CACHE_MB:-2048}"
if [ -n "$H9CACHE_TOKENS" ]; then
	printf '%s\n' "$H9CACHE_TOKENS" > /tmp/tokens
	set -- "$@" --tokens /tmp/tokens
fi
exec h9cache "$@" $H9CACHE_FLAGS
