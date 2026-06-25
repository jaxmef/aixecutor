package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// TestOrchestratorStructuredLoggingPersistsInvocations proves the AIX-0014 wiring
// end-to-end: with a Logger attached, a full run writes structured per-invocation
// records to <run>/logs/aixecutor.log (tagged with each role) AND persists each
// invocation's raw output to <run>/logs/<role>-NNN.out that the log points at.
func TestOrchestratorStructuredLoggingPersistsInvocations(t *testing.T) {
	cfg := orchCfg() // serial execution, one distinct harness per role
	hs := fullPipelineHarnesses()

	root := t.TempDir()
	store, err := run.NewStoreFromConfig(cfg, root,
		run.WithBaseliner(fakeBaseliner{}), run.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	fg := newFakeGit(root)
	reg := orchRegistry(t, cfg, hs)

	var console strings.Builder
	logger := log.New(log.Normal, &console)
	defer logger.Close()

	o, err := NewOrchestrator(store, cfg, reg, fg, prompt.NewRenderer(), fakeSummarizer{summary: "internal/example/*.go"},
		WithOrchestratorProgress(log.NewProgress(&strings.Builder{})),
		WithOrchestratorLogger(logger),
	)
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	r, err := o.Start(context.Background(), "add the example feature")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.Status != run.StatusCompleted {
		t.Fatalf("run status = %q; want completed", r.Status)
	}
	_ = logger.Close() // flush the run log file before reading it

	// The run log file exists and carries structured per-role invocation lines.
	logsDir := run.Layout{RunsDir: store.RunsDir(), ID: r.ID, DocsSubdir: cfg.Paths.DocsSubdir}.LogsDir()
	data, err := os.ReadFile(filepath.Join(logsDir, "aixecutor.log"))
	if err != nil {
		t.Fatalf("reading run log: %v", err)
	}
	logTxt := string(data)
	for _, want := range []string{
		"msg=\"harness invocation\"",
		"role=planner",
		"role=executor",
		"role=subtask-reviewer",
		"role=senior-reviewer",
		"output=", // pointer to the persisted raw output file
	} {
		if !strings.Contains(logTxt, want) {
			t.Errorf("run log missing %q:\n%s", want, logTxt)
		}
	}

	// The pointed-at raw-output files actually exist on disk. Persistence is
	// best-effort and only happens when the invocation produced raw output, so we
	// assert it for the roles whose mocks return Raw (planner + senior reviewer);
	// invocations with empty Raw legitimately have no file (and the log omits the
	// pointer for them).
	for _, role := range []string{"planner", "senior-reviewer"} {
		matches, _ := filepath.Glob(filepath.Join(logsDir, role+"-*.out"))
		if len(matches) == 0 {
			t.Errorf("no persisted raw-output file for role %q under %s", role, logsDir)
			continue
		}
		got, rerr := os.ReadFile(matches[0])
		if rerr != nil || len(got) == 0 {
			t.Errorf("persisted raw-output file for %q is unreadable/empty: %v", role, rerr)
		}
	}
}
