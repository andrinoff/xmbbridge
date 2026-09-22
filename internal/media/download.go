// Package media fetches attachments from the origin platform and re-encodes
// them so they fit the destination platform's limits.
package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
)

// DefaultFetchLimit caps how much is read from a single attachment. It exists
// to stop a hostile or misbehaving origin from exhausting memory.
const DefaultFetchLimit = 256 << 20

// Fetcher downloads attachments. Optional headers are applied to every
// request, which is how media behind an authenticated API is retrieved.
type Fetcher struct {
	client  *http.Client
	headers map[string]string
	limit   int64
}

// NewFetcher returns a fetcher using the supplied HTTP client. A nil client
// uses a default with a generous timeout.
func NewFetcher(client *http.Client) *Fetcher {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Fetcher{
		client:  client,
		headers: map[string]string{},
		limit:   DefaultFetchLimit,
	}
}

// WithHeader adds a header sent with every subsequent request.
func (f *Fetcher) WithHeader(key, value string) *Fetcher {
	if value != "" {
		f.headers[key] = value
	}
	return f
}

// SetLimit overrides the maximum number of bytes read per attachment.
func (f *Fetcher) SetLimit(n int64) *Fetcher {
	if n > 0 {
		f.limit = n
	}
	return f
}

// Fetch downloads the attachment described by m and returns its bytes together
// with a best-effort MIME type.
func (f *Fetcher) Fetch(ctx context.Context, m model.Media) ([]byte, string, error) {
	if m.URL == "" {
		return nil, "", fmt.Errorf("media has no URL to fetch")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build media request: %w", err)
	}
	for key, value := range f.headers {
		req.Header.Set(key, value)
	}
	if m.MimeType != "" {
		// Some origins advertise a MIME type we should not blindly trust, but
		// sending it helps origins that negotiate.
		req.Header.Set("Accept", m.MimeType+",*/*")
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch media %s: %w", m.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fetch media %s: unexpected status %s", m.URL, resp.Status)
	}

	limited := io.LimitReader(resp.Body, f.limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("read media %s: %w", m.URL, err)
	}
	if int64(len(data)) > f.limit {
		return nil, "", fmt.Errorf("media %s exceeds the %d byte fetch limit", m.URL, f.limit)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("media %s is empty", m.URL)
	}

	mimeType := m.MimeType
	if mimeType == "" {
		mimeType = resp.Header.Get("Content-Type")
	}
	return data, mimeType, nil
}
