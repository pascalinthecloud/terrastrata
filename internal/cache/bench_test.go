package cache

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"testing"
)

// BenchmarkLocalPut measures the atomic+durable write path (temp file, fsync,
// rename, dir fsync) — the cost paid once per cache miss.
func BenchmarkLocalPut(b *testing.B) {
	c, err := NewLocal(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	data := make([]byte, 64*1024)
	ctx := context.Background()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Put(ctx, "registry.terraform.io/ns/type/3.0.0.json", bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLocalGet measures the cache-hit open path including the mtime touch.
func BenchmarkLocalGet(b *testing.B) {
	c, err := NewLocal(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	key := "registry.terraform.io/ns/type/index.json"
	if err := c.Put(ctx, key, bytes.NewReader([]byte(`{"versions":{}}`))); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rc, _, err := c.Get(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}
}

// BenchmarkEvictorScan compares the two sweep passes as a function of file
// count: totalSize (the cheap common path, run every sweep) vs collectFiles
// (only run when actually over budget).
func BenchmarkEvictorScan(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		root := b.TempDir()
		for i := 0; i < n; i++ {
			writeBenchFile(b, root, i)
		}
		e := NewEvictor(root, 0, nil, discardLogger())

		b.Run("totalSize/"+strconv.Itoa(n)+"files", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := e.totalSize(); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("collectFiles/"+strconv.Itoa(n)+"files", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := e.collectFiles(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func writeBenchFile(b *testing.B, root string, i int) {
	b.Helper()
	c, _ := NewLocal(root)
	key := "registry.terraform.io/hashicorp/p" + strconv.Itoa(i%50) + "/v" + strconv.Itoa(i) + ".json"
	if err := c.Put(context.Background(), key, bytes.NewReader([]byte("x"))); err != nil {
		b.Fatal(err)
	}
}
