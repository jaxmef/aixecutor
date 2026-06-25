package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/config"
)

// TestRegistryFromDefaultResolvesRoles builds a registry from the hardcoded
// default config and resolves every role's harness by name — the path the
// pipeline will take.
func TestRegistryFromDefaultResolvesRoles(t *testing.T) {
	cfg := config.Default()
	reg, err := NewRegistry(cfg, Options{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Every harness in the config must be present.
	for name := range cfg.Harnesses {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("registry missing harness %q", name)
		}
	}

	// Every role's harness must resolve.
	roleHarnesses := []string{
		cfg.Roles.Planner.Harness,
		cfg.Roles.Executor.Harness,
		cfg.Roles.SubtaskReviewer.Harness,
		cfg.Roles.SeniorReviewer.Harness,
	}
	for _, name := range roleHarnesses {
		h, ok := reg.Get(name)
		if !ok {
			t.Errorf("role harness %q not resolvable", name)
			continue
		}
		if h.Name() != name {
			t.Errorf("resolved harness Name = %q, want %q", h.Name(), name)
		}
	}
}

// TestRegistryUnknownLookup checks an unknown name returns (_, false).
func TestRegistryUnknownLookup(t *testing.T) {
	reg, err := NewRegistry(config.Default(), Options{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if h, ok := reg.Get("does-not-exist"); ok || h != nil {
		t.Errorf("Get(unknown) = (%v, %v), want (nil, false)", h, ok)
	}
}

// TestRegistryNamesSorted checks Names returns the harness keys sorted.
func TestRegistryNamesSorted(t *testing.T) {
	reg, err := NewRegistry(config.Default(), Options{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got := reg.Names()
	want := []string{"claude", "pi"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Names = %v, want %v", got, want)
	}
}

// TestRegistryDryRunWrapsAll checks that with DryRun set, resolved harnesses are
// dry-run wrappers that never execute (proved by giving a bogus command and
// confirming Run still succeeds with the placeholder).
func TestRegistryDryRunWrapsAll(t *testing.T) {
	cfg := config.Default()
	reg, err := NewRegistry(cfg, Options{DryRun: true})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h, ok := reg.Get("claude")
	if !ok {
		t.Fatal("claude not found")
	}
	res, err := h.Run(context.Background(), Request{Prompt: "x", Model: "opus"})
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if !strings.HasPrefix(res.Text, "[dry-run]") {
		t.Errorf("expected dry-run placeholder, got %q", res.Text)
	}
}

// TestRegistryPropagatesBuildError checks a malformed harness config fails
// registry construction with a harness-named error.
func TestRegistryPropagatesBuildError(t *testing.T) {
	cfg := config.Default()
	// Corrupt one harness's arg template.
	bad := cfg.Harnesses["claude"]
	bad.Args = []string{"{{.Prompt"} // unterminated action
	cfg.Harnesses["claude"] = bad

	_, err := NewRegistry(cfg, Options{})
	if err == nil {
		t.Fatal("expected build error from malformed template, got nil")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the harness, got: %v", err)
	}
}

// TestRegistryFactoryOverride checks a registered Factory is used instead of the
// generic adapter for its name, and other names still build generically.
func TestRegistryFactoryOverride(t *testing.T) {
	cfg := config.Default()
	sentinel := NewMock("claude") // stand-in built by the factory
	reg, err := NewRegistry(cfg, Options{
		Factories: map[string]Factory{
			"claude": func(name string, _ config.Harness) (Harness, error) {
				if name != "claude" {
					t.Errorf("factory got name %q, want claude", name)
				}
				return sentinel, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, ok := reg.Get("claude")
	if !ok {
		t.Fatal("claude not found")
	}
	// The registry wraps every real harness in the retry layer (AIX-0014); unwrap
	// it to reach the factory-built harness.
	if unwrapHarness(got) != sentinel {
		t.Errorf("Get(claude) did not return the factory-built harness (got %T)", got)
	}
	// pi has no factory, so it must still be a generic CLI harness under the retry
	// wrapper.
	pi, ok := reg.Get("pi")
	if !ok {
		t.Fatal("pi not found")
	}
	if _, isCLI := unwrapHarness(pi).(*cliHarness); !isCLI {
		t.Errorf("pi = %T, want *cliHarness under retry (no factory registered)", pi)
	}
}

// unwrapHarness peels the registry's retry/dry-run decorators off a Harness so a
// test can assert on the underlying concrete type. It follows any number of
// wrappers that expose Unwrap() Harness.
func unwrapHarness(h Harness) Harness {
	for {
		u, ok := h.(interface{ Unwrap() Harness })
		if !ok {
			return h
		}
		h = u.Unwrap()
	}
}

// TestRegistryFactoryError checks a factory error is wrapped with context.
func TestRegistryFactoryError(t *testing.T) {
	boom := errors.New("factory boom")
	_, err := NewRegistry(config.Default(), Options{
		Factories: map[string]Factory{
			"claude": func(string, config.Harness) (Harness, error) { return nil, boom },
		},
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}

// TestRegistryFactoryDryRunWraps checks factory-built harnesses are also wrapped
// under DryRun.
func TestRegistryFactoryDryRunWraps(t *testing.T) {
	cfg := config.Default()
	inner := &recordingHarness{name: "claude"}
	reg, err := NewRegistry(cfg, Options{
		DryRun: true,
		Factories: map[string]Factory{
			"claude": func(string, config.Harness) (Harness, error) { return inner, nil },
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h, _ := reg.Get("claude")
	if _, err := h.Run(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inner.called {
		t.Error("factory-built inner harness ran under dry-run")
	}
}
