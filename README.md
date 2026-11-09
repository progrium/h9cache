# h9cache

A container registry cache that serves images as 9P trees. A client attaches
with an image reference as the attach spec and gets that image's merged
filesystem as its root. The first attach of an image pulls and converts it,
and every later attach is served from the store.

```sh
h9cache serve --store /var/lib/h9cache --listen 'tcp!*!564'
h9 --root 'tcp!localhost!564' --root-spec alpine:3.20 --ns linux.ns sh
```

Or over WebSocket, where the URL path names the image:

```sh
h9cache serve --store /var/lib/h9cache --listen 'http!*!8564'
h9 --root ws://localhost:8564/alpine:3.20 --ns linux.ns sh
```

The design is in [Registry cache over 9P](https://claude.ai/artifact/LwfegkQMCtV3DDncZFcw76).

## How it works

- **Pull on a miss.** The attach spec is a reference (`alpine:3.20`,
  `ghcr.io/org/app@sha256:…`, or either with `!linux/arm64` to pick a
  platform). A tag is checked upstream with a `HEAD`, which Docker Hub doesn't
  count against its pull limit, at most once per `--tag-ttl`. A digest never
  goes upstream once it's pulled. If upstream is down, a cached tag is used at
  its last resolution.
- **Files, not tarballs.** Each layer is unpacked once into a layer tree. File
  contents are chunked with [fvs core](https://github.com/fvs-lab/core)'s
  content-defined chunker and stored by BLAKE3, so the same file in two tags,
  two images, or two layers is stored once.
- **One merged tree per image.** Layers are merged at pull time, with
  whiteouts and opaque directories applied, and written as Merkle directory
  objects, so images that share a subtree share its objects.
- **Packs on an object store.** Chunks are written in packs of about 32 MB,
  each chunk compressed on its own, with a small index object per pack. A
  chunk read is one ranged GET, however small the chunk, and the pack indexes
  are loaded at startup. Any number of instances can share one bucket.
- **Read-only 9P2000 and 9P2000.L.** Walks and stats read tree objects from a
  memory cache, and reads fetch only the chunks they need, through a memory
  cache and an optional disk cache, reading the next few chunks ahead. Writes
  get "read-only file system", or `EROFS` over 9P2000.L.

### Files the server adds

Each image root also holds two read-only files that aren't in the image:

- **`/.h9index`** lists every name in the image with its qid, mode, length,
  mtime, and owner, in the text format of hyper9's
  [mount-index](https://github.com/progrium/hyper9/blob/main/design/mount-index.md)
  proposal, so h9 can answer walks and stats without asking. It lists the tree
  as the server serves it, so every entry matches the server's own stat. A
  symlink to a directory is listed but marked `!unindexed` rather than
  followed, and a name the format can't hold (one with a space or a newline)
  marks its directory `!unindexed`. The index is built the first time an
  image is served, or by `h9cache pull`, and stored under `index/`. An image
  never changes, so neither does its index.
- **`/.h9image`** is the image's identity and the config a runner needs, one
  `key value` line each, with the value as JSON:

  ```
  h9image 1
  manifest "sha256:…"
  platform "linux/amd64"
  env ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"]
  entrypoint []
  cmd ["/bin/sh"]
  workdir "/"
  user ""
  ```

### Symlinks

9P2000 has no symlinks, so the server follows them during walks, inside the
image: an absolute target starts at the image root, `..` stops there, and a
walk follows at most 40 links. Exec, open, and stat work through links. A
directory listing shows a link as its target under the link's name, and
leaves out dangling links. What's lost is `readlink`, so `ls -l` shows no
arrows. Over 9P2000.L, links are real (see below).

### 9P2000.L

A client that asks for 9P2000.L in its `Tversion` gets it: Linux's v9fs
(`mount -t 9p -o trans=tcp,version=9p2000.L,aname=alpine:3.20 HOST /mnt`),
QEMU, and other .L clients. The same image is served with Linux's
semantics:

- **Symlinks are symlinks.** A walk stops at one, `Treadlink` returns its
  target as the layer gave it, and the client resolves it, as Linux does.
  Listings show links as links, dangling ones too.
- **Owners are numbers**, the uid and gid from the layer, and modes keep
  their setuid, setgid, and sticky bits.
- **Device nodes and FIFOs** have their type and device numbers, so a Linux
  client makes `/dev/null` its own null device. Images pulled before device
  numbers were kept serve devices as 0, 0.
- **Not served:** extended attributes (`Txattrwalk` is `EOPNOTSUPP`, which
  Linux takes as none). Locks are always granted, since nothing changes.
  `Tstatfs` reports no free space.

h9 asks for `9P2000.L.Z` and gets 9P2000, which its translator passes
through. 9P2000 gives h9 owner names and whole directories in one read,
where .L would cost it a round trip per entry.

### Other details

- **Qids** are derived, not stored: the first 8 bytes of BLAKE3 over the image
  tree id and the file's canonical path. Every connection and every server
  instance gives a file the same qid. Hardlinked names share a qid, and so do a
  symlink and its target.
- **Owners** are shown as names from the image's own `/etc/passwd` and
  `/etc/group`, or as the number when there's no entry.
- **Device nodes and FIFOs** are listed but fail to open over 9P2000.
- **Mount points** that h9 binds onto (`/dev`, `/env`, `/fd`,
  `/mnt/term/cwd`, `/net`, `/proc`, `/srv`, and `/tmp`) are added to the image
  as empty directories when the image lacks them, since a 9P root is
  read-only and h9 needs `/mnt/term/cwd` for the working directory.
- **Not kept yet:** extended attributes, file capabilities included, are
  dropped. Setuid, setgid, and sticky bits are stored, and served only over
  9P2000.L (9P2000 has no bits for them).

## Running Linux images in h9

h9 treats a 9P root as a 9front root today: only a host directory is
detected as a Linux root. Until it detects Linux trees over 9P too, put the
image in the Linux view with a namespace file:

```sh
printf "bind -a / '#λ'\n" > linux.ns
h9 --root 'tcp!localhost!564' --root-spec alpine:3.20 --ns linux.ns /bin/sh -c 'cat /etc/os-release'
```

`--ns` is a guest path, so a relative name is found in the working directory.

## WebSocket and HTTP

An `http!HOST!PORT` or `https!HOST!PORT` listener serves:

| Request | Serves |
| --- | --- |
| WebSocket upgrade on `/IMAGE` | 9P, one binary message per 9P message, subprotocol `9p`. An attach with an empty spec attaches `IMAGE`; an attach that names a spec attaches that one. |
| `GET /IMAGE/.h9index` | The image's index, for h9's `--root-index` URL |
| `GET /IMAGE/.h9image` | The image's config, for prefetching |
| `GET /healthz` | `ok`, for load balancers |

`IMAGE` is written as on the command line, such as `alpine:3.20`,
`ghcr.io/org/app@sha256:…`, or `alpine:3.20!linux/arm64`. The metadata files
carry the platform manifest digest as their ETag. Fetched by digest, they're
cached for a year as immutable; fetched by tag, they're revalidated each
time. `https!` needs `--tls-cert` and `--tls-key`.

### Client tokens

`--tokens FILE` makes every HTTP request present a token, as
`Authorization: Bearer TOKEN` (h9's `--root-header`) or a `token` query
parameter, for namespace-file mounts that can't send headers. The file lists
each token and the repositories it may attach:

```
# comments and blank lines are ignored
3f9c0b7e5d1a42c8a6e1  docker.io/library/*  ghcr.io/acme/*
77ab5e10c4d94b2f9a02  *
```

A pattern matches `REGISTRY/REPOSITORY`, with Docker Hub written `docker.io`,
and `*` matches any run of characters, slashes included. Tokens are at least
16 characters. A missing or unknown token gets 401, and an image outside the
token's patterns gets 403 over HTTP or `permission denied` from the attach.

```sh
h9 --root ws://reg.example:8564/alpine:3.20 --root-header 'Authorization: Bearer 3f9c0b7e5d1a42c8a6e1' sh
```

Plain 9P has no way to present a token, so with `--tokens` h9cache refuses
`tcp!` listeners. A `unix!` socket is still allowed, guarded by its file
permissions. Tokens guard which images a client may attach, not upstream
credentials: those stay on the server either way.

### Several instances

Instances can share one store, such as an S3 bucket behind a load balancer.
Before an instance pulls an image, it claims the pull by creating
`pulling/DIGEST/PLATFORM` with a conditional put. Other instances that want
the same image wait for its record to appear instead of pulling it too. The
claimant rewrites its claim every quarter of `--claim-ttl` and deletes it
when done, so a claim left untouched for `--claim-ttl` belongs to an instance
that died, and the next waiter takes it over. A claim only saves work: what
a pull writes is content-addressed, so two instances that do pull the same
image at once store the same result.

## 9front roots

h9 runs a 9P root as a 9front root, so a 9front tree packaged as an image
needs nothing more:

```sh
h9 --root ws://localhost:8564/progrium/9front rc
```

**mkroot images.** Root images are built by repositories named
`mkroot-NAME`, such as [mkroot-9front](https://github.com/progrium/mkroot-9front),
and published as `ghcr.io/OWNER/mkroot-NAME`. A reference to `OWNER/NAME`
that names no registry, or names `ghcr.io`, is looked up as
`ghcr.io/OWNER/mkroot-NAME` first, so `progrium/9front` attaches
`ghcr.io/progrium/mkroot-9front`. When that doesn't exist, the reference is
pulled as written: `acme/app` is still Docker Hub's. Each answer is
remembered for `--tag-ttl`. Client tokens are checked against the repository
actually pulled. `--mkroot=false` turns the lookup off.

**Platforms.** A multi-platform image with no image for `linux/ARCH` falls
back to `plan9/ARCH`, so 9front images need no `!plan9/amd64`.

**Owners.** Plan 9 owners are names, not ids. A layer entry with zero ids
and an owner name other than `root` keeps its names, and the server reports
them as they are. Other entries are named through the image's `/etc/passwd`
and `/etc/group`.

## Commands

```
h9cache serve [flags]          serve images over 9P
h9cache pull [flags] IMAGE...  pull images into the store ahead of time
h9cache gc [flags]             delete images unused for --keep, and what only they used
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--store` | `$H9CACHE_STORE`, else `h9cache-data` | The object store: a directory, `dir:PATH`, or an `s3://` URL (below) |
| `--listen` | `tcp!*!564` | `tcp!HOST!PORT` or `unix!PATH` for 9P, `http!HOST!PORT` or `https!HOST!PORT` for WebSocket and HTTP; repeatable |
| `--tokens` | none | A file of client tokens; every HTTP request then needs one |
| `--tls-cert`, `--tls-key` | none | Certificate and key for `https!` listeners |
| `--mkroot` | on | Look `OWNER/NAME` and `ghcr.io/OWNER/NAME` up as `ghcr.io/OWNER/mkroot-NAME` first |
| `--mkroot-registry` | `ghcr.io` | Where `--mkroot` looks |
| `--claim-ttl` | `2m` | How long another instance's pull claim stands without being refreshed |
| `--platform` | `linux/` plus the build's architecture | Platform picked from multi-platform images |
| `--tag-ttl` | `5m` | How long a tag's resolution is trusted |
| `--cache-mb` | `256` | Memory for cached chunks |
| `--disk-cache` | none | A directory for a local cache of chunks, kept across restarts |
| `--disk-cache-mb` | `4096` | The disk cache's size |
| `--prefetch` | `4` | Chunks to read ahead after each read |
| `--keep` | `720h` (30 days) | `gc`: keep images pulled or attached within this long |
| `--dry-run` | off | `gc`: report what would be deleted, and delete nothing |
| `-v` | off | Debug logging |

Upstream credentials come from the Docker config (`~/.docker/config.json` and
its credential helpers). Clients never hold them.

## Store layout

Everything is an object under a key, so a directory and an object store share
one layout.

| Prefix | Key | Holds |
| --- | --- | --- |
| `packs/` | BLAKE3 of the pack | Chunks, each zstd-compressed when that's smaller, back to back |
| `packidx/` | pack id | Each chunk's id, offset, encoded length, and size in that pack |
| `chunks/` | BLAKE3 of the chunk | One chunk, from stores made before packs; still read |
| `trees/` | BLAKE3 of the object | One directory's sorted entries |
| `layers/` | layer blob digest | The layer's tree id and root attributes |
| `images/` | platform manifest digest | Image tree id, root attributes, platform, and config |
| `platforms/` | index digest and platform | The manifest digest picked from a multi-platform index |
| `refs/` | registry/repo/tag | Manifest or index digest, and when it was resolved |
| `index/v1/` | platform manifest digest | The image's `.h9index` text |
| `mkroot/` | reference as written | The mkroot reference it was found as, or none, and when it was checked |
| `pulling/` | top digest and platform | A pull in progress: the instance and when it last refreshed the claim |
| `used/` | platform manifest digest | When the image was last attached, written at most hourly per instance |
| `gc/` | `lock`, `epoch` | A collection in progress, and when the last one finished |

Only `refs/`, `mkroot/`, `pulling/`, `used/`, and `gc/` are ever overwritten. A pull writes one batch of packs per
layer and flushes it before recording the layer, so nothing refers to a chunk
that isn't stored. A pack is written before its index, so an index is only
ever visible for a whole pack. An instance that misses a chunk reloads the
pack list, at most once a second, to find packs other instances wrote.

### S3

```sh
export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…
h9cache serve --store 's3://my-bucket/h9cache?region=us-east-1'
h9cache serve --store 's3://h9cache?endpoint=http://localhost:9000&path-style=true'   # MinIO
```

The URL is `s3://BUCKET[/PREFIX]`, with optional `endpoint`, `region`, and
`path-style` query parameters for R2, GCS, MinIO, and other S3-compatible
servers. Credentials, and the region and endpoint when the URL doesn't give
them, come from the AWS SDK's usual sources: the environment, shared config
files, and instance roles. Checksums on responses aren't required, since a
ranged read can't be checked against a whole-object checksum; every chunk is
checked against its own BLAKE3 id instead.

`go test ./store/s3` runs against an in-process S3 server. Set
`H9CACHE_TEST_S3` to an `s3://` URL to also run the store tests against a real
bucket.

### ZeroFS

The directory store works over any POSIX mount, a ZeroFS mount included, with
no extra code. That's reasonable for a single cache next to a ZeroFS home
server. With more than one instance, use S3 directly: ZeroFS has one writer,
its read replicas lag the writer, and its compression and GC duplicate
h9cache's own.

## Garbage collection

```sh
h9cache gc --store s3://my-bucket/h9cache --dry-run
h9cache gc --store s3://my-bucket/h9cache --keep 720h
```

`h9cache gc` deletes images that weren't pulled or attached within `--keep`,
then everything only they used:

1. **Records:** each unused image's record, index, and use record go first,
   so nothing finds an image whose trees are going. Platform records that
   name a deleted image go too, as do tag resolutions and mkroot lookups
   older than `--keep`, which only cost a lookup to redo. Layer records go
   unless a kept image lists the layer. Image records from before images
   listed their layers keep every layer record until they're gone.
2. **Mark:** every tree and chunk reachable from the kept images and layer
   records.
3. **Sweep:** trees and loose chunks that aren't marked, and packs with no
   marked chunk. A pack that's less than half live is rewritten: its live
   chunks go into new packs, then it's deleted.

It runs beside serving instances. A pull can reuse any stored chunk, so a
collection and pulls take turns: the collection creates `gc/lock`, then waits
until no pull claim is fresh, and a pull that finds the lock after taking its
claim lets the claim go and waits. When it's done, the collection writes
`gc/epoch`. An instance reloads its pack indexes before its next pull if a
collection finished after it last loaded them, and whenever a read finds its
pack gone, so a rewritten pack moves chunks without breaking reads.

An image that's unused for `--keep` and then attached while a collection is
marking can be deleted under that attach. With a `--keep` of days, that's
rare, and the next attach pulls the image again.

## Layout

```
cmd/h9cache/   main: flags, store, listeners
store/       object store interface
  dir/       directory backend
  s3/        S3 backend
  storetest/ tests every backend runs
cas/         chunking over fvs core, packs, and the chunk caches
tree/        tree objects, layer merge, symlink resolution, qids
pull/        reference parsing, tag resolution, layer conversion, pull claims
imagefs/     an image as a 9P file tree, with /.h9index and /.h9image
index/       the .h9index text format
ninep/       9P2000 and 9P2000.L server (and a small client for tests)
web/         9P over WebSocket, and the metadata files over HTTP
access/      the client tokens file
gc/          garbage collection
```

Dependencies run one way: `imagefs` → `tree` → `cas` → `store`, with `pull`
writing through `tree` and `cas`. `ninep` knows nothing about images, and
`web` carries `ninep` over WebSocket with the client's access in the context.

## Roadmap

1. Directory store, layer conversion and merge, tag resolution, and a 9P2000
   server with server-side symlinks. **Built.**
2. `/.h9index` and `/.h9image`, per hyper9's
   [mount-index](https://github.com/progrium/hyper9/blob/main/design/mount-index.md).
   **Built.**
3. The S3 backend with packs and the chunk cache tiers. **Built.**
4. WebSocket transport, the metadata files over HTTP, client tokens, and the
   pull claim for several instances. **Built.**
5. Garbage collection. **Built.**
6. 9P2000.L, with real symlinks. **Built.**

## Test

```sh
go test ./...
```

The tests run against an in-process registry, so they need no network.
