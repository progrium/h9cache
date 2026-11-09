// Command h9reg is a container registry cache that serves images as 9P
// trees. A client attaches with an image reference as the attach spec, and
// gets that image's merged filesystem as the root:
//
//	h9reg serve --store /var/lib/h9reg --listen tcp!*!564
//	h9 --root tcp!localhost!564 --root-spec alpine:3.20 sh
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/progrium/h9reg/cas"
	"github.com/progrium/h9reg/imagefs"
	"github.com/progrium/h9reg/ninep"
	"github.com/progrium/h9reg/pull"
	"github.com/progrium/h9reg/store"
	"github.com/progrium/h9reg/store/dir"
	"github.com/progrium/h9reg/tree"
)

const usage = `usage: h9reg serve [flags]
       h9reg pull [flags] IMAGE...

Commands:
  serve   serve images over 9P; the attach spec names the image
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
	verbose  bool
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
	fs.StringVar(&cfg.store, "store", def, "object store: a directory (or dir:PATH); $H9REG_STORE")
	fs.Var(&cfg.listen, "listen", "address to serve 9P on: tcp!HOST!PORT or unix!PATH; repeatable (default tcp!*!564)")
	fs.StringVar(&cfg.platform, "platform", pull.DefaultPlatform().String(), "platform picked from multi-platform images")
	fs.DurationVar(&cfg.tagTTL, "tag-ttl", 5*time.Minute, "how long a tag's resolution is trusted before checking upstream")
	fs.Int64Var(&cfg.cacheMB, "cache-mb", 256, "memory for cached chunks, in MB")
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

func openStore(spec string) (store.Store, error) {
	switch {
	case strings.HasPrefix(spec, "s3://"):
		return nil, errors.New("the S3 backend is not built yet; use a directory")
	case strings.HasPrefix(spec, "dir:"):
		return dir.New(strings.TrimPrefix(spec, "dir:"))
	default:
		return dir.New(spec)
	}
}

func run(cmd string, cfg config, args []string, log *slog.Logger) error {
	st, err := openStore(cfg.store)
	if err != nil {
		return err
	}
	plat, err := v1.ParsePlatform(cfg.platform)
	if err != nil {
		return fmt.Errorf("--platform: %w", err)
	}
	chunks := cas.New(st, cfg.cacheMB<<20)
	puller := &pull.Puller{
		Store:    st,
		CAS:      chunks,
		Platform: *plat,
		TagTTL:   cfg.tagTTL,
		Remote: []remote.Option{
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
			remote.WithUserAgent("h9reg"),
		},
		Log: log,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

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
			fmt.Printf("%s\t%s\t%s\n", a, img.Manifest, img.Platform)
		}
		return nil
	}

	srv := &ninep.Server{
		FS:  &imagefs.FS{Puller: puller, CAS: chunks, Trees: tree.NewCache(st, 64<<10)},
		Log: log,
	}
	if len(cfg.listen) == 0 {
		cfg.listen = listFlag{"tcp!*!564"}
	}
	errc := make(chan error, len(cfg.listen))
	for _, addr := range cfg.listen {
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
