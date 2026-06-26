// Package s3 wraps an S3-compatible object store (AWS S3, Wasabi, MinIO, ...)
// for archiving flow data and health markers.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config configures the S3 target.
type Config struct {
	Endpoint   string // host[:port] or http(s)://host[:port]
	Region     string
	Bucket     string
	AccessKey  string
	SecretKey  string
	PathPrefix string
}

// putter is the subset of the minio client used here (swappable in tests).
type putter interface {
	PutObject(ctx context.Context, bucket, object string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	BucketExists(ctx context.Context, bucket string) (bool, error)
	MakeBucket(ctx context.Context, bucket string, opts minio.MakeBucketOptions) error
	StatObject(ctx context.Context, bucket, object string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
}

// Client uploads objects to an S3-compatible bucket.
type Client struct {
	mc        putter
	bucket    string
	prefix    string
	region    string
	endpoint  string // original endpoint (with scheme) for building ClickHouse s3() URLs
	accessKey string
	secretKey string
}

// Bucket returns the configured bucket name.
func (c *Client) Bucket() string { return c.bucket }

// AccessKey / SecretKey expose credentials for ClickHouse-native s3() transfers
// (server-side INSERT INTO FUNCTION s3 / SELECT FROM s3). Same module only.
func (c *Client) AccessKey() string { return c.accessKey }
func (c *Client) SecretKey() string { return c.secretKey }

// ObjectURL returns the full https URL of a relative key, for ClickHouse s3().
func (c *Client) ObjectURL(rel string) string {
	ep := c.endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	return strings.TrimRight(ep, "/") + "/" + c.bucket + "/" + c.Key(rel)
}

// Stat returns the object size in bytes (0 if missing).
func (c *Client) Stat(ctx context.Context, rel string) (int64, error) {
	info, err := c.mc.StatObject(ctx, c.bucket, c.Key(rel), minio.StatObjectOptions{})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

// EnsureBucket creates the bucket if it does not already exist. Best-effort and
// idempotent — convenient for self-hosted S3/MinIO; a no-op when the bucket is
// already present (e.g. pre-created AWS buckets).
func (c *Client) EnsureBucket(ctx context.Context) error {
	ok, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return c.mc.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{Region: c.region})
}

// New builds a Client. The endpoint may include an http(s):// scheme; otherwise
// HTTPS is assumed (the AWS/Wasabi default).
func New(cfg Config) (*Client, error) {
	host, secure, err := splitEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	mc, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return &Client{mc: mc, bucket: cfg.Bucket, prefix: strings.Trim(cfg.PathPrefix, "/"), region: cfg.Region,
		endpoint: cfg.Endpoint, accessKey: cfg.AccessKey, secretKey: cfg.SecretKey}, nil
}

// Key joins the configured path prefix with a relative object key.
func (c *Client) Key(rel string) string {
	rel = strings.TrimLeft(rel, "/")
	if c.prefix == "" {
		return rel
	}
	return c.prefix + "/" + rel
}

// Upload writes size bytes from r to the given relative key.
func (c *Client) Upload(ctx context.Context, rel string, r io.Reader, size int64, contentType string) (string, error) {
	key := c.Key(rel)
	_, err := c.mc.PutObject(ctx, c.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return key, fmt.Errorf("put %s/%s: %w", c.bucket, key, err)
	}
	return key, nil
}

// HealthMarker uploads a tiny marker file proving connectivity, returning the
// object key written.
func (c *Client) HealthMarker(ctx context.Context, dpName string, ts time.Time) (string, error) {
	rel := HealthRel(dpName, ts)
	body := []byte(fmt.Sprintf("natflow-dataplane health\nserver=%s\ntime=%s\n", dpName, ts.UTC().Format(time.RFC3339)))
	return c.Upload(ctx, rel, bytes.NewReader(body), int64(len(body)), "text/plain")
}

// HealthRel builds the relative health-marker key: _health/<dp>/<unixnano>.txt
func HealthRel(dpName string, ts time.Time) string {
	name := sanitize(dpName)
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf("_health/%s/%d.txt", name, ts.UTC().UnixNano())
}

// ArchiveRel builds the partitioned archive key for one day's export:
//
//	isp_id=<id>/year=YYYY/month=MM/day=DD/part-000.<ext>
func ArchiveRel(ispID uint32, day time.Time, ext string) string {
	d := day.UTC()
	return fmt.Sprintf("isp_id=%d/year=%04d/month=%02d/day=%02d/part-000.%s",
		ispID, d.Year(), int(d.Month()), d.Day(), strings.TrimPrefix(ext, "."))
}

// splitEndpoint normalizes an endpoint into (host, secure).
func splitEndpoint(endpoint string) (host string, secure bool, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, fmt.Errorf("empty s3 endpoint")
	}
	if strings.Contains(endpoint, "://") {
		u, perr := url.Parse(endpoint)
		if perr != nil {
			return "", false, fmt.Errorf("parse endpoint: %w", perr)
		}
		return u.Host, u.Scheme == "https", nil
	}
	return endpoint, true, nil // bare host -> HTTPS by default
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
}
