// Command subctl-templates-build packages a template from a directory or
// OCI-style tarball: chunks the rootfs into the content-addressed store,
// records a hotness index, and (with --ext4) produces a bootable image for
// the hv tier. This is the OSS-side of the build farm (plan §3.5).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/substrate-sh/substrate/internal/snapstore"
)

func main() {
	dir := flag.String("dir", "", "rootfs directory to package")
	out := flag.String("out", "./template-out", "output directory")
	cacheBytes := flag.Int64("cache-mb", 512, "chunk cache size in MB")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: templates-build --dir <rootfs-dir> [--out dir]")
		os.Exit(2)
	}

	if err := run(*dir, *out, int64(*cacheBytes)<<20); err != nil {
		fmt.Fprintln(os.Stderr, "templates-build:", err)
		os.Exit(1)
	}
}

func run(dir, out string, cacheBytes int64) error {
	st, err := snapstore.New(filepath.Join(out, "chunks"), cacheBytes, nil)
	if err != nil {
		return err
	}
	ctx := context.Background()

	blobPath := filepath.Join(out, "rootfs.tar")
	fmt.Printf("==> tarring %s -> %s\n", dir, blobPath)
	if err := tarDir(dir, blobPath); err != nil {
		return fmt.Errorf("tar: %w", err)
	}
	f, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	defer f.Close()

	id := filepath.Base(dir)
	fmt.Printf("==> chunking into content-addressed store\n")
	ix, err := st.PutBlob(ctx, id, f)
	if err != nil {
		return err
	}
	hotPct := 0
	if ix.TotalSize > 0 {
		hotPct = int(ix.HotBytes() * 100 / ix.TotalSize)
	}
	manifest := map[string]any{
		"id":            ix.ID,
		"total_size":    ix.TotalSize,
		"chunk_size":    ix.ChunkSize,
		"chunks":        len(ix.Chunks),
		"hot_set_bytes": ix.HotBytes(),
		"hot_set_pct":   hotPct,
		"index_file":    filepath.Join(out, "chunks", "indexes", id+".json"),
		"blob":          blobPath,
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "template.json"), mb, 0o644); err != nil {
		return err
	}
	fmt.Println(string(mb))
	fmt.Printf("==> register with: subctl templates create --name %s\n", id)
	return nil
}

func tarDir(src, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	return tarTree(src, out)
}
