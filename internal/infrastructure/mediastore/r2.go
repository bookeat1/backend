// Package mediastore adapts Cloudflare R2 (S3 API) to media.Store.
//
// It is deliberately the ONLY place in the service that holds an S3 client,
// and it exposes exactly four operations: list, head, get, put. There is no
// DeleteObject and no CopyObject wrapper here, and none should be added
// without a very specific reason: the originals in this bucket are the only
// copies in existence, and the cheapest way to keep them is to never build the
// tool that could remove them.
package mediastore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"backend-core/internal/media"
)

// CacheControl is set on every derivative written.
//
// The same value R2 already returns for the originals: a derivative's key
// contains the original's key, which contains an upload timestamp and a random
// suffix, so a given derivative key names one immutable set of bytes forever.
// A year of immutable caching is therefore not a bet, it is a fact about how
// the key is constructed.
const CacheControl = "public, max-age=31536000, immutable"

// Config is the connection to one bucket. Every field comes from the
// environment; nothing here is ever logged.
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
}

// ConfigFromEnv reads the R2 credentials.
//
//	R2_ENDPOINT           https://<account>.r2.cloudflarestorage.com
//	R2_ACCESS_KEY_ID
//	R2_SECRET_ACCESS_KEY
//	R2_BUCKET             default "book-eat-media"
func ConfigFromEnv() (Config, error) {
	c := Config{
		Endpoint:  strings.TrimSpace(os.Getenv("R2_ENDPOINT")),
		AccessKey: strings.TrimSpace(os.Getenv("R2_ACCESS_KEY_ID")),
		SecretKey: strings.TrimSpace(os.Getenv("R2_SECRET_ACCESS_KEY")),
		Bucket:    strings.TrimSpace(os.Getenv("R2_BUCKET")),
	}
	if c.Bucket == "" {
		c.Bucket = "book-eat-media"
	}
	var missing []string
	if c.Endpoint == "" {
		missing = append(missing, "R2_ENDPOINT")
	}
	if c.AccessKey == "" {
		missing = append(missing, "R2_ACCESS_KEY_ID")
	}
	if c.SecretKey == "" {
		missing = append(missing, "R2_SECRET_ACCESS_KEY")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("mediastore: missing %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// Store is an R2 bucket behind the media.Store port.
type Store struct {
	client *s3.Client
	bucket string
	// readOnly refuses every Put. Set it when a tool has no business writing;
	// it is a second lock on top of the caller's dry-run flag.
	readOnly bool
}

var _ media.Store = (*Store)(nil)

// New builds the client. R2 ignores the region but the SDK insists on one, so
// "auto" is passed, which is what Cloudflare documents.
func New(ctx context.Context, cfg Config) (*Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		),
	)
	if err != nil {
		// The SDK's error can quote configuration; it never quotes the secret,
		// but wrap it in our own words rather than pass it through verbatim.
		return nil, errors.New("mediastore: could not build the AWS config")
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		// R2 serves path-style addressing; virtual-host style would resolve a
		// bucket name into a hostname that does not exist.
		o.UsePathStyle = true
	})
	return &Store{client: client, bucket: cfg.Bucket}, nil
}

// ReadOnly returns a view of the store that refuses every write.
func (s *Store) ReadOnly() *Store {
	cp := *s
	cp.readOnly = true
	return &cp
}

// ErrReadOnly is returned by Put on a read-only store.
var ErrReadOnly = errors.New("mediastore: store is read-only")

func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var (
		keys  []string
		token *string
	)
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", prefix, err)
		}
		for _, o := range out.Contents {
			if o.Key == nil {
				continue
			}
			keys = append(keys, *o.Key)
		}
		if out.NextContinuationToken == nil {
			return keys, nil
		}
		token = out.NextContinuationToken
	}
}

func (s *Store) Head(ctx context.Context, key string) (int64, bool, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// "Not there" is the normal answer for a derivative that has not been
		// generated yet — it is the signal the whole backfill runs on, not an
		// error.
		var nf *types.NotFound
		var nsk *types.NoSuchKey
		if errors.As(err, &nf) || errors.As(err, &nsk) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("head %q: %w", key, err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return size, true, nil
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", key, err)
	}
	return b, nil
}

// Put writes a derivative.
//
// It refuses any key outside the derived/ prefix. That check is the last line
// of a chain — media.DerivedKey cannot produce such a key, and the backfill
// checks before calling — and it lives HERE, at the only point in the program
// that can actually modify the bucket, because that is the one place a future
// caller cannot forget to go through.
func (s *Store) Put(ctx context.Context, key string, body []byte, contentType string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if !media.IsDerived(key) {
		return fmt.Errorf("%w: %q", media.ErrNotDerived, key)
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Body:         strings.NewReader(string(body)),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String(CacheControl),
	})
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	return nil
}

// Bucket is the bucket name, for a tool that wants to print what it is about
// to act on.
func (s *Store) Bucket() string { return s.bucket }
