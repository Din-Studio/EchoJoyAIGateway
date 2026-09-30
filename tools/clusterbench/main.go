// Command clusterbench drives the cluster-mode acceptance harness defined in
// tools/clusterbench/compose.yml: a fixed-latency fake upstream, deterministic
// correctness checks against the sample topology, and throughput/p99
// measurements against a single-instance baseline.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage: clusterbench <upstream|verify|bench> [flags]

  upstream  serve an OpenAI chat-completions fixture after a fixed delay
  verify    run the deterministic cluster acceptance checks
  bench     measure throughput and p99 against the single-instance baseline`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "upstream":
		err = runUpstream(ctx, os.Args[2:])
	case "verify":
		err = runVerify(ctx, os.Args[2:])
	case "bench":
		err = runBench(ctx, os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clusterbench:", err)
		os.Exit(1)
	}
}
