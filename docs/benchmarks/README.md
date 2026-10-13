# Benchmarks

Measurements that decided something, kept with the method that produced them.

Each report says what was asked, what was run, what came back, and what it does **not** show. A
number here is only as good as the conditions it was taken under, so those are written down too —
including the cases where an earlier figure turned out to be wrong.

| Report                                                     | Question                                                  | Outcome                                               |
| ---------------------------------------------------------- | --------------------------------------------------------- | ----------------------------------------------------- |
| [sicht: model choice for `kind`](sicht-kind-2026-10-13.md) | Which local vision model, and does the `kind` stage work? | `qwen3-vl:8b-instruct`, and `other` renamed `graphic` |

## Privacy

The corpora these run against are the owner's own archives, and they hold other people's data.
Reports publish **aggregates only** — counts, rates, timings — never filenames, content hashes or
per-file results. Ground-truth labels are made by the owner, never by an agent looking at the
images, because looking would mean sending them off the machine (see [Privacy Notes](../ARCHITECTURE.md#privacy-notes)).
