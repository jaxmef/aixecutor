package run

import (
	"context"

	"github.com/jaxmef/aixecutor/internal/git"
)

// BaselineSource is anything that can snapshot a working tree into a directory and
// return a git.Baseline — both the single-repo *git.Gateway and the multi-root
// *workspace.Workspace (AIX-0020) satisfy it, so GitBaseliner adapts either to the
// run package's Baseliner seam without the run package depending on the workspace
// type. The capture performs no mutating git (read-only enumeration + raw file I/O).
type BaselineSource interface {
	CaptureBaseline(ctx context.Context, dstDir string, warn func(bytes int64)) (git.Baseline, error)
}

// GitBaseliner adapts a read-only BaselineSource (a git gateway or a workspace) to
// the run package's Baseliner seam. It is the production implementation injected
// into a Store by the CLI; tests inject a fake Baseliner instead so they never touch
// a real repository or run a git command.
type GitBaseliner struct {
	gw BaselineSource
	// ctx bounds the capture; a nil ctx defaults to context.Background().
	ctx context.Context
	// warn, if set, is invoked once when the snapshot exceeds the soft size ceiling
	// (it forwards the caller's logger). May be nil.
	warn func(bytes int64)
}

// NewGitBaseliner builds a GitBaseliner over gw. ctx bounds the capture (nil →
// Background); warn (optional) is forwarded to the size-guard callback.
func NewGitBaseliner(gw BaselineSource, ctx context.Context, warn func(bytes int64)) *GitBaseliner {
	if ctx == nil {
		ctx = context.Background()
	}
	return &GitBaseliner{gw: gw, ctx: ctx, warn: warn}
}

// CaptureBaseline snapshots the gateway's working tree into dstDir and converts
// the git Baseline into the run package's Baseline (path + file/byte counts), so
// the persisted run.yaml stays independent of git's internal types.
func (b *GitBaseliner) CaptureBaseline(dstDir string) (Baseline, error) {
	gb, err := b.gw.CaptureBaseline(b.ctx, dstDir, b.warn)
	if err != nil {
		return Baseline{}, err
	}
	return Baseline{
		Dir:   gb.Dir(),
		Files: len(gb.Snapshot.Files),
		Bytes: gb.Snapshot.Bytes,
	}, nil
}
