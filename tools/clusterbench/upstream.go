package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"gpt-load/internal/testutil/fakeupstream"
)

// newUpstreamHandler serves the OpenAI success fixture for every chat
// completion after delay. Unlike fakeupstream.Server it never runs out of
// scripted steps and records nothing, so it can sustain a load test.
func newUpstreamHandler(delay time.Duration) (http.Handler, error) {
	completion, err := fakeupstream.Fixture("openai", "success.json")
	if err != nil {
		return nil, err
	}
	modelList, err := fakeupstream.Fixture("openai", "models.json")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(completion)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(modelList)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Unmatched paths usually mean a channel base_url mismatch; log them so
		// the gateway's "upstream 404" can be traced to the exact request.
		fmt.Fprintf(os.Stderr, "fake upstream: no route for %s %s\n", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	return mux, nil
}

func runUpstream(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("upstream", flag.ContinueOnError)
	listen := flags.String("listen", ":8080", "listen address")
	delay := flags.Duration("delay", 50*time.Millisecond, "fixed latency before each chat completion")
	if err := flags.Parse(args); err != nil {
		return err
	}
	handler, err := newUpstreamHandler(*delay)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Printf("fake upstream listening on %s with %s delay\n", *listen, *delay)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
