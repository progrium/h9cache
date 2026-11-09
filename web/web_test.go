package web

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/progrium/h9reg/access"
	"github.com/progrium/h9reg/cas"
	"github.com/progrium/h9reg/imagefs"
	"github.com/progrium/h9reg/ninep"
	"github.com/progrium/h9reg/pull"
	"github.com/progrium/h9reg/store/dir"
	"github.com/progrium/h9reg/tree"
)

const (
	tokenAll   = "aaaaaaaaaaaaaaaa"
	tokenOther = "bbbbbbbbbbbbbbbb"
)

// setup pushes a one-file image to a test registry and serves h9reg over
// HTTP in front of it. It returns the server URL and the image reference.
func setup(t *testing.T, tokens bool) (string, string) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := "hello from the image\n"
	tw.WriteHeader(&tar.Header{Name: "hello", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(1700000000, 0)})
	tw.Write([]byte(body))
	tw.Close()
	b := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, _ := mutate.AppendLayers(empty.Image, l)
	img, _ = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: "amd64", Config: v1.Config{Cmd: []string{"/hello"}}})
	reg := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(reg.Close)
	ref := strings.TrimPrefix(reg.URL, "http://") + "/test/img:1"
	tag, _ := name.NewTag(ref)
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}

	st, _ := dir.New(t.TempDir())
	chunks := cas.New(st, 1<<20)
	fs := &imagefs.FS{
		Puller: &pull.Puller{Store: st, CAS: chunks, Platform: pull.DefaultPlatform(), TagTTL: time.Hour},
		CAS:    chunks,
		Trees:  tree.NewCache(st, 100),
	}
	h := &Handler{FS: fs, Server: &ninep.Server{FS: fs}}
	if tokens {
		h.Tokens, err = access.Parse(strings.NewReader(tokenAll + " *\n" + tokenOther + " */other/*\n"))
		if err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL, ref
}

// dial attaches over WebSocket, with the URL path naming the image.
func dial(t *testing.T, url string, hdr http.Header) (*ninep.Client, error) {
	ctx := context.Background()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{
		Subprotocols: []string{"9p"},
		HTTPHeader:   hdr,
	})
	if err != nil {
		if resp != nil {
			return nil, &statusError{resp.StatusCode}
		}
		return nil, err
	}
	if c.Subprotocol() != "9p" {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	cl, err := ninep.NewClient(nc, 8192+ninep.IOHdrSz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl, nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }

func attachAndRead(t *testing.T, c *ninep.Client) string {
	t.Helper()
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Tattach, Fid: 0, Afid: ninep.NoFid, Uname: "none"}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Twalk, Fid: 0, Newfid: 1, Wname: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Topen, Fid: 1, Mode: ninep.OREAD}); err != nil {
		t.Fatal(err)
	}
	b, err := c.ReadAll(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWebSocket(t *testing.T) {
	url, ref := setup(t, false)
	c, err := dial(t, url+"/"+ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := attachAndRead(t, c); got != "hello from the image\n" {
		t.Fatalf("read %q", got)
	}
	// An attach spec, when given, wins over the URL path.
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Tattach, Fid: 9, Afid: ninep.NoFid, Uname: "none", Aname: ref}); err != nil {
		t.Fatal(err)
	}
}

func TestTokens(t *testing.T) {
	url, ref := setup(t, true)
	if _, err := dial(t, url+"/"+ref, nil); err == nil || err.(*statusError).code != http.StatusUnauthorized {
		t.Fatalf("no token: %v", err)
	}
	if _, err := dial(t, url+"/"+ref, http.Header{"Authorization": {"Bearer nope-nope-nope-nope"}}); err == nil {
		t.Fatal("bad token accepted")
	}
	c, err := dial(t, url+"/"+ref, http.Header{"Authorization": {"Bearer " + tokenAll}})
	if err != nil {
		t.Fatal(err)
	}
	attachAndRead(t, c)

	// A query token works for clients that can't send headers, and a
	// token limited to other repositories can't attach this one.
	c, err = dial(t, url+"/"+ref+"?token="+tokenOther, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RPC(&ninep.Fcall{Type: ninep.Tattach, Fid: 0, Afid: ninep.NoFid, Uname: "none"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("attach outside the token's repositories: %v", err)
	}

	resp, err := http.Get(url + "/" + ref + "/.h9index?token=" + tokenOther)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("index outside the token's repositories: %s", resp.Status)
	}
	resp, err = http.Get(url + "/" + ref + "/.h9index")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("index without a token: %s", resp.Status)
	}
}

func TestMetaFiles(t *testing.T) {
	url, ref := setup(t, false)
	get := func(path, etag string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", url+"/"+path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get(ref+"/.h9index", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "h9index 1\n") || !strings.Contains(body, "\n/hello ") {
		t.Fatalf("index: %s\n%s", resp.Status, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("tag Cache-Control %q", cc)
	}
	etag := resp.Header.Get("ETag")
	if resp, _ := get(ref+"/.h9index", etag); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation: %s", resp.Status)
	}

	// The ETag is the manifest digest, and a digest reference is immutable.
	digest := strings.Trim(etag, `"`)
	byDigest := strings.TrimSuffix(ref, ":1") + "@" + digest
	resp, body = get(byDigest+"/.h9image", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "h9image 1\n") || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("image by digest: %s %q\n%s", resp.Status, resp.Header.Get("Cache-Control"), body)
	}

	if resp, _ := get("healthz", ""); resp.StatusCode != 200 {
		t.Fatalf("healthz: %s", resp.Status)
	}
	if resp, _ := get("nothing/here", ""); resp.StatusCode != 404 {
		t.Fatalf("unknown path: %s", resp.Status)
	}
}
