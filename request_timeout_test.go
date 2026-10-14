package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestDeadlineIncludesBody(t *testing.T) {
	for _, prefix := range []string{"", "{", "data: {"} {
		t.Run(prefix, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, prefix)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
			resp, err := clientDoWithRetryTimeout(context.Background(), srv.Client(), req, 20*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			_, err = io.ReadAll(resp.Body)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("body read error=%v", err)
			}
		})
	}
}

func TestRetryDoesNotDrainHangingBody(t *testing.T) {
	fastRetry(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := clientDoWithRetryTimeout(ctx, srv.Client(), req, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || string(raw) != "done" || calls.Load() != 2 {
		t.Fatalf("raw=%q err=%v calls=%d", raw, err, calls.Load())
	}
}

func TestResponseBodyCloseCancelsRequest(t *testing.T) {
	stopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	resp, err := clientDoWithRetryTimeout(context.Background(), srv.Client(), req, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("body close did not cancel the request")
	}
}
