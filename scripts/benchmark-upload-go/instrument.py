"""Add opt-in phase recording to a temporary copy of a TOUKO source tree.

The benchmark must not edit the measured checkout and must not ship profiling
code in production builds. This module copies ``go.mod``, ``go.sum`` and
``internal`` into a scratch directory, adds the benchmark-only ``benchrec``
package and inserts timing spans at fixed anchors. Every anchor is required to
occur exactly once: if a source revision does not match, preparation fails with
the file and anchor instead of building different code.

The inserted spans measure wall-clock time with Go's monotonic ``time.Since``.
``benchrec`` records nothing until the runner calls ``Enable``, and this module
is only used for the temporary benchmark build.
"""

import hashlib
import os
import shutil

BENCHREC = '''// Package benchrec records opt-in wall-clock phase durations for the upload
// benchmark. It is injected into a temporary source copy by
// scripts/benchmark-upload-go/instrument.py; production builds never contain
// this package or its call sites. Nothing is recorded until Enable is called.
package benchrec

import (
\t"io"
\t"sync"
\t"time"
)

var (
\tmu      sync.Mutex
\tenabled bool
\tphases  = make(map[string]int64)
)

// Enable starts a fresh recording before the timed operation.
func Enable() {
\tmu.Lock()
\tenabled = true
\tphases = make(map[string]int64)
\tmu.Unlock()
}

// Take returns the recorded phase durations in nanoseconds and clears them.
func Take() map[string]int64 {
\tmu.Lock()
\tdefer mu.Unlock()
\tout := make(map[string]int64, len(phases))
\tfor name, ns := range phases {
\t\tout[name] = ns
\t}
\tphases = make(map[string]int64)
\treturn out
}

func active() bool {
\tmu.Lock()
\tdefer mu.Unlock()
\treturn enabled
}

func add(name string, duration time.Duration) {
\tif duration <= 0 {
\t\treturn
\t}
\tmu.Lock()
\tif enabled {
\t\tphases[name] += duration.Nanoseconds()
\t}
\tmu.Unlock()
}

// Span times the work between the call and the returned stop function. A
// disabled recorder returns a no-op.
func Span(name string) func() {
\tif !active() {
\t\treturn func() {}
\t}
\tstarted := time.Now()
\treturn func() { add(name, time.Since(started)) }
}

// Writer times the writes it forwards, attributing SHA-256 work without a
// second read of the image.
type Writer struct {
\tW    io.Writer
\tName string
}

func (w Writer) Write(p []byte) (int, error) {
\tif !active() {
\t\treturn w.W.Write(p)
\t}
\tstarted := time.Now()
\tn, err := w.W.Write(p)
\tadd(w.Name, time.Since(started))
\treturn n, err
}
'''


class InstrumentationError(RuntimeError):
    pass


def _replace(text, rel, old, new, occurrences=1):
    found = text.count(old)
    if found != occurrences:
        raise InstrumentationError(
            f"{rel}: expected {occurrences} instrumentation anchor(s), found {found}: {old[:120]!r}"
        )
    return text.replace(old, new)


def _rewrite(root, rel, edits):
    path = os.path.join(root, rel)
    try:
        with open(path, encoding="utf-8") as handle:
            text = handle.read()
    except OSError as error:
        raise InstrumentationError(f"cannot read {rel}: {error}")
    for old, new, occurrences in edits:
        text = _replace(text, rel, old, new, occurrences)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text)


def _import(anchor):
    return ("\t" + anchor, "\t\"github.com/Eurekaimer/TOUKO/internal/benchrec\"\n\t" + anchor, 1)


def _stage_add_edits():
    return [
        _import('"github.com/Eurekaimer/TOUKO/internal/domain"'),
        ("""\t\tresult, err := ingest.IngestWithOptions(ctx, input.Reader, tempDir, ingest.Options{
\t\t\tConvertWebP: opts.ConvertWebP,
\t\t\tWebPQuality: opts.WebPQuality,
\t\t})
\t\tif err != nil {""",
         """\t\tstopIngest := benchrec.Span("add.ingest")
\t\tresult, err := ingest.IngestWithOptions(ctx, input.Reader, tempDir, ingest.Options{
\t\t\tConvertWebP: opts.ConvertWebP,
\t\t\tWebPQuality: opts.WebPQuality,
\t\t})
\t\tstopIngest()
\t\tif err != nil {""", 1),
        ("""\t\titem, err := PrepareInput(result, input.Name, AddOptions{Namespace: ns, Directory: opts.Directory, Title: opts.Title, Tags: opts.Tags, Collections: opts.Collections})
\t\tif err != nil {""",
         """\t\tstopMetadata := benchrec.Span("add.metadata")
\t\titem, err := PrepareInput(result, input.Name, AddOptions{Namespace: ns, Directory: opts.Directory, Title: opts.Title, Tags: opts.Tags, Collections: opts.Collections})
\t\tstopMetadata()
\t\tif err != nil {""", 1),
        ("""\tnext, err := applyPrepared(st, prepared)
\tif err != nil {""",
         """\tstopPrepare := benchrec.Span("add.metadata")
\tnext, err := applyPrepared(st, prepared)
\tstopPrepare()
\tif err != nil {""", 1),
        ("""\tout, err := s.installPrepared(ctx, st, next, prepared)
\tif err != nil {""",
         """\tstopInstall := benchrec.Span("add.install")
\tout, err := s.installPrepared(ctx, st, next, prepared)
\tstopInstall()
\tif err != nil {""", 1),
    ]


def _prepare_edits():
    return [
        _import('"github.com/Eurekaimer/TOUKO/internal/domain"'),
        ("""\tif err := s.Store.Save(ctx, next); err != nil {
\t\treturn nil, err
\t}
\tsuccess = true""",
         """\tstopSave := benchrec.Span("add.save")
\terr := s.Store.Save(ctx, next)
\tstopSave()
\tif err != nil {
\t\treturn nil, err
\t}
\tsuccess = true""", 1),
    ]


def _staging_edits():
    return [
        _import('"github.com/Eurekaimer/TOUKO/internal/domain"'),
        ("""func verifyFile(ctx context.Context, path string, object domain.ContentObject, syncFile bool) error {
\tif err := ctx.Err(); err != nil {""",
         """func verifyFile(ctx context.Context, path string, object domain.ContentObject, syncFile bool) error {
\tstopVerify := benchrec.Span("add.verify")
\tdefer stopVerify()
\tif err := ctx.Err(); err != nil {""", 1),
    ]


def _commit_edits():
    return [
        _import('"github.com/Eurekaimer/TOUKO/internal/domain"'),
        ("""\thead := st.BaseHead
\tbase, err := repository.ReadSnapshot(ctx, r, head)""",
         """\tstopPrepare := benchrec.Span("commit.prepare")
\thead := st.BaseHead
\tbase, err := repository.ReadSnapshot(ctx, r, head)""", 1),
        ("""\tif plan.Empty() {
\t\treturn CommitResult{Head: head, AssetCount: len(base.Assets)}, nil
\t}""",
         """\tstopPrepare()
\tif plan.Empty() {
\t\treturn CommitResult{Head: head, AssetCount: len(base.Assets)}, nil
\t}""", 1),
        ("""\tfor _, p := range plan.ReusedObjects {
\t\tif err = repository.VerifyObject(ctx, r, p.Blob, p.Object); err != nil {
\t\t\treturn CommitResult{}, err
\t\t}
\t}""",
         """\tfor _, p := range plan.ReusedObjects {
\t\tstopVerify := benchrec.Span("commit.verify")
\t\tverifyErr := repository.VerifyObject(ctx, r, p.Blob, p.Object)
\t\tstopVerify()
\t\tif verifyErr != nil {
\t\t\treturn CommitResult{}, verifyErr
\t\t}
\t}""", 1),
        ("""\tfor _, p := range plan.NewObjects {
\t\tblob, err := s.uploadStaged(ctx, r, p)
\t\tif err != nil {""",
         """\tfor _, p := range plan.NewObjects {
\t\tstopUpload := benchrec.Span("commit.upload")
\t\tblob, err := s.uploadStaged(ctx, r, p)
\t\tstopUpload()
\t\tif err != nil {""", 1),
        ("""\tfor _, write := range plan.MetadataWrites {
\t\tblob, err := r.CreateBlob(ctx, bytes.NewReader(write.Data), int64(len(write.Data)))
\t\tif err != nil {""",
         """\tfor _, write := range plan.MetadataWrites {
\t\tstopMetadataBlob := benchrec.Span("commit.metadata_blob")
\t\tblob, err := r.CreateBlob(ctx, bytes.NewReader(write.Data), int64(len(write.Data)))
\t\tstopMetadataBlob()
\t\tif err != nil {""", 1),
        ("""\ttree, err := r.CreateTree(ctx, base.Tree, changes)
\tif err != nil {""",
         """\tstopTree := benchrec.Span("commit.tree")
\ttree, err := r.CreateTree(ctx, base.Tree, changes)
\tstopTree()
\tif err != nil {""", 1),
        ("""\tcandidate, err := r.CreateCommit(ctx, tree, base.Head, message)
\tif err != nil {""",
         """\tstopCommit := benchrec.Span("commit.commit")
\tcandidate, err := r.CreateCommit(ctx, tree, base.Head, message)
\tstopCommit()
\tif err != nil {""", 1),
        ("""\tsnap, err := plannedSnapshot(base, plan, blobs, candidate, tree)
\tif err != nil {""",
         """\tstopPlan := benchrec.Span("commit.plan")
\tsnap, err := plannedSnapshot(base, plan, blobs, candidate, tree)
\tstopPlan()
\tif err != nil {""", 1),
        ("""\tif err = s.Store.SavePublication(ctx, st.Publication); err != nil {
\t\treturn CommitResult{}, err
\t}""",
         """\tstopJournal := benchrec.Span("commit.journal")
\terr = s.Store.SavePublication(ctx, st.Publication)
\tstopJournal()
\tif err != nil {
\t\treturn CommitResult{}, err
\t}""", 1),
        ("""\tif err := r.UpdateRef(ctx, p.BaseHead, p.CandidateHead); err != nil {""",
         """\tstopRef := benchrec.Span("commit.ref")
\terr := r.UpdateRef(ctx, p.BaseHead, p.CandidateHead)
\tstopRef()
\tif err != nil {""", 1),
        ("""\tnext := snapshotState(st, snap, true)
\tif err := s.Store.Save(ctx, next); err != nil {""",
         """\tnext := snapshotState(st, snap, true)
\tstopFinalize := benchrec.Span("commit.finalize")
\terr := s.Store.Save(ctx, next)
\tstopFinalize()
\tif err != nil {""", 1),
        ("""\tif err := s.cleanup(ctx, next); err != nil {
\t\treturn result, fmt.Errorf("published head %s; local payload cleanup failed: %w", head, err)
\t}""",
         """\tstopCleanup := benchrec.Span("commit.cleanup")
\tcleanupErr := s.cleanup(ctx, next)
\tstopCleanup()
\tif cleanupErr != nil {
\t\treturn result, fmt.Errorf("published head %s; local payload cleanup failed: %w", head, cleanupErr)
\t}""", 1),
    ]


def _cloudflare_edits():
    return [
        _import('"github.com/Eurekaimer/TOUKO/internal/credential"'),
        ("""\tetag, err := r.mirror.PutObject(ctx, key, file, r2.PutOptions{
\t\tObject: ref.object, RepositoryID: r.repository, SourceID: r.sourceID, IfMatch: ifMatch,
\t})
\tif err != nil {""",
         """\tstopPut := benchrec.Span("commit.r2_put")
\tetag, err := r.mirror.PutObject(ctx, key, file, r2.PutOptions{
\t\tObject: ref.object, RepositoryID: r.repository, SourceID: r.sourceID, IfMatch: ifMatch,
\t})
\tstopPut()
\tif err != nil {""", 1),
    ]


def _ingest_edits():
    return [
        _import('"github.com/Eurekaimer/TOUKO/internal/domain"'),
        ("""\thash := sha256.New()
\tsize, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, source}, MaxBytes+1))""",
         """\tstopStream := benchrec.Span("ingest.stream")
\tdigest := sha256.New()
\thash := benchrec.Writer{W: digest, Name: "ingest.hash"}
\tsize, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, source}, MaxBytes+1))""", 1),
        ("""\tfile = nil
\t// Decode only the persisted header; never rewind or re-read the source.""",
         """\tfile = nil
\tstopStream()
\t// Decode only the persisted header; never rewind or re-read the source.""", 1),
        ("""\tconfig, format, decodeErr := stdimage.DecodeConfig(contextReader{ctx, header})
\tcloseErr := header.Close()""",
         """\tstopDecode := benchrec.Span("ingest.decode")
\tconfig, format, decodeErr := stdimage.DecodeConfig(contextReader{ctx, header})
\tstopDecode()
\tcloseErr := header.Close()""", 1),
        ("""\t\tconverted, err := convertToWebP(ctx, filePath, tempDir, opts.WebPQuality)
\t\tif err != nil {""",
         """\t\tstopWebP := benchrec.Span("ingest.webp")
\t\tconverted, err := convertToWebP(ctx, filePath, tempDir, opts.WebPQuality)
\t\tstopWebP()
\t\tif err != nil {""", 1),
        ("""\thash.Sum(object.Hash[:0])""",
         """\tdigest.Sum(object.Hash[:0])""", 1),
        ("""\tdecoded, _, decodeErr := stdimage.Decode(contextReader{ctx, source})
\tcloseErr := source.Close()""",
         """\tstopDecode := benchrec.Span("ingest.decode")
\tdecoded, _, decodeErr := stdimage.Decode(contextReader{ctx, source})
\tstopDecode()
\tcloseErr := source.Close()""", 1),
        ("""\tdigest := sha256.New()
\tif err := webp.Encode(io.MultiWriter(output, digest), decoded, webp.Options{Quality: quality}); err != nil {""",
         """\tdigest := sha256.New()
\ttimedDigest := benchrec.Writer{W: digest, Name: "ingest.hash"}
\tif err := webp.Encode(io.MultiWriter(output, timedDigest), decoded, webp.Options{Quality: quality}); err != nil {""", 1),
    ]


def _wrap_cloudflare_publication(root):
    """The publication guard differs between revisions; insert spans around the
    call line alone so both guarded and unguarded forms are supported."""
    rel = "internal/app/commit.go"
    path = os.path.join(root, rel)
    with open(path, encoding="utf-8") as handle:
        text = handle.read()
    updated = False
    for indent in ("\t\t", "\t"):
        call = f"{indent}result.Cloudflare = s.cloudflarePublication(ctx, r, st, snap)"
        if text.count(call) == 1:
            text = text.replace(
                call,
                f"{indent}stopR2 := benchrec.Span(\"commit.r2\")\n{call}\n{indent}stopR2()",
            )
            updated = True
            break
    if not updated:
        raise InstrumentationError(
            f"{rel}: expected one cloudflarePublication call, found a different shape"
        )
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text)


def _sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def prepare_source(source, destination):
    """Copy and instrument one source tree. Returns a metadata dictionary.

    ``source`` is never modified. ``destination`` must not exist.
    """
    if os.path.exists(destination):
        raise InstrumentationError(f"instrumented source already exists: {destination}")
    for required in ("go.mod", "internal"):
        if not os.path.exists(os.path.join(source, required)):
            raise InstrumentationError(f"{source} is not a TOUKO source tree: missing {required}")
    if os.path.exists(os.path.join(source, "internal", "benchrec")):
        raise InstrumentationError(
            "source already contains internal/benchrec; the benchmark refuses to overwrite it"
        )
    os.makedirs(destination)
    for name in ("go.mod", "go.sum"):
        path = os.path.join(source, name)
        if os.path.exists(path):
            shutil.copy2(path, os.path.join(destination, name))
    shutil.copytree(os.path.join(source, "internal"), os.path.join(destination, "internal"))
    benchrec_dir = os.path.join(destination, "internal", "benchrec")
    os.makedirs(benchrec_dir, exist_ok=True)
    with open(os.path.join(benchrec_dir, "benchrec.go"), "w", encoding="utf-8") as handle:
        handle.write(BENCHREC)

    plan = {
        "internal/app/stage.go": _stage_add_edits(),
        "internal/app/prepare.go": _prepare_edits(),
        "internal/app/commit.go": _commit_edits(),
        "internal/app/cloudflare.go": _cloudflare_edits(),
        "internal/image/ingest.go": _ingest_edits(),
        "internal/staging/files.go": _staging_edits(),
    }
    for rel, edits in plan.items():
        _rewrite(destination, rel, edits)
    _wrap_cloudflare_publication(destination)

    files = ["internal/benchrec/benchrec.go", *plan]
    return {
        "source": os.path.abspath(source),
        "instrumented_source": os.path.abspath(destination),
        "benchrec": "github.com/Eurekaimer/TOUKO/internal/benchrec",
        "files": {name: _sha256(os.path.join(destination, name)) for name in files},
        "overhead_note": (
            "spans add time.Now/Since pairs and one map update per phase; "
            "phase timings are measured inside the same process as production work"
        ),
    }
