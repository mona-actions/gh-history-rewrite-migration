package migrate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/remap"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/rewriter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// --- fakes ---------------------------------------------------------------

type fakeDoctor struct {
	called bool
	err    error
}

func (f *fakeDoctor) Run(ctx context.Context) error { f.called = true; return f.err }

type fakeExporter struct {
	called bool
	err    error
}

func (f *fakeExporter) Run(ctx context.Context) error { f.called = true; return f.err }

type fakeRewriter struct {
	called bool
	res    *rewriter.Result
	err    error
}

func (f *fakeRewriter) Run(ctx context.Context, inputs ...rewriter.Input) (*rewriter.Result, error) {
	f.called = true
	return f.res, f.err
}

type fakeRemapper struct {
	called bool
	res    remap.Result
	err    error
}

func (f *fakeRemapper) Run(ctx context.Context, in remap.Input) (remap.Result, error) {
	f.called = true
	return f.res, f.err
}

type fakeImporter struct {
	called bool
	err    error
}

func (f *fakeImporter) Run(ctx context.Context) error { f.called = true; return f.err }

// recorder captures every line emitted by the orchestrator's printers
// so tests can assert on user-visible output without touching pterm.
type recorder struct {
	lines []string
	rows  [][]string
}

func (r *recorder) printers() Printers {
	return Printers{
		Info:    func(m string) { r.lines = append(r.lines, "INFO:"+m) },
		Warn:    func(m string) { r.lines = append(r.lines, "WARN:"+m) },
		Error:   func(m string) { r.lines = append(r.lines, "ERR:"+m) },
		Success: func(m string) { r.lines = append(r.lines, "OK:"+m) },
		Table: func(h []string, rows [][]string) {
			r.lines = append(r.lines, "TABLE")
			r.rows = append(r.rows, rows...)
		},
	}
}

func (r *recorder) joined() string { return strings.Join(r.lines, "\n") }

// build constructs an Orchestrator with a fresh recorder. Any of the
// runners can be swapped before calling Run.
func build(t *testing.T,
	d *fakeDoctor, e *fakeExporter, rw *fakeRewriter, rm *fakeRemapper, im *fakeImporter,
	cfg Config,
) (*Orchestrator, *recorder) {
	t.Helper()
	wd, err := workdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	if err := os.WriteFile(wd.RawMetadataArchive(), []byte("raw metadata"), 0o644); err != nil {
		t.Fatalf("write raw metadata: %v", err)
	}
	if err := os.WriteFile(wd.CommitMap(), []byte("old new\n"), 0o644); err != nil {
		t.Fatalf("write commit-map: %v", err)
	}

	rec := &recorder{}
	var dr DoctorRunner
	if d != nil {
		dr = d
	}
	cfg.Resume = true
	o := New(wd, dr, e, rw, rm, im, remap.Input{
		WorkDir:            wd,
		RawMetadataArchive: wd.RawMetadataArchive(),
		CommitMapPath:      wd.CommitMap(),
	}, cfg, rec.printers())
	return o, rec
}

// --- tests ---------------------------------------------------------------

func TestRun_HappyPath(t *testing.T) {
	d := &fakeDoctor{}
	e := &fakeExporter{}
	rw := &fakeRewriter{res: &rewriter.Result{StripPerformed: true}}
	rm := &fakeRemapper{res: remap.Result{}}
	im := &fakeImporter{}

	o, rec := build(t, d, e, rw, rm, im, Config{TargetRepoURL: "https://github.com/x/y"})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	for name, called := range map[string]bool{
		"doctor": d.called, "exporter": e.called, "rewriter": rw.called,
		"remapper": rm.called, "importer": im.called,
	} {
		if !called {
			t.Errorf("%s.Run was not invoked", name)
		}
	}
	if !strings.Contains(rec.joined(), "OK:Migration complete: https://github.com/x/y") {
		t.Errorf("missing success banner with URL; got: %s", rec.joined())
	}
}

func TestRun_DoctorFails_AbortsBeforeOtherPhases(t *testing.T) {
	d := &fakeDoctor{err: errors.New("boom")}
	e := &fakeExporter{}
	rw := &fakeRewriter{}
	rm := &fakeRemapper{}
	im := &fakeImporter{}

	o, _ := build(t, d, e, rw, rm, im, Config{})
	err := o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("expected preflight error, got %v", err)
	}
	if e.called || rw.called || rm.called || im.called {
		t.Errorf("downstream phases must not run when doctor fails")
	}
}

func TestRun_ExporterFails_AbortsBeforeRewriteRemapImport(t *testing.T) {
	e := &fakeExporter{err: errors.New("export down")}
	rw := &fakeRewriter{}
	rm := &fakeRemapper{}
	im := &fakeImporter{}

	o, _ := build(t, nil, e, rw, rm, im, Config{})
	if err := o.Run(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "export phase failed") {
		t.Fatalf("expected export failure, got %v", err)
	}
	if rw.called || rm.called || im.called {
		t.Errorf("downstream phases must not run when export fails")
	}
}

func TestRun_RewriterIdempotentSkip_ContinuesToRemap(t *testing.T) {
	e := &fakeExporter{}
	rw := &fakeRewriter{res: nil, err: nil} // nil/nil = idempotent skip
	rm := &fakeRemapper{res: remap.Result{}}
	im := &fakeImporter{}

	o, rec := build(t, nil, e, rw, rm, im, Config{})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("expected success on idempotent skip, got %v", err)
	}
	if !rm.called || !im.called {
		t.Errorf("remap+import must run after rewrite skip")
	}
	if !strings.Contains(rec.joined(), "Rewrite skipped") {
		t.Errorf("expected idempotent-skip notice in output: %s", rec.joined())
	}
}

func TestRun_RewriterWarningsRendered(t *testing.T) {
	e := &fakeExporter{}
	rw := &fakeRewriter{res: &rewriter.Result{
		Warnings: []string{"GPG signatures stripped", "LFS pointers detected"},
	}}
	rm := &fakeRemapper{res: remap.Result{}}
	im := &fakeImporter{}

	o, rec := build(t, nil, e, rw, rm, im, Config{})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	out := rec.joined()
	if !strings.Contains(out, "GPG signatures stripped") || !strings.Contains(out, "LFS pointers detected") {
		t.Errorf("expected rewriter warnings in output, got: %s", out)
	}
}

func TestRun_RemapError_AbortsBeforeImport(t *testing.T) {
	e := &fakeExporter{}
	rw := &fakeRewriter{res: &rewriter.Result{}}
	rm := &fakeRemapper{err: errors.New("metadata remap failed")}
	im := &fakeImporter{}

	o, _ := build(t, nil, e, rw, rm, im, Config{})
	err := o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remap phase failed") {
		t.Fatalf("expected wrapped remap failure, got %v", err)
	}
	if im.called {
		t.Errorf("importer must NOT run when remap fails")
	}
}

func TestRun_RemapOtherError_Aborts(t *testing.T) {
	e := &fakeExporter{}
	rw := &fakeRewriter{res: &rewriter.Result{}}
	rm := &fakeRemapper{err: errors.New("disk full")}
	im := &fakeImporter{}

	o, _ := build(t, nil, e, rw, rm, im, Config{})
	err := o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remap phase failed") {
		t.Fatalf("expected wrapped remap error, got %v", err)
	}
	if im.called {
		t.Errorf("importer must NOT run when remap fails")
	}
}

func TestRun_ImporterFails_ReturnsError(t *testing.T) {
	e := &fakeExporter{}
	rw := &fakeRewriter{res: &rewriter.Result{}}
	rm := &fakeRemapper{res: remap.Result{}}
	im := &fakeImporter{err: errors.New("gei exit 1")}

	o, _ := build(t, nil, e, rw, rm, im, Config{})
	err := o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "import phase failed") {
		t.Fatalf("expected import error, got %v", err)
	}
	// Rewrite + remap must have completed; we explicitly do NOT
	// clean up artifacts on import failure so the user can re-run.
	if !rw.called || !rm.called {
		t.Errorf("rewrite+remap must have completed before import")
	}
}

func TestRun_NoCommitMap_CopiesRawMetadataAndSkipsRemap(t *testing.T) {
	wd, err := workdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	if err := os.WriteFile(wd.RawMetadataArchive(), []byte("raw metadata"), 0o644); err != nil {
		t.Fatalf("write raw metadata: %v", err)
	}

	e := &fakeExporter{}
	rw := &fakeRewriter{res: &rewriter.Result{}}
	rm := &fakeRemapper{}
	im := &fakeImporter{}
	rec := &recorder{}
	o := New(wd, nil, e, rw, rm, im, remap.Input{
		WorkDir:            wd,
		RawMetadataArchive: wd.RawMetadataArchive(),
		CommitMapPath:      wd.CommitMap(),
	}, Config{Resume: true}, rec.printers())

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if rm.called {
		t.Fatal("remapper should not run when no commit-map exists")
	}
	if !im.called {
		t.Fatal("importer should run when remap is skipped")
	}
	got, err := os.ReadFile(wd.MetadataArchive())
	if err != nil {
		t.Fatalf("read final metadata archive: %v", err)
	}
	if string(got) != "raw metadata" {
		t.Fatalf("expected final metadata archive to match raw metadata, got %q", string(got))
	}
	if !strings.Contains(rec.joined(), "no rewrites performed; using original metadata archive") {
		t.Fatalf("expected no-op remap warning, got %s", rec.joined())
	}
}

func TestNew_NilPrintersAreNoOp(t *testing.T) {
	// Construct with empty Printers and ensure Run doesn't panic.
	o := New(nil, nil,
		&fakeExporter{}, &fakeRewriter{res: &rewriter.Result{}},
		&fakeRemapper{res: remap.Result{}}, &fakeImporter{},
		remap.Input{}, Config{}, Printers{})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error with nil printers: %v", err)
	}
}
