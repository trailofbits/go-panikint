**Project context**
- Fork of the Go toolchain that instruments integer arithmetic to panic on overflow/underflow and can detect integer truncation.
- “Good” changes keep compiler stability and avoid false positives in stdlib/vendor code while catching user-code issues.

**Repo map**
- `src/` Go toolchain source (compiler changes live under `src/cmd/compile/`).
- `tests/` Go-Panikint unit tests (arithmetic/truncation).
- `fuzz_test/` fuzz harness and seeds.
- `doc/` upstream Go documentation.

**Tech stack**
- Go, Go compiler internals, Go assembly, shell scripts.

**Common commands**
```bash
# build toolchain
cd src
./make.bash

# build + run full Go tests
./all.bash

# run Go-Panikint unit tests
cd tests
GOROOT=/path/to/go-panikint /path/to/go-panikint/bin/go test -v .

# single unit test
GOROOT=/path/to/go-panikint /path/to/go-panikint/bin/go test -run TestArithmetic -v .

# fuzz harness
cd fuzz_test
GOROOT=/path/to/go-panikint ../bin/go test -fuzz=FuzzIntegerOverflow -v .
```

**Tooling notes**
- Truncation detection is controlled by compiler flag `-truncationdetect` via `GOFLAGS` when building.
- Use `GOROOT` pointing at the repo root to run the built toolchain.

**Workflow / verification**
- Default branch is `master` (see `codereview.cfg`).
- CI builds twice: default build and with truncation enabled; run both if you touch compiler instrumentation.

**Gotchas**
- Overflow instrumentation skips stdlib/internal/vendor/pkg/mod code via source-location filtering in `src/cmd/compile/internal/ssagen/ssa.go`.
- Suppress known false positives with `overflow_false_positive` / `truncation_false_positive`; inlining can hide markers (use `//go:noinline` if needed).
- Upstream `$GOROOT/test` relies on wrap-around; use `tests/` for Go-Panikint checks and set `GOPANIKINT_DISABLE_OVERFLOW=1` when running `cmd/internal/testdir` via `src/all.bash` if needed.

**References**
- @README.md
- @CONTRIBUTING.md
- @SECURITY.md
