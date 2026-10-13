package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/adnoctem/pertry/internal/sicht/core"
)

// defaultPrompts are the prompt templates the default stages refer to.
//
// They are starting points, not finished prompts. Each one is deliberately short and says what to
// look at rather than what to conclude, because the schema already constrains the answer: a prompt
// that also enumerates the options tends to make the model argue with the enum instead of using it.
//
// The exception is a value the model would otherwise never reach. kind.md describes `graphic`
// because, measured on the 2017 benchmark (docs/benchmarks/sicht-kind-2026-10-13.md), the shipped prompt left the non-photo,
// non-screenshot case undescribed and the models filed every saved wallpaper, logo and cover as a
// photo or a screenshot - 25% accuracy over 85 labelled files. Describing it took the 8b to 58%;
// describing it under a name that says what it is took it to 92%. That figure is in-sample: the
// paragraph was written after seeing the labels, and it is owed a check on another year.
var defaultPrompts = map[string]string{
	"kind.md": `Classify what kind of image this is.

Look at the whole frame before deciding. A screenshot has interface furniture somewhere in it -
a status bar, a navigation bar, a keyboard, a scroll bar, a cursor, text rendered crisply at a
consistent size. A photograph has optical evidence instead: depth of field, uneven lighting,
noise, motion blur, a horizon.

A photograph *of* a screen is a photograph. A screenshot of a photograph is a screenshot.

A graphic is made rather than captured: a wallpaper, a logo, artwork, a poster, cover art, a meme,
a texture or a rendered image. It has neither interface furniture nor a camera's optical evidence,
and a picture saved from the web is a graphic even when it shows a landscape.

Describe the interface you can see before you decide, and describe nothing if there is none.
`,

	"platform.md": `Identify which application or website this screenshot came from.

Read the visible text first and quote the most identifying part of it. A URL in an address bar, an
account handle, a tab title or a watermark is worth more than a layout impression - interfaces
imitate each other, but a domain name does not lie.

If the only evidence is a layout you recognise, say so in the description rather than treating it
as certain, and let your confidence reflect it.

Prefer the specific over the general: the name of the app, not "a social network".
`,
}

// writePrompts writes any missing prompt template and returns the names it wrote, sorted.
//
// Existing files are left alone: a tuned prompt is exactly the kind of work re-running `init`
// must not destroy, and docs/ARCHITECTURE.md#model-stages records a prompt hash with every result precisely because prompts get
// iterated on.
func writePrompts(w *core.Workspace) ([]string, error) {
	var written []string

	for name, body := range defaultPrompts {
		path := filepath.Join(w.PromptsPath(), name)

		switch _, err := os.Stat(path); {
		case err == nil:
			continue
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}

		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}

		written = append(written, name)
	}

	sort.Strings(written)

	return written, nil
}
