// Command h9reg is a container registry cache that serves images as 9P
// trees. A client attaches with an image reference as the attach spec, and
// gets that image's merged filesystem as the root:
//
//	h9reg serve --store /var/lib/h9reg --listen tcp!*!564
//	h9 --root tcp!localhost!564 --root-spec alpine:3.20 sh
//
// It also serves 9P over WebSocket, where the URL path names the image:
//
//	h9reg serve --listen http!*!8564
//	h9 --root ws://localhost:8564/alpine:3.20 sh
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/progrium/h9reg/access"
	"github.com/progrium/h9reg/cas"
	"github.com/progrium/h9reg/imagefs"
	"github.com/progrium/h9reg/ninep"
	"github.com/progrium/h9reg/pull"
	"github.com/progrium/h9reg/store"
	"github.com/progrium/h9reg/store/dir"
	"github.com/progrium/h9reg/store/s3"
	"github.com/progrium/h9reg/tree"
	"github.com/progrium/h9reg/web"
)

const usage = `usage: h9reg serve [flags]
       h9reg pull [flags] IMAGE...

Commands:
  serve   serve images over 9P; the attach spec, or the WebSocket URL
          path, names the image
  pull    pull images into the store ahead of time

IMAGE is a reference such as alpine:3.20, ghcr.io/org/app@sha256:...,
optionally followed by !OS/ARCH[/VARIANT] to pick a platform.

Flags:
`

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

type config struct {
	store    string
	listen   listFlag
	platform string
	tagTTL   time.Duration
	cacheMB  int64
	diskDir  string
	diskMB   int64
	prefetch int
	verbose  bool
	tokens   string
	tlsCert  string
	tlsKey   string
	claimTTL time.Duration
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	var cfg config
	fs := flag.NewFlagSet("h9reg "+cmd, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}
	def := os.Getenv("H9REG_STORE")
	if def == "" {
		def = "h9reg-data"
	}
	fs.StringVar(&cfg.store, "store", def, "object store: a directory (or dir:PATH), or s3://BUCKET[/PREFIX][?endpoint=URL&region=R&path-style=true]; $H9REG_STORE")
	fs.Var(&cfg.listen, "listen", "address to serve on: tcp!HOST!PORT or unix!PATH for 9P, http!HOST!PORT or https!HOST!PORT for 9P over WebSocket and the image files; repeatable (default tcp!*!564)")
	fs.StringVar(&cfg.tokens, "tokens", "", "file of client tokens and the repositories each may attach; every HTTP request then needs one")
	fs.StringVar(&cfg.tlsCert, "tls-cert", "", "certificate for https listeners")
	fs.StringVar(&cfg.tlsKey, "tls-key", "", "key for https listeners")
	fs.DurationVar(&cfg.claimTTL, "claim-ttl", pull.DefaultClaimTTL, "how long another instance's pull claim stands without being refreshed")
	fs.StringVar(&cfg.platform, "platform", pull.DefaultPlatform().String(), "platform picked from multi-platform images")
	fs.DurationVar(&cfg.tagTTL, "tag-ttl", 5*time.Minute, "how long a tag's resolution is trusted before checking upstream")
	fs.Int64Var(&cfg.cacheMB, "cache-mb", 256, "memory for cached chunks, in MB")
	fs.StringVar(&cfg.diskDir, "disk-cache", "", "directory for a local cache of chunks, between memory and the store; none if empty")
	fs.Int64Var(&cfg.diskMB, "disk-cache-mb", 4096, "size of the disk cache, in MB")
	fs.IntVar(&cfg.prefetch, "prefetch", 4, "chunks to read ahead after each read")
	fs.BoolVar(&cfg.verbose, "v", false, "log debug detail")

	switch cmd {
	case "serve", "pull":
		fs.Parse(os.Args[2:])
	case "-h", "-help", "--help", "help":
		fs.Usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "h9reg: unknown command %q\n\n", cmd)
		fs.Usage()
		os.Exit(2)
	}

	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(cmd, cfg, fs.Args(), log); err != nil {
		fmt.Fprintf(os.Stderr, "h9reg: %v\n", err)
		os.Exit(1)
	}
}

func openStore(ctx context.Context, spec string) (store.Store, error) {
	switch {
	case strings.HasPrefix(spec, "s3://"):
		return s3.Open(ctx, spec)
	case strings.HasPrefix(spec, "dir:"):
		return dir.New(strings.TrimPrefix(spec, "dir:"))
	default:
		return dir.New(spec)
	}
}

func run(cmd string, cfg config, args []string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	st, err := openStore(ctx, cfg.store)
	if err != nil {
		return err
	}
	plat, err := v1.ParsePlatform(cfg.platform)
	if err != nil {
		return fmt.Errorf("--platform: %w", err)
	}
	chunks := cas.New(st, cfg.cacheMB<<20)
	chunks.Prefetch = cfg.prefetch
	chunks.Log = log
	if cfg.diskDir != "" {
		if chunks.Disk, err = cas.OpenDiskCache(cfg.diskDir, cfg.diskMB<<20); err != nil {
			return fmt.Errorf("--disk-cache: %w", err)
		}
	}
	start := time.Now()
	if err := chunks.LoadPacks(ctx); err != nil {
		return fmt.Errorf("loading pack indexes: %w", err)
	}
	packs, n := chunks.PackStats()
	log.Info("loaded pack indexes", "packs", packs, "chunks", n, "took", time.Since(start).Round(time.Millisecond))
	puller := &pull.Puller{
		Store:    st,
		CAS:      chunks,
		Platform: *plat,
		TagTTL:   cfg.tagTTL,
		Remote: []remote.Option{
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
			remote.WithUserAgent("h9reg"),
		},
		Log:      log,
		ClaimTTL: cfg.claimTTL,
	}
	fs := &imagefs.FS{Puller: puller, CAS: chunks, Trees: tree.NewCache(st, 64<<10)}

	switch cmd {
	case "pull":
		if len(args) == 0 {
			return errors.New("pull: name at least one image")
		}
		for _, a := range args {
			spec, err := pull.ParseSpec(a)
			if err != nil {
				return err
			}
			img, err := puller.Resolve(ctx, spec)
			if err != nil {
				return err
			}
			if err := fs.Prepare(ctx, img); err != nil {
				return err
			}
			fmt.Printf("%s\t%s\t%s\n", a, img.Manifest, img.Platform)
		}
		return nil
	}

	srv := &ninep.Server{
		FS:  fs,
		Log: log,
	}
	if len(cfg.listen) == 0 {
		cfg.listen = listFlag{"tcp!*!564"}
	}
	var tokens *access.Tokens
	if cfg.tokens != "" {
		if tokens, err = access.Load(cfg.tokens); err != nil {
			return err
		}
	}
	handler := &web.Handler{FS: fs, Server: srv, Tokens: tokens, Log: log}
	errc := make(chan error, len(cfg.listen))
	for _, addr := range cfg.listen {
		proto, _, _ := strings.Cut(addr, "!")
		switch proto {
		case "http", "https":
			l, err := listen("tcp" + strings.TrimPrefix(addr, proto))
			if err != nil {
				return err
			}
			hs := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
			defer hs.Close()
			if proto == "https" {
				if cfg.tlsCert == "" || cfg.tlsKey == "" {
					return fmt.Errorf("%s: https needs --tls-cert and --tls-key", addr)
				}
				go func() { errc <- hs.ServeTLS(l, cfg.tlsCert, cfg.tlsKey) }()
			} else {
				go func() { errc <- hs.Serve(l) }()
			}
			log.Info("serving 9P over WebSocket", "addr", addr, "store", cfg.store, "tokens", tokens != nil)
			continue
		case "unix":
		default:
			if tokens != nil {
				// Plain 9P has no way to present a token.
				return fmt.Errorf("%s: plain 9P can't check --tokens; serve it on a unix socket, or only http and https", addr)
			}
		}
		l, err := listen(addr)
		if err != nil {
			return err
		}
		defer l.Close()
		log.Info("serving 9P", "addr", addr, "store", cfg.store)
		go func() { errc <- srv.Serve(l) }()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return nil
	}
}

// listen opens a Plan 9 style address: tcp!HOST!PORT (HOST may be * for
// every interface) or unix!PATH. A bare host:port is TCP.
func listen(addr string) (net.Listener, error) {
	f := strings.Split(addr, "!")
	switch {
	case len(f) == 3 && f[0] == "tcp":
		host := f[1]
		if host == "*" {
			host = ""
		}
		return net.Listen("tcp", net.JoinHostPort(host, f[2]))
	case len(f) == 2 && f[0] == "tcp":
		host := f[1]
		if host == "*" {
			host = ""
		}
		return net.Listen("tcp", net.JoinHostPort(host, "564"))
	case len(f) == 2 && f[0] == "unix":
		os.Remove(f[1])
		return net.Listen("unix", f[1])
	case len(f) == 1 && strings.Contains(addr, ":"):
		return net.Listen("tcp", addr)
	}
	return nil, fmt.Errorf("bad listen address %q: want tcp!HOST!PORT or unix!PATH", addr)
}
