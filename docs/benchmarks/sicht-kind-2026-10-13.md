# sicht: model choice for the `kind` stage

Measured 2026-10-13. Answers which local vision model `sicht`
should default to — and found on the way that the `kind` stage itself was the larger problem.

## Outcome

- **Default model: `qwen3-vl:8b-instruct`.** 92% correct over 85 labelled files, 0.83 s an image,
  7.8 GiB of VRAM.
- **The plain `qwen3-vl` tags must not be used.** They are thinking variants, and they never
  answered about a quarter of the files.
- **`gemma3:4b` is out**: its confidence does not drop when it is wrong.
- **`kind`'s catch-all value is renamed from `other` to `graphic`.** Under the old name no instruct
  model ever chose it, and every model scored **25%**.

## Setup

| Item     | Value                                                                                                      |
| -------- | ---------------------------------------------------------------------------------------------------------- |
| GPU      | NVIDIA RTX 4080, 16 GiB; 2.9 GiB already held by the Windows desktop                                       |
| Host     | WSL2 guest, 15.6 GiB with 4 GiB of swap; other agents stopped; 5.2 GiB used at the start                   |
| Ollama   | 0.40.3, systemd service, `OLLAMA_MAX_LOADED_MODELS=1`, `OLLAMA_NUM_PARALLEL=1`, `OLLAMA_FLASH_ATTENTION=1` |
| Request  | `temperature: 0`, `seed: 42`, `num_ctx: 4096`, long edge 1280 px, `tall_strategy: crop`                    |
| Pipeline | `sicht run kind`, 2 workers, `review_below: 0.75` — the real stage runner, not a harness                   |
| Corpus   | One year of the reference archive, copied off its `drvfs` mount onto ext4                                  |

The corpus is 87 image files with 87 Takeout sidecars paired; one file is a real HEIC, which nothing
in a pure-Go build decodes, so 86 can reach a model. The scan of the copy took 546 ms.

### Ground truth

| Source                         | Files  | photo  | screenshot | scan_document | other  |
| ------------------------------ | ------ | ------ | ---------- | ------------- | ------ |
| The default rules              | 13     | 12     | 1          | 0             | 0      |
| The owner's labels, made blind | 72     | 9      | 0          | 0             | 63     |
| **Total**                      | **85** | **21** | **1**      | **0**         | **63** |

The labels were made on a local page that showed the images and no model answers, so that no
answer could anchor a choice; the 16 files the two leading models disagreed on came first. One file
on the page was left unlabelled. The owner made the labels rather than an agent because looking at
the images would have sent them off the machine.

For the runs, every file was sent to every model: each model got its own copy of the workspace with
the rules' decisions and the labels removed. That is what lets the rule-decided files serve as
ground truth too, rather than being skipped.

Each model ran alone and was unloaded before the next, under a watchdog that would have killed the
run below 800 MiB of free host memory. The lowest it saw was 1.6 GiB, during the thinking 8b.

## Round 1: does it answer, and how fast

| Model                  | Answered | Failed | Per image | VRAM over idle |
| ---------------------- | -------- | ------ | --------- | -------------- |
| `qwen3-vl:8b-instruct` | 86       | 0      | 0.83 s    | 7.8 GiB        |
| `qwen3-vl:4b-instruct` | 86       | 0      | 0.57 s    | 5.4 GiB        |
| `qwen3-vl:8b`          | **64**   | **22** | 24.4 s    | 7.6 GiB        |
| `qwen3-vl:4b`          | **61**   | **25** | 24.3 s    | 5.2 GiB        |
| `gemma3:4b`            | 86       | 0      | 0.82 s    | 4.4 GiB        |

All five fit on the GPU. Before this run the 8b had been ruled out as needing 11.4 GiB; that figure
was `sicht doctor`'s estimate — twice the model's weights — quoted as though it had been measured.

### Why the thinking variants fail

Ollama's plain `qwen3-vl:8b` tag is its `qwen3-vl:8b-thinking`; the two share a config digest. A
thinking model reasons before it answers, and the reasoning shares the context window with the
image and the answer. On an ambiguous image the model kept deliberating until the window was full.
Reproducing one failure directly against the API: `done_reason: length`, 13.6 KB of reasoning,
**zero bytes of answer**.

Sending `think: false` does not help. Ollama 0.40.3 still runs qwen3-vl's reasoning: the output
arrives in `message.thinking`, opening with a literal `<think>` tag, and fails the same way. The
`-instruct` tags report no `thinking` capability at all, so `sicht doctor` now warns about any model
that does.

## Round 2: is it right

Against the 85 labelled files, with the `kind` stage as it shipped:

| Model                  | Correct       | Confident (≥ 0.75) | …of which right |
| ---------------------- | ------------- | ------------------ | --------------- |
| `qwen3-vl:8b-instruct` | 21 / 85 (25%) | 58                 | 21 (36%)        |
| `qwen3-vl:4b-instruct` | 21 / 85 (25%) | 56                 | 21 (38%)        |
| `qwen3-vl:8b`          | 22 / 64 (34%) | 61                 | 22 (36%)        |
| `qwen3-vl:4b`          | 24 / 61 (39%) | 58                 | 24 (41%)        |
| `gemma3:4b`            | 21 / 85 (25%) | 81                 | 20 (25%)        |

Every model got all 21 photographs right and missed the one rule-decided screenshot, which every
model called a photo at confidence 1.0, or 0.9999 for gemma. Almost everything else is the same mistake: the 63 files the
owner labelled `other` — saved wallpapers, logos, album covers, textures — were filed as photos or
screenshots. Between them the two instruct models answered `other` **zero** times; the thinking
variants managed 1 and 3.

That is not a confidence problem the review queue can catch. For the 8b-instruct, 37 of its 58
confident answers are wrong, and those would be filed without anybody being asked.

## Round 3: fixing the stage

The stage offers four values, and the shipped prompt describes two of them. Three variants were
measured, on the instruct models and gemma, against the same 85 files:

| `kind` stage                                      | 8b-instruct | 4b-instruct | gemma3:4b |
| ------------------------------------------------- | ----------- | ----------- | --------- |
| As shipped: value `other`, never described        | 25%         | 25%         | 25%       |
| `other` and `scan_document` described, briefly    | 25%         | 25%         | **18%**   |
| A fuller description, value still called `other`  | 58%         | —           | —         |
| The same description, value renamed **`graphic`** | **92%**     | **87%**     | —         |

The last two rows are the ablation that matters. Describing the case recovers about half of the
gap; renaming the value recovers the rest. A model reaches for a value that names a thing and
avoids one that names an absence. Together they moved the 8b's confident answers from 36% to 93%
right; the description alone reached 68%.

The renamed variant also separates the two instruct models for the first time: the 8b keeps all 21
photographs, while the 4b files 5 of them as graphics.

**gemma3:4b is out on calibration.** Its 81 confident answers in round 2 look like the best row, and
the brief description then took it from 12 to 6 of the 13 rule-decided files, calling camera
photographs screenshots at confidence 1.0 — while sending nothing at all to review. A confidence
that stays at 1.0 while the answer is wrong cannot gate a review queue, which is the job it is
there to do.

### A bug the stage was hiding

`"other"` is also the escape-hatch value sicht adds to a closed **taxonomy** field, where it is
paired with a free-text companion. The runner treated it as the escape hatch on _every_ field, so on
`kind` — an inline enum with no companion — each `other` answer was taken for an invented value and
sent to review regardless of its confidence. Nobody saw it because the models almost never said
`other`. It is fixed independently of the rename: the escape hatch now applies only where the
schema created one. The stored values in this report were unaffected; review counts for the
variants that kept `other` were inflated by it.

## What this does not show

- **The 92% is in-sample.** The `graphic` description was written after seeing what this year's
  labels meant, and one of its sentences — a landscape saved from the web is a graphic — is aimed at
  one file. It needs checking on a year it was not tuned on before the figure is quoted as the
  model's accuracy.
- **Screenshot accuracy is not measured at all.** The ground truth holds one screenshot, so nothing
  here says how well any model recognises one. Neither is `scan_document`, which has no example.
- **One year is a small and unusual sample.** This one is mostly downloaded images, which is
  exactly what exposed the `other` problem; a year of camera photographs would have hidden it.
- **Host memory was tight for every model.** The guest's used memory peaked at 13–14 GiB of 15.6,
  partly the model and partly everything else running. `sicht doctor`'s memory check stays
  conservative for that reason.

## Reproducing it

1. Copy one year of a scanned archive to a local filesystem, then `sicht init` and `sicht scan`
   there. Keep it off `drvfs`, or the run measures 9P round trips instead of the model.
2. Run `sicht rules`, and keep the rule decisions as part of the ground truth.
3. Label the rest by hand, blind, and record the labels with `sicht review set`.
4. For each model: copy the workspace, delete the rule and human rows from the copy's `results`, set
   `model.name`, and run `sicht run kind` — alone, unloading the previous model first.
5. Compare each copy's `model:` rows with the ground truth.
