// Package bench holds the M2 model benchmark (docs/TODO.md, open item 8).
//
// It lives in the repository rather than in a scratch directory because docs/TODO.md asks for the model
// choice to be measured and the finding recorded, and a number without a reproducible way to
// re-derive it is a number nobody can check. The benchmark itself is behind the `integration`
// build tag: it needs a running Ollama, several gigabytes of pulled models, and a scanned archive
// to draw ground truth from.
//
// The ground truth costs nothing to obtain, which is the nice part. docs/TODO.md asks for "~100
// hand-labelled screenshots"; the deterministic rules already label far more than that from
// device metadata - the iOS screenshot marker and camera EXIF - and a metadata label is better
// evidence than a human's recollection.
package bench
