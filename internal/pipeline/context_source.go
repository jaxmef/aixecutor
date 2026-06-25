package pipeline

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/jaxmef/aixecutor/internal/run"
)

// runContextProvider is the default contextProvider: it reads the planner's
// docs/context.md once (lazily, on first use) and returns the whole document as the
// context excerpt for every subtask. Handing the full context to each executor is a
// safe, simple v1: it is the information the planner deemed relevant to the task.
// Per-subtask slicing is a future refinement that can replace this provider without
// touching the scheduler (it is injected via WithContextProvider).
//
// The read is best-effort: a missing or unreadable context.md yields an empty
// excerpt rather than an error, because the executor prompt tolerates an empty
// context and a run should not fail merely because the optional context doc is
// absent (e.g. a hand-seeded test run).
type runContextProvider struct {
	contextFile string

	once    sync.Once
	excerpt string
}

// newRunContextProvider builds a provider that reads <run docs>/context.md, resolved
// through the same layout the store uses (so it matches where planning wrote it).
func newRunContextProvider(store *run.Store, r *run.Run) *runContextProvider {
	return &runContextProvider{
		contextFile: filepath.Join(store.DocsDir(r.ID), contextDocName),
	}
}

// ContextExcerpt returns the context document text for st. The same full document is
// returned for every subtask in this implementation; it is loaded once and cached.
// sync.Once makes the lazy load safe under the scheduler's parallel workers.
func (p *runContextProvider) ContextExcerpt(_ run.Subtask) string {
	p.once.Do(func() {
		data, err := os.ReadFile(p.contextFile)
		if err != nil {
			p.excerpt = "" // optional doc; absence is not an error.
			return
		}
		p.excerpt = string(data)
	})
	return p.excerpt
}
