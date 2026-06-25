package git

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Snapshot is a point-in-time copy of a set of repo files laid out under a
// destination directory, preserving each file's repo-relative path. Diffs are
// computed with `git diff --no-index <before> <after>` over two snapshot dirs
// (read-only), so a snapshot is just plain files on disk — git is never asked to
// record or stage anything.
//
// File contents are copied with raw os file I/O, never via git, so snapshotting
// performs no git writes. Only the paths handed to the snapshot are copied;
// callers enumerate those paths via the gateway's read-only ls-files/status
// helpers (baseline) or by matching declared globs (per-subtask), so ignored
// paths are excluded upstream and never reach here.
type Snapshot struct {
	// Dir is the absolute root under which the snapshot's files live. The file
	// originally at <repoRoot>/<rel> is copied to <Dir>/<rel>.
	Dir string
	// Files is the sorted list of repo-relative paths captured, for reporting
	// and tests. Paths use the OS separator as produced by filepath.
	Files []string
	// Bytes is the total number of bytes copied, for the size guard / logging.
	Bytes int64
}

// maxSnapshotBytes is the soft size ceiling for a single snapshot. Past this,
// snapshotFiles invokes the optional warn callback (the caller's logger) once so
// a pathological tree is surfaced rather than silently copied. It is a warning,
// not a hard limit: correctness must not depend on tree size, and the user may
// legitimately have a large working set. ~256 MiB is generous for source trees
// while still catching "someone pointed us at a data lake" mistakes.
const maxSnapshotBytes int64 = 256 << 20

// SnapshotFiles copies each srcRoot-relative path in rels into dstDir, preserving
// structure, via raw file I/O (no git). It is the exported engine the workspace
// layer (AIX-0020) uses to snapshot a multi-root file set into one snapshot dir,
// the same engine CaptureBaseline/SnapshotPaths use for a single repo.
func SnapshotFiles(srcRoot, dstDir string, rels []string, warn func(bytes int64)) (Snapshot, error) {
	return snapshotFiles(srcRoot, dstDir, rels, warn)
}

// snapshotFiles copies each repo-relative path in rels from srcRoot into a fresh
// layout under dstDir, preserving relative structure. It is the shared engine
// behind baseline and per-subtask snapshots.
//
//   - Paths that do not exist under srcRoot are skipped (not an error): a file a
//     subtask declares but has not yet created simply has no "before" content, so
//     git diff --no-index reports it as an addition — exactly what we want.
//   - Directories are walked recursively; only regular files are copied. Symlinks
//     are skipped (we copy content, not link targets, and never follow a link out
//     of the tree).
//   - If warn is non-nil and the cumulative size crosses maxSnapshotBytes, warn is
//     called once with the running total; copying then continues.
//
// On any I/O failure it returns a wrapped, actionable error; partial output may
// exist under dstDir and is the caller's to clean up (callers place dstDir under
// a run/temp dir).
func snapshotFiles(srcRoot, dstDir string, rels []string, warn func(bytes int64)) (Snapshot, error) {
	return snapshotFilesWithLimit(srcRoot, dstDir, rels, maxSnapshotBytes, warn)
}

// snapshotFilesWithLimit is snapshotFiles with the soft size ceiling injected.
// snapshotFiles passes maxSnapshotBytes; tests pass a small limit so the size
// guard can be exercised without writing hundreds of megabytes.
func snapshotFilesWithLimit(srcRoot, dstDir string, rels []string, limit int64, warn func(bytes int64)) (Snapshot, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("git snapshot: creating %q: %w", dstDir, err)
	}

	snap := Snapshot{Dir: dstDir}
	warned := false
	seen := map[string]bool{}

	add := func(rel string, size int64) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		snap.Files = append(snap.Files, rel)
		snap.Bytes += size
		if warn != nil && !warned && snap.Bytes > limit {
			warned = true
			warn(snap.Bytes)
		}
	}

	for _, rel := range rels {
		clean := filepath.Clean(rel)
		if clean == "." || clean == "" {
			continue
		}
		// Reject paths that try to escape the repo root; declared globs and git
		// output should never produce these, but we guard defensively so a bad
		// input cannot make us read or write outside the tree.
		if filepath.IsAbs(clean) || clean == ".." || hasDotDotPrefix(clean) {
			return snap, fmt.Errorf("git snapshot: path %q escapes the repository root", rel)
		}

		src := filepath.Join(srcRoot, clean)
		info, err := os.Lstat(src)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // missing path: no "before" content; skip.
			}
			return snap, fmt.Errorf("git snapshot: stat %q: %w", src, err)
		}

		switch {
		case info.IsDir():
			if err := walkDirInto(src, clean, dstDir, add); err != nil {
				return snap, err
			}
		case info.Mode().IsRegular():
			n, err := copyFileInto(src, clean, dstDir)
			if err != nil {
				return snap, err
			}
			add(clean, n)
		default:
			// Symlink, device, socket, etc.: skip — we snapshot regular file
			// content only.
		}
	}

	sort.Strings(snap.Files)
	return snap, nil
}

// walkDirInto recursively copies the regular files under srcDir (whose
// repo-relative path is relPrefix) into dstDir, preserving structure, invoking
// add for each copied file.
func walkDirInto(srcDir, relPrefix, dstDir string, add func(rel string, size int64)) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("git snapshot: walking %q: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks/specials
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("git snapshot: relativizing %q: %w", path, err)
		}
		repoRel := filepath.Join(relPrefix, rel)
		n, err := copyFileInto(path, repoRel, dstDir)
		if err != nil {
			return err
		}
		add(repoRel, n)
		return nil
	})
}

// copyFileInto copies the regular file at src to <dstDir>/<repoRel>, creating
// parent directories, and returns the number of bytes copied. Content is copied
// with io.Copy over os file handles — never git — so the snapshot involves no
// git writes.
func copyFileInto(src, repoRel, dstDir string) (int64, error) {
	dst := filepath.Join(dstDir, repoRel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("git snapshot: creating dir for %q: %w", dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("git snapshot: opening %q: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("git snapshot: creating %q: %w", dst, err)
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("git snapshot: copying %q -> %q: %w", src, dst, err)
	}
	return n, nil
}

// hasDotDotPrefix reports whether a cleaned, relative path begins with a ".."
// segment (i.e. would escape its root). filepath.Clean keeps leading ".." for
// relative paths, so checking the first segment is sufficient.
func hasDotDotPrefix(clean string) bool {
	return len(clean) >= 3 && clean[0] == '.' && clean[1] == '.' && (clean[2] == filepath.Separator)
}
