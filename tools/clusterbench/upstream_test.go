package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/testutil/fakeupstream"
)

func TestUpstreamServesFixtureRepeatedlyAfterDelay(t *testing.T) {
	const delay = 20 * time.Millisecond
	handler, err := newUpstreamHandler(delay)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	want, err := fakeupstream.Fixture("openai", "success.json")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for range 100 {
		wg.Go(func() {
			started := time.Now()
			response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", nil)
			if err != nil {
				errs <- err.Error()
				return
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
				errs <- "unexpected response " + response.Status
			}
			if elapsed := time.Since(started); elapsed < delay {
				errs <- "response arrived before the configured delay: " + elapsed.String()
			}
		})
	}
	wg.Wait()
	close(errs)
	for message := range errs {
		t.Error(message)
	}
}

func TestUpstreamRejectsUnknownPaths(t *testing.T) {
	handler, err := newUpstreamHandler(0)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", recorder.Code)
	}
}
