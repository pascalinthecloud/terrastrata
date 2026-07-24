package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/pascalinthecloud/terrastrata/internal/config"
)

func testS3Config(prefix string) config.S3Config {
	return config.S3Config{
		Bucket: "b", Prefix: prefix, Region: "us-east-1",
		AccessKey: "a", SecretKey: "s",
	}
}

// fakeS3 implements s3API in memory, optionally failing with a given API error
// code so the not-found classification can be pinned.
type fakeS3 struct {
	objects map[string][]byte
	getErr  error
	putErr  error
	lastKey string
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.lastKey = *in.Key
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.objects[*in.Key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "absent"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.lastKey = *in.Key
	if f.putErr != nil {
		return nil, f.putErr
	}
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[*in.Key] = b
	return &s3.PutObjectOutput{}, nil
}

func newTestS3(fake *fakeS3, prefix string) *S3 {
	return &S3{client: fake, bucket: "test-bucket", prefix: strings.Trim(prefix, "/")}
}

func TestS3PutThenGetRoundtrip(t *testing.T) {
	fake := newFakeS3()
	s := newTestS3(fake, "tf-mirror")
	ctx := context.Background()

	if err := s.Put(ctx, "host/ns/type/index.json", bytes.NewReader([]byte("body"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if fake.lastKey != "tf-mirror/host/ns/type/index.json" {
		t.Errorf("object key = %q, want prefix applied", fake.lastKey)
	}

	rc, hit, err := s.Get(ctx, "host/ns/type/index.json")
	if err != nil || !hit {
		t.Fatalf("Get hit=%v err=%v", hit, err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "body" {
		t.Errorf("Get body = %q", got)
	}
}

func TestS3ObjectKeyWithoutPrefix(t *testing.T) {
	fake := newFakeS3()
	s := newTestS3(fake, "")
	_ = s.Put(context.Background(), "a/b", bytes.NewReader(nil))
	if fake.lastKey != "a/b" {
		t.Errorf("object key = %q, want bare key with empty prefix", fake.lastKey)
	}
}

func TestS3GetMissOnNotFoundCodes(t *testing.T) {
	for _, code := range []string{"NoSuchKey", "NotFound"} {
		fake := newFakeS3()
		fake.getErr = &smithy.GenericAPIError{Code: code, Message: "absent"}
		s := newTestS3(fake, "p")

		rc, hit, err := s.Get(context.Background(), "k")
		if err != nil || hit || rc != nil {
			t.Errorf("code %s: Get = (%v, %v, %v), want clean miss", code, rc, hit, err)
		}
	}
}

// A missing bucket is an operator fault and must propagate as an error, never
// be swallowed as a cache miss (which would silently disable the durable layer).
func TestS3GetNoSuchBucketPropagates(t *testing.T) {
	fake := newFakeS3()
	fake.getErr = &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "bucket gone"}
	s := newTestS3(fake, "p")

	_, hit, err := s.Get(context.Background(), "k")
	if err == nil || hit {
		t.Fatalf("Get = (hit=%v, err=%v), want propagated error", hit, err)
	}
	if !strings.Contains(err.Error(), "k") {
		t.Errorf("error %q should name the key", err)
	}
}

func TestS3PutWrapsErrorWithKey(t *testing.T) {
	fake := newFakeS3()
	wantErr := errors.New("upload failed")
	fake.putErr = wantErr
	s := newTestS3(fake, "p")

	err := s.Put(context.Background(), "some/key", bytes.NewReader(nil))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Put err = %v, want wrapped %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "some/key") {
		t.Errorf("error %q should name the key", err)
	}
}

func TestNewS3TrimsPrefixSlashes(t *testing.T) {
	s := NewS3(testS3Config("/deep/prefix/"))
	if s.prefix != "deep/prefix" {
		t.Errorf("prefix = %q, want slashes trimmed", s.prefix)
	}
}
