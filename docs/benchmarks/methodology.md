# Benchmark methodology

How every benchmark in `docs/benchmarks/` is planned, run, measured and recorded. A record that misses any of these is not a benchmark result, and is not quoted as one.

## 1. Kinds of benchmark

| Kind | Question it answers | Tool |
|---|---|---|
| **Simulation (SIM)** | Does a rule (matching, placement, relocation) serve more orders within the promise on real roads? | `cmd/sim` on OSRM with the committed road data |
| **Performance (PERF)** | How fast is a hot path, and how much memory does it use, at pilot, zone and city size? | Go `testing.B`, `-benchmem`, summarised by benchstat |
| **Load (LOAD)** | How many requests a second an API serves within its latency target | Not run yet: planned before the pilot, against staging |

## 2. Before the run
1. **An ID:** SIM-nnn or PERF-nnn, and the GitHub Issue it belongs to.
2. **The question, and the acceptance bar, written before the run.** For example, w4 step 5: "more orders served within the budget than today's rule, at the same or fewer moves". The bar is not changed after the numbers are seen. If it has to change, the record says so and why.
3. **The comparison:** which rules or versions are compared, and what is held the same: orders, seeds, settings and road data.

## 3. Environment, recorded with every run
The run scripts write `environment.txt` beside the raw output. They refuse to run on uncommitted code, so the recorded commit is exactly the code that ran. It holds:
- **the commit SHA**, plus the start and finish time in UTC
- **Go version, OS, CPU model and threads, memory**
- **for SIM, the road data:**
  - the OSRM image and its digest
  - the routing profile and its SHA-256
  - the OpenStreetMap extract, its SHA-256 and timestamp (`tools/osrm/data/*/build.json`)
- **every setting that differs from the defaults**, and the seeds

PERF runs need an otherwise idle machine. A simulation or a build running on the same CPU makes PERF numbers wrong, so PERF is never run alongside SIM.

## 4. Running
- **SIM:**
  - Seeds 1 to 20 are the standard; fewer seeds are a smoke test, not a result.
  - Every rule in a comparison runs on the same orders: the same seed means the same arrivals and gates.
  - Anything fitted, such as a placement plan, is fitted on data drawn with its own seed. It is never fitted on the orders it is judged on.
- **PERF:**
  - `-count 10`, with `-benchmem`.
  - Sizes: pilot, zone and city.
  - Inputs are generated from a fixed seed, so every run measures the same work.
- **The scripts:**

| Kind | Script | Task |
|---|---|---|
| SIM, relocation | `tools/bench/relocation.sh <out>` | `task bench:relocation` |
| PERF | `tools/bench/go.sh <out>` | `task bench` |

## 5. Statistics
- **Shares** (in time per order, served): the count and the total, with a **95% Wilson interval**.
- **Rule comparisons:**
  - Paired by seed: the average difference, plus how many seeds were better, worse and equal.
  - The **exact two-sided sign test p-value** (`sim.SignTest`), with ties left out.
  - A difference is called real only when p < 0.05 **and** the direction makes sense. Overlapping pooled intervals alone neither prove nor rule out a difference: the paired test decides.
- **PERF:** benchstat's median with its 95% confidence interval, per benchmark and size. A change between two commits is reported only when benchstat marks it significant.
- **Baselines are named.** "Plan vs today" and "plan vs none" are different numbers; a record never mixes them.

## 6. Bounds, when the model simplifies
When the simulator cannot model something exactly, run both bounds and report both. For example, whether a rider who is moving can take orders: `-move-takes-orders` is the optimistic bound, and without it the pessimistic bound. A conclusion holds only if it holds under both bounds. If it holds only under one, the record says which.

## 7. The record
Each benchmark has one page in `docs/benchmarks/` with:
1. **ID, Issue, date, commit, and the status:** current or superseded, and by what.
2. **The question and the acceptance bar.**
3. **Method:** what was compared and what was held the same, and the exact commands.
4. **Environment:** a link to `environment.txt`.
5. **Results:** tables with intervals and p-values. **The raw output is committed** in `results/<date>_<sha>/`, and nothing is quoted that is not in it.
6. **What it shows:** only what the numbers support, and the bar met or not.
7. **Limits:** every way the model differs from production.
8. **Independent review:** who reviewed it, what they found, and what was fixed.

`README.md` lists every record with its headline result.

## 8. Re-running and superseding
A result is reproduced by checking out its commit and running its command. A newer run of the same benchmark supersedes the older one, and the older record stays, marked superseded with a link. Results are never edited after the run. A correction is a new run, or a note that says what was wrong.
