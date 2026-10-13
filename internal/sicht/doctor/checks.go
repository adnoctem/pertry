package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/pkg/infer"
	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
)

// MinOllamaVersion is the oldest Ollama whose logprob reporting sicht's confidence scoring needs
// (docs/ARCHITECTURE.md#doctor). Below it, confidence falls back to the opt-in agreement mode.
const MinOllamaVersion = "0.12.11"

// Ollama checks that the server is reachable and recent enough.
//
// sicht never starts Ollama (docs/ARCHITECTURE.md#inference-backend-and-model-choice), so an unreachable server is a fail with instructions rather than
// something to paper over. The WSL case gets its own hint because it is the most common cause: a
// Windows-hosted Ollama is not reachable from WSL on localhost unless mirrored networking is on.
func Ollama(ctx context.Context, backend *ollamabackend.Backend) (Result, infer.ServerInfo) {
	const name = "ollama"

	info, err := backend.Health(ctx)
	if err != nil {
		return fail(name, fmt.Sprintf("not reachable at %s", backend.Host()),
			"start Ollama. Under WSL, confirm networkingMode=mirrored in .wslconfig, or point "+
				"OLLAMA_HOST at the Windows host explicitly. sicht never starts it for you."), info
	}

	if info.Version == "" {
		return warn(name, fmt.Sprintf("reachable at %s, version unknown", backend.Host()),
			"the server answered its heartbeat but not /api/version"), info
	}

	if CompareVersions(info.Version, MinOllamaVersion) < 0 {
		return warn(name, fmt.Sprintf("version %s at %s", info.Version, backend.Host()),
			fmt.Sprintf("version %s or newer is wanted: older servers do not report logprobs, so "+
				"confidence scoring falls back to the opt-in agreement mode. Several published "+
				"advisories against the server are also only fixed in later releases.",
				MinOllamaVersion)), info
	}

	return ok(name, fmt.Sprintf("version %s at %s", info.Version, backend.Host())), info
}

// OllamaExposure warns when OLLAMA_HOST is not loopback.
//
// Ollama has no authentication on its model-management endpoints - one of the open advisories
// against it - so a non-loopback host means anyone who can reach it can pull, delete or run
// models (docs/ARCHITECTURE.md#doctor).
func OllamaExposure(backend *ollamabackend.Backend) Result {
	const name = "ollama exposure"

	host := backend.Host()

	loopback, err := ollamabackend.HostIsLoopback(host)
	if err != nil {
		return warn(name, fmt.Sprintf("could not interpret %q: %v", host, err),
			"check OLLAMA_HOST")
	}

	if !loopback {
		return warn(name, fmt.Sprintf("%s is not loopback", host),
			"Ollama has no authentication on its model-management endpoints, so anyone who can "+
				"reach this address can pull, delete or run models. Bind it to 127.0.0.1 unless "+
				"you intend otherwise.")
	}

	return ok(name, fmt.Sprintf("%s is loopback; nothing leaves this machine", host))
}

// Inference reports whether inference is local.
//
// It exists to make the guarantee in docs/ARCHITECTURE.md#sicht-scope and docs/ARCHITECTURE.md#privacy-notes something `doctor` states rather than something
// a reader has to infer from the absence of a provider setting. When opt-in cloud backends arrive
// (TODO.md), this is the check that will say so out loud.
func Inference(info infer.ServerInfo) Result {
	const name = "inference"

	if info.Name == "" {
		return skip(name, "backend unavailable")
	}

	if !info.Local {
		return warn(name, fmt.Sprintf("%s is not local", info.Name),
			"image data and OCR text would leave this machine")
	}

	return ok(name, fmt.Sprintf("%s, on this machine", info.Name))
}

// Model checks that the configured model is present and can see images.
//
// Absent is a warn with the command that fixes it, since `run` pulls it automatically anyway. No
// vision capability is a fail: a text-only model cannot classify a screenshot, and discovering
// that from nonsense output would be much worse than discovering it here.
func Model(ctx context.Context, backend *ollamabackend.Backend, name string) Result {
	const check = "model"

	info, err := backend.ModelInfo(ctx, name)
	if err != nil {
		// Only the first line of the underlying error: the backend's errors carry their own
		// "hint:" continuation line, and embedding one inside a hint produced a doubled
		// hint across two lines in a report whose layout assumes one line per field.
		return warn(check, fmt.Sprintf("%s is not pulled (%s)", name, firstLine(err.Error())),
			"run `sicht model pull`, or let `sicht run` fetch it on first use")
	}

	if len(info.Capabilities) > 0 && !info.HasCapability(ollamabackend.CapabilityVision) {
		return fail(check, fmt.Sprintf("%s does not report the vision capability", name),
			"sicht classifies images, so a text-only model cannot work here. Pick a vision model "+
				"such as qwen3-vl.")
	}

	// A warn rather than a fail, because a thinking model does work - on most files. Measured
	// on the 2017 benchmark (docs/benchmarks/sicht-kind-2026-10-13.md): qwen3-vl:8b and qwen3-vl:4b,
	// both thinking variants by default, returned no answer at all for 22 and 25 of 87 files,
	// having reasoned until the 4096-token context was full, at about 24 seconds an image.
	// The -instruct variants of the same models answered 86 of 87 at under a second each.
	// `think: false` does not help: Ollama 0.40.3 still runs qwen3-vl's reasoning phase.
	if info.HasCapability(ollamabackend.CapabilityThinking) {
		return warn(check, name+" is a thinking model, which can reason until no room is left for the answer",
			"use its -instruct variant, e.g. qwen3-vl:8b-instruct, as model.name in sicht.yaml")
	}

	detail := name
	if info.Digest != "" {
		detail += " (" + shortDigest(info.Digest) + ")"
	}

	return ok(check, detail)
}

// VRAM warns when a loaded model does not fit in VRAM.
//
// SizeVRAM below Size means part of the model is on the CPU, which is correct but slow enough to
// change what is practical over an archive of several thousand images (docs/ARCHITECTURE.md#doctor).
func VRAM(ctx context.Context, backend *ollamabackend.Backend, name string) Result {
	const check = "vram"

	info, err := backend.ModelInfo(ctx, name)
	if err != nil {
		return skip(check, fmt.Sprintf("could not ask about %s", name))
	}

	// Nothing to report until the weights are loaded; a cold model has no footprint. Keyed on
	// Loaded rather than on the sizes, because a cold model reports its on-disk size with a
	// VRAM footprint of zero - which is indistinguishable from a model running wholly on the
	// CPU, and reading it as the latter made this check warn about a healthy GPU setup.
	if !info.Loaded || info.Size == 0 {
		// One line: the report's layout gives each check a single detail column, and an
		// embedded newline breaks the alignment for every row after it.
		return skip(check, fmt.Sprintf("%s is not loaded; it loads on first use", name))
	}

	if info.SizeVRAM == 0 {
		return warn(check, fmt.Sprintf("%s is loaded entirely on the CPU", name),
			"inference will be slow. Check that the GPU is visible to Ollama.")
	}

	if info.SizeVRAM < info.Size {
		return warn(check,
			fmt.Sprintf("%s: %s of %s in VRAM", name, bytes(info.SizeVRAM), bytes(info.Size)),
			"part of the model is offloaded to the CPU, which is much slower. A smaller model or "+
				"a lower quantisation would fit.")
	}

	return ok(check, fmt.Sprintf("%s fits in VRAM (%s)", name, bytes(info.Size)))
}

// firstLine returns the first line of s, trimmed. Used to flatten a multi-line wrapped error into
// something that fits one column of the report.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}

	return strings.TrimSpace(s)
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}

	return d
}

// bytes renders a byte count in the largest unit that keeps it readable.
func bytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	value := float64(n)

	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit

		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}

	return fmt.Sprintf("%.1f PiB", value)
}

// Workspace checks that the workspace is usable: writable, with a database that opens and
// migrations that are current (docs/ARCHITECTURE.md#doctor).
func Workspace(ctx context.Context, w *core.Workspace, opener DatabaseOpener) Result {
	const name = "workspace"

	if err := writable(w.Dir()); err != nil {
		return fail(name, fmt.Sprintf("%s is not writable", w.Dir()),
			fmt.Sprintf("check permissions (%v)", err))
	}

	db, err := opener(ctx, w.DatabasePath())
	if err != nil {
		// The reason belongs in the detail, next to the other checks' details; the hint is
		// for what to do about it. Swapping the two makes a report where the only useful
		// line is labelled as advice.
		return fail(name, fmt.Sprintf("%s could not be opened: %v", w.DatabasePath(), err),
			"if the file is corrupt, move it aside and re-run `sicht init`; scan results would "+
				"have to be rebuilt but no original file is touched")
	}
	defer func() { _ = db.Close() }()

	pending, err := db.PendingMigrations(ctx)
	if err != nil {
		return fail(name, "the schema version could not be read", fmt.Sprintf("%v", err))
	}

	// Opening the database applies migrations, so anything still pending here means a
	// migration failed to apply rather than simply not having run.
	if len(pending) > 0 {
		return fail(name, fmt.Sprintf("%d migration(s) did not apply: %v", len(pending), pending),
			"this should not happen after a successful open; the database may be corrupt")
	}

	return ok(name, fmt.Sprintf("%s, schema current", w.Dir()))
}

// ModelHeadroomFactor is how much memory a model wants relative to its own size.
//
// A model is not merely its weights. Ollama streams the file through host memory while offloading
// it, the page cache retains it, and the KV cache is allocated on top. Measured against a 5.8 GB
// model on a 16 GiB WSL guest, those together were enough to exhaust the guest - so the useful
// question is not "does it fit" but "does it fit with room to work in", and twice its size is the
// threshold below which it stops being comfortable.
//
// Checked against the 2017 benchmark (docs/benchmarks/sicht-kind-2026-10-13.md): for the 5.7 GiB
// qwen3-vl:8b-instruct the guest's used memory rose by about 8.3 GiB, and llama-server's resident
// set peaked between 8.7 and 9.6 GiB. So 2x - 11.4 GiB - is conservative by roughly two GiB, which
// is the right side to err on for a check whose failure mode is the whole VM dying. Not retuned
// from one run on a machine with other work in flight.
//
// The figure is reported as an estimate on purpose. It was once quoted as if measured, as "the 8b
// wants 11.4 GiB", and that was enough to rule out a model that in fact ran comfortably.
const ModelHeadroomFactor = 2.0

// MemoryHeadroom checks that the host has room for the configured model.
//
// This check exists because of a specific failure: on a WSL guest capped at 16 GiB by
// `.wslconfig`, loading a 5.8 GB model while other work was in flight exhausted the guest and
// killed the VM outright. Nothing in the pipeline reported anything, because the process that
// would have reported it was the one that died.
//
// A warning rather than a failure: the figures are an estimate, the machine's other load is not
// knowable from here, and refusing to run on a tight host would be presumptuous. But a warning
// before a twenty-minute run is worth far more than an out-of-memory kill during it.
func MemoryHeadroom(modelName string, modelBytes int64) Result {
	const name = "memory"

	mem, found := HostMemory()
	if !found {
		return skip(name, "could not read the host's memory")
	}

	detail := fmt.Sprintf("%s total, %s available", bytes(mem.Total), bytes(mem.Available))

	if mem.SwapTotal > 0 {
		detail += fmt.Sprintf(", %s swap", bytes(mem.SwapTotal))
	}

	// Under WSL the total is a ceiling someone configured rather than the machine's RAM, and
	// the distinction matters precisely when the number looks surprisingly small.
	if UnderWSL() {
		detail += " (WSL ceiling)"
	}

	if modelBytes <= 0 {
		return ok(name, detail)
	}

	wanted := int64(float64(modelBytes) * ModelHeadroomFactor)

	detail += fmt.Sprintf("; %s is estimated to need about %s (twice its weights)", modelName, bytes(wanted))

	if mem.Available < wanted {
		return warn(name, detail,
			fmt.Sprintf("a model is not just its weights: it is streamed through host memory, "+
				"retained in the page cache, and given a KV cache on top. With %s available "+
				"against %s of weights, a load can exhaust the host - which under WSL kills "+
				"the whole guest. Close other work, lower model.options.num_ctx, set "+
				"OLLAMA_MAX_LOADED_MODELS=1, or raise memory= in .wslconfig.",
				bytes(mem.Available), bytes(modelBytes)))
	}

	return ok(name, detail)
}
