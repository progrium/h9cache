package s3

import (
	"context"
	"os"
	"testing"

	"github.com/progrium/h9reg/store/s3/s3fake"
	"github.com/progrium/h9reg/store/storetest"
)

func TestFake(t *testing.T) {
	st, err := Open(context.Background(), s3fake.URL(t, "some/prefix"))
	if err != nil {
		t.Fatal(err)
	}
	storetest.Run(t, st)
}

// TestReal runs the conformance tests against a real bucket, such as a
// MinIO server, when H9REG_TEST_S3 names one (an s3:// store URL whose
// prefix is safe to fill).
func TestReal(t *testing.T) {
	u := os.Getenv("H9REG_TEST_S3")
	if u == "" {
		t.Skip("H9REG_TEST_S3 is not set")
	}
	st, err := Open(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Run(t, st)
}
