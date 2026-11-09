// Package s3fake runs an in-process S3 server for tests.
package s3fake

import (
	"net/http/httptest"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// URL starts an in-process S3 server with one bucket and returns a store
// URL for it under prefix. The server stops when the test ends.
func URL(t testing.TB, prefix string) string {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	backend := s3mem.New()
	if err := backend.CreateBucket("h9reg"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	return "s3://h9reg/" + prefix + "?path-style=true&region=us-east-1&endpoint=" + srv.URL
}
