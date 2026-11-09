# h9reg

A container registry cache that serves images as 9P trees. A client attaches
with an image reference as the attach spec and gets that image's merged
filesystem as its root. The first attach of an image pulls and converts it,
and every later attach is served from the store.

```sh
h9reg serve --store /var/lib/h9reg --listen 'tcp!*!564'
h9 --root 'tcp!localhost!564' --root-spec alpine:3.20 --ns linux.ns sh
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
- **Read-only 9P2000.** Walks and stats read tree objects from a memory cache,
  and reads fetch only the chunks they need. Writes get "read-only file
  system".

### Symlinks

9P2000 has no symlinks, so the server follows them during walks, inside the
image: an absolute target starts at the image root, `..` stops there, and a
walk follows at most 40 links. Exec, open, and stat work through links. A
directory listing shows a link as its target under the link's name, and
leaves out dangling links. What's lost is `readlink`, so `ls -l` shows no
arrows. 9P2000.L, where links are real, comes later.

### Other details

- **Qids** are derived, not stored: the first 8 bytes of BLAKE3 over the image
  tree id and the file's canonical path. Every connection and every server
  instance gives a file the same qid. Hardlinked names share a qid, and so do a
  symlink and its target.
- **Owners** are shown as names from the image's own `/etc/passwd` and
  `/etc/group`, or as the number when there's no entry.
- **Device nodes and FIFOs** are listed but fail to open.
- **Mount points** that h9 binds onto (`/dev`, `/env`, `/fd`,
  `/mnt/term/cwd`, `/net`, `/proc`, `/srv`, and `/tmp`) are added to the image
  as empty directories when the image lacks them, since a 9P root is
  read-only and h9 needs `/mnt/term/cwd` for the working directory.
- **Not kept yet:** setuid, setgid, and sticky bits are stored but not served
  (9P2000 has no bits for them), and extended attributes, file capabilities
  included, are dropped.

## Running Linux images in h9

h9 treats a 9P root as a 9front root today: only a host directory is
detected as a Linux root. Until it detects Linux trees over 9P too, put the
image in the Linux view with a namespace file:

```sh
printf "bind -a / '#λ'\n" > linux.ns
h9 --root 'tcp!localhost!564' --root-spec alpine:3.20 --ns linux.ns /bin/sh -c 'cat /etc/os-release'
```

`--ns` is a guest path, so a relative name is found in the working directory.

## Commands

```
h9reg serve [flags]          serve images over 9P
h9reg pull [flags] IMAGE...  pull images into the store ahead of time
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--store` | `$H9REG_STORE`, else `h9reg-data` | The object store: a directory, or `dir:PATH` |
| `--listen` | `tcp!*!564` | `tcp!HOST!PORT` or `unix!PATH`; repeatable |
| `--platform` | `linux/` plus the build's architecture | Platform picked from multi-platform images |
| `--tag-ttl` | `5m` | How long a tag's resolution is trusted |
| `--cache-mb` | `256` | Memory for cached chunks |
| `-v` | off | Debug logging |

Upstream credentials come from the Docker config (`~/.docker/config.json` and
its credential helpers). Clients never hold them.

## Store layout

Everything is an object under a key, so a directory and an object store share
one layout.

| Prefix | Key | Holds |
| --- | --- | --- |
| `chunks/` | BLAKE3 of the chunk | One chunk, zstd-compressed when that's smaller |
| `trees/` | BLAKE3 of the object | One directory's sorted entries |
| `layers/` | layer blob digest | The layer's tree id and root attributes |
| `images/` | platform manifest digest | Image tree id, root attributes, platform, and config |
| `platforms/` | index digest and platform | The manifest digest picked from a multi-platform index |
| `refs/` | registry/repo/tag | Manifest or index digest, and when it was resolved |

Only `refs/` is ever overwritten. Chunks are one object each for now; packs,
which group chunks so S3 serves them with ranged GETs, come with the S3
backend.

### ZeroFS

The directory store works over any POSIX mount, a ZeroFS mount included, with
no extra code. That's reasonable for a single cache next to a ZeroFS home
server. With more than one instance, use S3 directly: ZeroFS has one writer,
its read replicas lag the writer, and its compression and GC duplicate
h9reg's own.

## Layout

```
cmd/h9reg/   main: flags, store, listeners
store/       object store interface
  dir/       directory backend
cas/         chunking and chunk storage over fvs core
tree/        tree objects, layer merge, symlink resolution, qids
pull/        reference parsing, tag resolution, layer conversion
imagefs/     an image as a 9P file tree
ninep/       9P2000 server (and a small client for tests)
```

Dependencies run one way: `imagefs` → `tree` → `cas` → `store`, with `pull`
writing through `tree` and `cas`. `ninep` knows nothing about images.

## Roadmap

1. Directory store, layer conversion and merge, tag resolution, and a 9P2000
   server with server-side symlinks. **This is what's built.**
2. `/.h9index` and `/.h9image` at commit, per hyper9's
   [mount-index](https://github.com/progrium/hyper9/blob/main/design/mount-index.md).
3. The S3 backend with packs and the chunk cache tiers.
4. WebSocket transport, client tokens, and the pull claim for several
   instances.
5. Garbage collection.
6. 9P2000.L, with real symlinks, once h9's dotl translator exists.

## Test

```sh
go test ./...
```

The tests run against an in-process registry, so they need no network.
