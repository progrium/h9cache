// Package s3 is a store.Store backed by an S3-compatible bucket: AWS S3,
// R2, GCS's XML API, or MinIO. Keys are object names under an optional
// prefix. Claims use conditional writes (If-None-Match), so any number of
// server instances can share one bucket.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/progrium/h9reg/store"
)

// Store keeps objects in Bucket under Prefix.
type Store struct {
	Client *awss3.Client
	Bucket string
	Prefix string // empty, or ending in "/"
}

// Open parses a store URL and connects:
//
//	s3://BUCKET[/PREFIX][?endpoint=URL&region=REGION&path-style=true]
//
// Credentials and, without the query parameters, the region and endpoint
// come from the AWS SDK's usual sources: the environment, shared config
// files, and instance roles.
func Open(ctx context.Context, rawURL string) (*Store, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("s3 store: want s3://BUCKET[/PREFIX], got %q", rawURL)
	}
	q := u.Query()
	var opts []func(*config.LoadOptions) error
	if r := q.Get("region"); r != "" {
		opts = append(opts, config.WithRegion(r))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	pathStyle, _ := strconv.ParseBool(q.Get("path-style"))
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if ep := q.Get("endpoint"); ep != "" {
			o.BaseEndpoint = aws.String(ep)
		}
		o.UsePathStyle = pathStyle
		// Ranged reads of packs can't be checked against a whole-object
		// checksum, and S3-compatible servers vary in what they support.
		// Chunks are verified against their own hashes instead.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return New(client, u.Host, strings.TrimPrefix(u.Path, "/")), nil
}

// New returns a store over an existing client.
func New(client *awss3.Client, bucket, prefix string) *Store {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Store{Client: client, Bucket: bucket, Prefix: prefix}
}

func (s *Store) key(k string) *string { return aws.String(s.Prefix + k) }

func httpStatus(err error) int {
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func errCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

func isNotFound(err error) bool {
	if httpStatus(err) == http.StatusNotFound {
		return true
	}
	switch errCode(err) {
	case "NoSuchKey", "NotFound":
		return true
	}
	return false
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.Client.GetObject(ctx, &awss3.GetObjectInput{Bucket: &s.Bucket, Key: s.key(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *Store) GetRange(ctx context.Context, key string, off, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}
	rng := fmt.Sprintf("bytes=%d-%d", off, off+length-1)
	out, err := s.Client.GetObject(ctx, &awss3.GetObjectInput{Bucket: &s.Bucket, Key: s.key(key), Range: &rng})
	if err != nil {
		if isNotFound(err) {
			return nil, store.ErrNotFound
		}
		// A range that starts at or past the end.
		if httpStatus(err) == http.StatusRequestedRangeNotSatisfiable || errCode(err) == "InvalidRange" {
			return nil, nil
		}
		return nil, err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if int64(len(b)) > length { // a server that ignored the range
		if off >= int64(len(b)) {
			return nil, err
		}
		b = b[off:min(int64(len(b)), off+length)]
	}
	return b, err
}

func (s *Store) Has(ctx context.Context, key string) (bool, error) {
	_, err := s.Client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: &s.Bucket, Key: s.key(key)})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.Client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: &s.Bucket, Key: s.key(key),
		Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))),
	})
	return err
}

func (s *Store) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	_, err := s.Client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: &s.Bucket, Key: s.key(key),
		Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))),
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		switch {
		case httpStatus(err) == http.StatusPreconditionFailed, errCode(err) == "PreconditionFailed":
			return false, nil
		// S3 answers 409 to a write that races another conditional write
		// to the same key; that other write is the winner.
		case httpStatus(err) == http.StatusConflict, errCode(err) == "ConditionalRequestConflict":
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.Client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: &s.Bucket, Key: s.key(key)})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := awss3.NewListObjectsV2Paginator(s.Client, &awss3.ListObjectsV2Input{Bucket: &s.Bucket, Prefix: s.key(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, strings.TrimPrefix(aws.ToString(o.Key), s.Prefix))
		}
	}
	sort.Strings(keys)
	return keys, nil
}

var _ store.Store = (*Store)(nil)
