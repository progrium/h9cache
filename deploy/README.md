# Deploying to Cloudflare Containers

This directory deploys h9cache as a Cloudflare Worker in front of one
container, with images kept in an R2 bucket. The Worker passes every
request, WebSocket upgrades included, to the container, which serves 9P
over WebSocket and the image files on port 8080.

- `Dockerfile` builds h9cache from this repository for `linux/amd64`.
- `entrypoint.sh` points h9cache at R2 and writes the tokens file.
- `src/index.ts` is the Worker and the container's class.
- `wrangler.jsonc` configures both.

## Setup

You need Node, Docker (wrangler builds the image locally), and a
Cloudflare account with Containers enabled.

1. Create a bucket:

   ```sh
   npm install
   npx wrangler r2 bucket create h9cache
   ```

2. In the dashboard, under R2, create an API token with Object Read & Write
   on that bucket. Note its access key ID and secret.

3. Set `R2_ACCOUNT_ID` in `wrangler.jsonc` to your account ID, and
   `R2_BUCKET` if you named the bucket something else.

4. Store the token, and the client tokens, as secrets:

   ```sh
   npx wrangler secret put R2_ACCESS_KEY_ID
   npx wrangler secret put R2_SECRET_ACCESS_KEY
   npx wrangler secret put H9CACHE_TOKENS
   ```

   `H9CACHE_TOKENS` is the contents of a `--tokens` file, one token and its
   repository patterns per line (see the main README). Leave it unset only if
   anyone may pull any image through your cache.

5. Deploy:

   ```sh
   npx wrangler deploy
   ```

Then attach an image with the token:

```sh
h9 --root wss://h9cache.SUBDOMAIN.workers.dev/alpine:3.20 \
   --root-header 'Authorization: Bearer TOKEN' sh
```

## Settings

- **`H9CACHE_FLAGS`**, a var or secret, adds flags to `h9cache serve`, such
  as `--tag-ttl 1h`.
- **`H9CACHE_STORE`** names another store in place of R2, any `--store`
  value, such as an S3 bucket elsewhere. Its credentials still come from
  `R2_ACCESS_KEY_ID` and `R2_SECRET_ACCESS_KEY`.
- **`instance_type`** in `wrangler.jsonc` is `standard-1` (½ vCPU, 4 GiB of
  memory, and 8 GB of disk). A larger type pulls big images faster.
- **`sleepAfter`** in `src/index.ts` is 30 minutes. A sleeping container
  starts again on the next request, which takes a few seconds and loads the
  pack indexes from R2.

The container's disk is ephemeral, so the disk cache (`DISK_CACHE_MB`,
2048 by default) starts empty after each sleep. Pulled images stay in R2.

## Garbage collection

The container doesn't run collections. Run one from anywhere with the same
R2 credentials:

```sh
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... h9cache gc \
  --store 's3://h9cache?endpoint=https://ACCOUNT_ID.r2.cloudflarestorage.com&region=auto&path-style=true'
```

It's safe while the container serves, as with any instance sharing a store.
