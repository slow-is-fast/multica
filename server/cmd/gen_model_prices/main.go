// Command gen_model_prices renders the web UI's price table from the server's.
//
// The two tables used to be maintained separately and drifted in both
// directions (ruel#39, ruel#44). There is now one source —
// internal/metrics/modelPrices — and this command writes it out in the shape
// the frontend imports.
//
// Usage:
//
//	go run ./cmd/gen_model_prices            # write the file
//	go run ./cmd/gen_model_prices -check     # exit non-zero if it is stale
//
// -check is what CI should run: it turns "somebody edited a rate and did not
// regenerate" into a red build instead of a dashboard that disagrees with the
// budget gate.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/metrics"
)

func main() {
	check := flag.Bool("check", false, "fail if the committed file differs from a fresh render, instead of writing it")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen_model_prices:", err)
		os.Exit(1)
	}
	out := filepath.Join(root, metrics.GeneratedFrontendPricingRel())

	rendered, err := metrics.GeneratedFrontendPricing()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen_model_prices:", err)
		os.Exit(1)
	}

	committed, err := os.ReadFile(out)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "gen_model_prices: read", out, err)
		os.Exit(1)
	}
	// The generated file is written with LF. Windows checkouts that normalize
	// to CRLF would otherwise make -check fail on every run for a reason that
	// has nothing to do with the rates.
	if strings.ReplaceAll(string(committed), "\r\n", "\n") == rendered {
		fmt.Println("gen_model_prices: up to date:", out)
		return
	}

	if *check {
		fmt.Fprintln(os.Stderr, "gen_model_prices:", out, "is stale — run `go run ./cmd/gen_model_prices` and commit the result")
		os.Exit(1)
	}
	if err := os.WriteFile(out, []byte(rendered), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen_model_prices: write", out, err)
		os.Exit(1)
	}
	fmt.Println("gen_model_prices: wrote", out)
}

// repoRoot walks up from the working directory to the directory holding
// go.mod, so the command works from anywhere in the tree. Running it from the
// wrong place and silently writing to the wrong path is the one failure mode
// worth guarding: a generated file that lands somewhere the build does not
// read looks exactly like a generator that did nothing.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 10; i++ {
		// The path this command writes to is relative to the REPOSITORY root,
		// not to the Go module root — server/ is its own module, so the first
		// go.mod upward is one level too deep. Anchor on the tree layout
		// instead.
		if st, err := os.Stat(filepath.Join(dir, filepath.Dir(metrics.GeneratedFrontendPricingRel()))); err == nil && st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("could not find go.mod at or above the working directory")
}
