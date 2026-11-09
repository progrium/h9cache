// Package web serves images over HTTP: 9P over WebSocket, where the URL
// path is the attach spec, and each image's /.h9index and /.h9image as
// plain files, for h9's --root-index URL and for prefetching.
//
//	wss://reg.example/docker.io/library/alpine:3.20     9P, attaching that image
//	https://reg.example/alpine:3.20/.h9index            the image's index
//
// With tokens configured, every request needs one, as
// "Authorization: Bearer TOKEN" (h9's --root-header) or a token query
// parameter (for namespace-file mounts, which can't send headers).
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/progrium/h9reg/access"
	"github.com/progrium/h9reg/imagefs"
	"github.com/progrium/h9reg/ninep"
)

// MaxMessage bounds one WebSocket message, as h9 bounds its own.
const MaxMessage = 16 << 20

// Handler serves 9P over WebSocket and the image metadata files.
type Handler struct {
	FS     *imagefs.FS
	Server *ninep.Server
	Tokens *access.Tokens // nil: no tokens needed
	Log    *slog.Logger
}

func (h *Handler) log() *slog.Logger {
	if h.Log == nil {
		return slog.Default()
	}
	return h.Log
}

func bearer(r *http.Request) string {
	if a := r.Header.Get("Authorization"); a != "" {
		if t, ok := strings.CutPrefix(a, "Bearer "); ok {
			return strings.TrimSpace(t)
		}
		return ""
	}
	return r.URL.Query().Get("token")
}

// access returns the client's access, or false after answering 401.
func (h *Handler) access(w http.ResponseWriter, r *http.Request, spec string) (imagefs.Access, bool) {
	a := imagefs.Access{Spec: spec}
	if h.Tokens == nil {
		return a, true
	}
	allow, ok := h.Tokens.Allow(bearer(r))
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="h9reg"`)
		http.Error(w, "a valid bearer token is required", http.StatusUnauthorized)
		return a, false
	}
	a.Allowed = allow
	return a, true
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "healthz" {
		fmt.Fprintln(w, "ok")
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		h.websocket(w, r, p)
		return
	}
	for _, name := range []string{imagefs.IndexName, imagefs.ImageName} {
		if spec, ok := strings.CutSuffix(p, "/"+name); ok {
			h.meta(w, r, spec, name)
			return
		}
	}
	if p == "" {
		fmt.Fprint(w, "h9reg: attach an image over 9P at ws(s)://HOST/IMAGE, or fetch /IMAGE/.h9index and /IMAGE/.h9image\n")
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) websocket(w http.ResponseWriter, r *http.Request, spec string) {
	a, ok := h.access(w, r, spec)
	if !ok {
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"9p"},
		// Browser h9s connect from other origins; tokens, not origins,
		// decide what a client may attach.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		h.log().Debug("websocket accept", "err", err)
		return
	}
	c.SetReadLimit(MaxMessage)
	ctx := r.Context()
	// Each 9P reply is one Write, so one binary message, and a message the
	// client sends may hold any part of the stream.
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	h.log().Debug("websocket connected", "spec", spec, "remote", r.RemoteAddr)
	h.Server.ServeConnContext(imagefs.WithAccess(ctx, a), nc)
}

func (h *Handler) meta(w http.ResponseWriter, r *http.Request, spec, name string) {
	a, ok := h.access(w, r, spec)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(imagefs.WithAccess(r.Context(), a), 30*time.Minute)
	defer cancel()
	im, err := h.FS.Open(ctx, spec)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, imagefs.ErrDenied) {
			code = http.StatusForbidden
		}
		http.Error(w, err.Error(), code)
		return
	}
	data, _ := im.MetaFile(name)
	etag := `"` + im.Manifest() + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if strings.Contains(spec, "@") {
		// A digest names one image forever.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(data)
}
