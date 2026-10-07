package main

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tarTree writes a deterministic (sorted, uid/gid 0, mtime 0) tar of src.
// Determinism matters: identical template content must produce identical
// chunk hashes for cross-node dedup (plan §3.5).
func tarTree(src string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := strings.TrimPrefix(filepath.ToSlash(rel), "./")

		hdr := &tar.Header{
			Name:    name,
			Mode:    int64(info.Mode().Perm()),
			ModTime: timeZero,
			Uid:     0, Gid: 0, Uname: "root", Gname: "root",
		}
		switch {
		case info.IsDir():
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = link
			return tw.WriteHeader(hdr)
		case !info.Mode().IsRegular():
			// FIFOs, sockets, device nodes are not part of a rootfs payload;
			// opening a FIFO with no writer blocks the build forever.
			return nil
		default:
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			// Size comes from the OPEN fd, not the Walk stat: a writer
			// mutating the tree mid-archive otherwise desynchronizes the
			// stream from the header. CopyN caps the copy at that size, so
			// post-stat growth truncates deterministically instead of
			// corrupting every subsequent chunk hash.
			fi, serr := f.Stat()
			if serr != nil {
				f.Close()
				return serr
			}
			hdr.Typeflag = tar.TypeReg
			hdr.Size = fi.Size()
			if err := tw.WriteHeader(hdr); err != nil {
				f.Close()
				return err
			}
			_, err = io.CopyN(tw, f, fi.Size())
			if err == io.EOF {
				err = io.ErrUnexpectedEOF // shrank under us: refuse silently-shrunk archive
			}
			cerr := f.Close()
			if err != nil {
				return err
			}
			return cerr
		}
	})
}

var timeZero = time.Time{}
