package s3

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func TestHealthRel(t *testing.T) {
	ts := time.Unix(0, 1234).UTC()
	if got := HealthRel("dp-india-01", ts); got != "_health/dp-india-01/1234.txt" {
		t.Errorf("HealthRel = %q", got)
	}
	if got := HealthRel("dp/india 01", ts); got != "_health/dp-india-01/1234.txt" {
		t.Errorf("HealthRel sanitize = %q", got)
	}
}

func TestArchiveRel(t *testing.T) {
	day := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	want := "isp_id=7/year=2026/month=06/day=25/part-000.csv.gz"
	if got := ArchiveRel(7, day, "csv.gz"); got != want {
		t.Errorf("ArchiveRel = %q, want %q", got, want)
	}
	if got := ArchiveRel(7, day, ".parquet"); got != "isp_id=7/year=2026/month=06/day=25/part-000.parquet" {
		t.Errorf("ArchiveRel parquet = %q", got)
	}
}

func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		in     string
		host   string
		secure bool
		err    bool
	}{
		{"http://127.0.0.1:9000", "127.0.0.1:9000", false, false},
		{"https://s3.wasabisys.com", "s3.wasabisys.com", true, false},
		{"s3.amazonaws.com", "s3.amazonaws.com", true, false},
		{"", "", false, true},
	}
	for _, c := range cases {
		host, secure, err := splitEndpoint(c.in)
		if (err != nil) != c.err {
			t.Errorf("splitEndpoint(%q) err=%v want err=%v", c.in, err, c.err)
			continue
		}
		if err == nil && (host != c.host || secure != c.secure) {
			t.Errorf("splitEndpoint(%q) = %q,%v want %q,%v", c.in, host, secure, c.host, c.secure)
		}
	}
}

type fakePutter struct {
	bucket, object string
	size           int64
	body           []byte
}

func (f *fakePutter) PutObject(_ context.Context, bucket, object string, r io.Reader, size int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	f.bucket, f.object, f.size = bucket, object, size
	f.body, _ = io.ReadAll(r)
	return minio.UploadInfo{}, nil
}

func (f *fakePutter) BucketExists(context.Context, string) (bool, error) { return true, nil }
func (f *fakePutter) MakeBucket(context.Context, string, minio.MakeBucketOptions) error {
	return nil
}
func (f *fakePutter) StatObject(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error) {
	return minio.ObjectInfo{Size: f.size}, nil
}

func TestUploadKeyConstruction(t *testing.T) {
	fp := &fakePutter{}
	c := &Client{mc: fp, bucket: "flows", prefix: "natflow"}
	key, err := c.Upload(context.Background(), "/a/b.txt", bytes.NewReader([]byte("hi")), 2, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if key != "natflow/a/b.txt" {
		t.Errorf("key = %q, want natflow/a/b.txt", key)
	}
	if fp.bucket != "flows" || fp.object != "natflow/a/b.txt" || fp.size != 2 {
		t.Errorf("putter got bucket=%q object=%q size=%d", fp.bucket, fp.object, fp.size)
	}
	if string(fp.body) != "hi" {
		t.Errorf("body = %q", fp.body)
	}
}

func TestHealthMarkerUsesFakePutter(t *testing.T) {
	fp := &fakePutter{}
	c := &Client{mc: fp, bucket: "b", prefix: ""}
	key, err := c.HealthMarker(context.Background(), "dp1", time.Unix(0, 9).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if key != "_health/dp1/9.txt" {
		t.Errorf("health key = %q", key)
	}
	if len(fp.body) == 0 {
		t.Error("marker body empty")
	}
}
