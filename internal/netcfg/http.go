package netcfg

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// FetchProgressCallback is called periodically during a download.
type FetchProgressCallback func(downloaded, total int64, elapsed time.Duration, done bool)

// FetchOptions configures an HTTP GET fetch.
type FetchOptions struct {
	Timeout   time.Duration
	Out       io.Writer
	OutPath   string
	Progress  FetchProgressCallback
	UserAgent string
}

// FetchResult provides transfer metrics after completion.
type FetchResult struct {
	StatusCode int
	StatusText string
	BytesRead  int64
	Duration   time.Duration
}

// Fetch performs an HTTP/HTTPS GET request using pure Go networking.
func Fetch(ctx context.Context, rawURL string, opts FetchOptions) (*FetchResult, error) {
	return fetchWithRedirects(ctx, rawURL, opts, 0)
}

func fetchWithRedirects(ctx context.Context, rawURL string, opts FetchOptions, redirects int) (*FetchResult, error) {
	if redirects > 5 {
		return nil, errors.New("fetch: too many redirects (>5)")
	}

	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "http://" + rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("fetch: invalid url: %w", err)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	port := u.Port()
	hostOnly := u.Hostname()
	scheme := strings.ToLower(u.Scheme)

	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(hostOnly, port)

	var conn net.Conn
	dialer := &net.Dialer{Timeout: timeout}

	if scheme == "https" {
		tlsConfig := &tls.Config{
			ServerName:         hostOnly,
			InsecureSkipVerify: true,
		}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch: connect to %s: %w", addr, err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	reqPath := u.RequestURI()
	if reqPath == "" {
		reqPath = "/"
	}

	ua := opts.UserAgent
	if ua == "" {
		ua = "myos-fetch/0.3.0"
	}

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nConnection: close\r\nAccept: */*\r\n\r\n", reqPath, u.Host, ua)
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("fetch: send request: %w", err)
	}

	startTime := time.Now()
	reader := bufio.NewReader(conn)

	// Read status line
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("fetch: read status: %w", err)
	}
	statusLine = strings.TrimRight(statusLine, "\r\n")
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("fetch: malformed status line: %q", statusLine)
	}
	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("fetch: invalid status code: %q", parts[1])
	}
	statusText := ""
	if len(parts) > 2 {
		statusText = parts[2]
	}

	// Read headers
	headers := make(map[string]string)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("fetch: read header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}

	// Redirect check (301, 302, 303, 307, 308)
	if statusCode >= 301 && statusCode <= 308 && statusCode != 304 && statusCode != 305 && statusCode != 306 {
		if loc, ok := headers["location"]; ok && loc != "" {
			nextURL, err := u.Parse(loc)
			if err == nil {
				return fetchWithRedirects(ctx, nextURL.String(), opts, redirects+1)
			}
		}
	}

	var dest io.Writer
	var closeDest func() error

	if opts.OutPath != "" {
		f, err := os.Create(opts.OutPath)
		if err != nil {
			return nil, fmt.Errorf("fetch: create output file: %w", err)
		}
		dest = f
		closeDest = f.Close
	} else if opts.Out != nil {
		dest = opts.Out
	} else {
		dest = io.Discard
	}
	if closeDest != nil {
		defer closeDest()
	}

	contentLength := int64(-1)
	if clStr, ok := headers["content-length"]; ok {
		if cl, err := strconv.ParseInt(clStr, 10, 64); err == nil {
			contentLength = cl
		}
	}

	isChunked := strings.EqualFold(headers["transfer-encoding"], "chunked")

	var totalBytes int64
	lastProgress := time.Now()

	reportProgress := func(done bool) {
		if opts.Progress != nil {
			opts.Progress(totalBytes, contentLength, time.Since(startTime), done)
		}
	}

	buf := make([]byte, 16*1024)

	if isChunked {
		// Read chunked encoding
		for {
			_ = conn.SetDeadline(time.Now().Add(timeout))
			line, err := reader.ReadString('\n')
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("fetch: read chunk size: %w", err)
			}
			chunkHex := strings.TrimRight(line, "\r\n")
			if chunkHex == "" {
				continue
			}
			// Hex size can have chunk extensions
			if semi := strings.IndexByte(chunkHex, ';'); semi >= 0 {
				chunkHex = chunkHex[:semi]
			}
			chunkHex = strings.TrimSpace(chunkHex)
			chunkSize, err := strconv.ParseInt(chunkHex, 16, 64)
			if err != nil {
				return nil, fmt.Errorf("fetch: invalid chunk size %q: %w", chunkHex, err)
			}
			if chunkSize == 0 {
				break // end of chunked stream
			}

			// Read exactly chunkSize bytes
			written, err := io.CopyN(dest, reader, chunkSize)
			totalBytes += written
			if err != nil {
				return nil, fmt.Errorf("fetch: read chunk body: %w", err)
			}

			// Read trailing CRLF
			_, _ = reader.ReadString('\n')

			if time.Since(lastProgress) >= 100*time.Millisecond {
				reportProgress(false)
				lastProgress = time.Now()
			}
		}
	} else {
		// Non-chunked body
		for {
			_ = conn.SetDeadline(time.Now().Add(timeout))
			n, err := reader.Read(buf)
			if n > 0 {
				wn, werr := dest.Write(buf[:n])
				totalBytes += int64(wn)
				if werr != nil {
					return nil, fmt.Errorf("fetch: write output: %w", werr)
				}
				if time.Since(lastProgress) >= 100*time.Millisecond {
					reportProgress(false)
					lastProgress = time.Now()
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("fetch: read body: %w", err)
			}
		}
	}

	reportProgress(true)

	return &FetchResult{
		StatusCode: statusCode,
		StatusText: statusText,
		BytesRead:  totalBytes,
		Duration:   time.Since(startTime),
	}, nil
}
