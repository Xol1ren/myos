package netcfg

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchPlainGET(t *testing.T) {
	expectedBody := "Hello, pure-Go MYOS HTTP client!"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "myos-fetch/0.3.0" {
			t.Errorf("unexpected user agent: %s", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(expectedBody)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expectedBody))
	}))
	defer ts.Close()

	var buf bytes.Buffer
	progressCalls := 0
	res, err := Fetch(context.Background(), ts.URL, FetchOptions{
		Timeout: 5 * time.Second,
		Out:     &buf,
		Progress: func(downloaded, total int64, elapsed time.Duration, done bool) {
			progressCalls++
		},
	})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if res.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", res.StatusCode)
	}
	if buf.String() != expectedBody {
		t.Errorf("expected body %q, got %q", expectedBody, buf.String())
	}
	if res.BytesRead != int64(len(expectedBody)) {
		t.Errorf("expected %d bytes, got %d", len(expectedBody), res.BytesRead)
	}
	if progressCalls == 0 {
		t.Errorf("expected progress callbacks, got 0")
	}
}

func TestFetchChunked(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("chunk1-"))
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("chunk2-"))
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("chunk3"))
		flusher.Flush()
	}))
	defer ts.Close()

	var buf bytes.Buffer
	res, err := Fetch(context.Background(), ts.URL, FetchOptions{
		Timeout: 5 * time.Second,
		Out:     &buf,
	})
	if err != nil {
		t.Fatalf("Fetch chunked failed: %v", err)
	}

	want := "chunk1-chunk2-chunk3"
	if buf.String() != want {
		t.Errorf("expected %q, got %q", want, buf.String())
	}
	if res.BytesRead != int64(len(want)) {
		t.Errorf("expected %d bytes, got %d", len(want), res.BytesRead)
	}
}

func TestFetchRedirect(t *testing.T) {
	finalBody := "Destination reached!"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		if r.URL.Path == "/final" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(finalBody))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	var buf bytes.Buffer
	res, err := Fetch(context.Background(), ts.URL+"/redirect", FetchOptions{
		Timeout: 5 * time.Second,
		Out:     &buf,
	})
	if err != nil {
		t.Fatalf("Fetch redirect failed: %v", err)
	}

	if res.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", res.StatusCode)
	}
	if buf.String() != finalBody {
		t.Errorf("expected body %q, got %q", finalBody, buf.String())
	}
}

func TestFetchToFile(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("File content test"))
	}))
	defer ts.Close()

	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "downloaded.txt")

	res, err := Fetch(context.Background(), ts.URL, FetchOptions{
		Timeout: 5 * time.Second,
		OutPath: outPath,
	})
	if err != nil {
		t.Fatalf("Fetch to file failed: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(data) != "File content test" {
		t.Errorf("unexpected file content: %q", string(data))
	}
	if res.BytesRead != int64(len("File content test")) {
		t.Errorf("unexpected bytes read: %d", res.BytesRead)
	}
}

func TestFetchInvalidHost(t *testing.T) {
	_, err := Fetch(context.Background(), "http://127.0.0.1:59999/nonexistent", FetchOptions{
		Timeout: 100 * time.Millisecond,
	})
	if err == nil {
		t.Error("expected error for unreachable port, got nil")
	}
}
