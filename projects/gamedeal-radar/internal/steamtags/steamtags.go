// Package steamtags reads Steam's user tags (the community tags shown on a store page)
// from Steam's public store API. It is used only by cmd/steam-tags. No key is needed.
//
// Two endpoints are used:
//   - IStoreService/GetTagList, once per run, to map tag ids to names.
//   - IStoreBrowseService/GetItems, once per batch of up to 50 app ids, for the tags of each game.
package steamtags

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// TagListURL lists every tag id with its English name.
	TagListURL = "https://api.steampowered.com/IStoreService/GetTagList/v1/"
	// ItemsURL returns the tags of a batch of app ids.
	ItemsURL = "https://api.steampowered.com/IStoreBrowseService/GetItems/v1"

	// UserAgent identifies the job to Steam. Honest name, no browser spoofing.
	UserAgent = "GameDealRadar/1.0 (tags job)"

	// BatchSize is the most app ids sent in one GetItems request.
	BatchSize = 50
	// MaxTags is how many tags are stored per game, most popular first.
	MaxTags = 10

	maxBodyBytes = 5 << 20
	maxRetryWait = 60 * time.Second
)

// ErrBlocked means Steam refused the run: 401, 403, or 429 that did not clear after retries.
// The run must stop calling Steam.
var ErrBlocked = errors.New("steam refused the request")

// Tag is one stored tag. ID is the Steam tag id as a string, Description is its English name.
type Tag struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// ItemTag is one tag of a store item, as Steam returns it.
type ItemTag struct {
	TagID  int `json:"tagid"`
	Weight int `json:"weight"`
}

// StoreItem is one entry of GetItems. Success is 1 when Steam knows the app.
type StoreItem struct {
	AppID   int       `json:"appid"`
	Success int       `json:"success"`
	Tags    []ItemTag `json:"tags"`
}

// ParseTagList reads a GetTagList body into a tag id to name map. An empty list is an error,
// since no tag could then be named.
func ParseTagList(body []byte) (map[int]string, error) {
	var env struct {
		Response struct {
			Tags []struct {
				TagID int    `json:"tagid"`
				Name  string `json:"name"`
			} `json:"tags"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parsing tag list: %w", err)
	}
	names := make(map[int]string, len(env.Response.Tags))
	for _, t := range env.Response.Tags {
		if name := strings.TrimSpace(t.Name); t.TagID > 0 && name != "" {
			names[t.TagID] = name
		}
	}
	if len(names) == 0 {
		return nil, errors.New("tag list is empty")
	}
	return names, nil
}

// ParseItems reads a GetItems body.
func ParseItems(body []byte) ([]StoreItem, error) {
	var env struct {
		Response struct {
			StoreItems []StoreItem `json:"store_items"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parsing store items: %w", err)
	}
	return env.Response.StoreItems, nil
}

// MapTags turns one game's Steam tags into the stored list. Tags are ordered by weight,
// most popular first. Tags with no known name, and repeated ids, are dropped. The list
// is capped at MaxTags. It is never nil.
func MapTags(tags []ItemTag, names map[int]string) []Tag {
	sorted := make([]ItemTag, len(tags))
	copy(sorted, tags)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Weight > sorted[j].Weight })

	out := make([]Tag, 0, MaxTags)
	seen := make(map[int]bool, len(sorted))
	for _, t := range sorted {
		if len(out) == MaxTags {
			break
		}
		name, ok := names[t.TagID]
		if !ok || seen[t.TagID] {
			continue
		}
		seen[t.TagID] = true
		out = append(out, Tag{ID: strconv.Itoa(t.TagID), Description: name})
	}
	return out
}

// ItemsInput builds the input_json value for a GetItems request.
func ItemsInput(appIDs []int) (string, error) {
	type idRef struct {
		AppID int `json:"appid"`
	}
	req := struct {
		IDs     []idRef `json:"ids"`
		Context struct {
			Language    string `json:"language"`
			CountryCode string `json:"country_code"`
		} `json:"context"`
		DataRequest struct {
			IncludeTagCount int `json:"include_tag_count"`
		} `json:"data_request"`
	}{}
	req.IDs = make([]idRef, 0, len(appIDs))
	for _, id := range appIDs {
		req.IDs = append(req.IDs, idRef{AppID: id})
	}
	req.Context.Language = "english"
	req.Context.CountryCode = "US"
	req.DataRequest.IncludeTagCount = MaxTags

	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encoding items request: %w", err)
	}
	return string(raw), nil
}

// ItemSource returns the store items for a batch of app ids.
type ItemSource interface {
	GetItems(ctx context.Context, appIDs []int) ([]StoreItem, error)
}

// Result is the outcome of Collect.
type Result struct {
	// Tags holds the mapped tags of every app whose item carried at least one known tag.
	Tags map[int][]Tag
	// Failed lists app ids whose batch failed after retries. They are not handled.
	Failed []int
}

// Collect fetches the tags of appIDs in batches. An item that is not successful or has no
// tags is skipped, which is not an error. A batch that fails is listed in Failed and the
// run goes on. ErrBlocked, and a cancelled context, stop the run and are returned.
func Collect(ctx context.Context, src ItemSource, names map[int]string, appIDs []int) (Result, error) {
	res := Result{Tags: map[int][]Tag{}}
	for start := 0; start < len(appIDs); start += BatchSize {
		end := min(start+BatchSize, len(appIDs))
		batch := appIDs[start:end]

		items, err := src.GetItems(ctx, batch)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return res, ctxErr
			}
			if errors.Is(err, ErrBlocked) {
				return res, err
			}
			res.Failed = append(res.Failed, batch...)
			continue
		}
		for _, item := range items {
			if item.Success != 1 || len(item.Tags) == 0 {
				continue
			}
			if tags := MapTags(item.Tags, names); len(tags) > 0 {
				res.Tags[item.AppID] = tags
			}
		}
	}
	return res, nil
}

// Client calls the two Steam endpoints. Requests are paced by Gap. A 429 or 5xx is retried
// with a doubling wait, up to MaxAttempts tries. The fields can be changed in tests.
type Client struct {
	HTTP        *http.Client
	TagListURL  string
	ItemsURL    string
	UserAgent   string
	Gap         time.Duration
	Backoff     time.Duration
	MaxAttempts int

	mu   sync.Mutex
	last time.Time
}

// NewClient returns a Client for the live Steam endpoints: one request a second, four tries.
func NewClient() *Client {
	return &Client{
		HTTP:        &http.Client{Timeout: 20 * time.Second},
		TagListURL:  TagListURL,
		ItemsURL:    ItemsURL,
		UserAgent:   UserAgent,
		Gap:         time.Second,
		Backoff:     5 * time.Second,
		MaxAttempts: 4,
	}
}

// TagNames fetches the tag id to name map.
func (c *Client) TagNames(ctx context.Context) (map[int]string, error) {
	body, err := c.get(ctx, c.TagListURL+"?"+url.Values{"language": {"english"}}.Encode())
	if err != nil {
		return nil, fmt.Errorf("fetching tag list: %w", err)
	}
	return ParseTagList(body)
}

// GetItems fetches the store items, with their tags, for up to BatchSize app ids.
func (c *Client) GetItems(ctx context.Context, appIDs []int) ([]StoreItem, error) {
	input, err := ItemsInput(appIDs)
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, c.ItemsURL+"?"+url.Values{"input_json": {input}}.Encode())
	if err != nil {
		return nil, fmt.Errorf("fetching store items: %w", err)
	}
	return ParseItems(body)
}

// pace waits until Gap has passed since the previous request started, or ctx ends.
func (c *Client) pace(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.last.IsZero() {
		if d := c.Gap - time.Since(c.last); d > 0 {
			if err := sleepCtx(ctx, d); err != nil {
				return err
			}
		}
	}
	c.last = time.Now()
	return nil
}

// get performs a GET with pacing and retries. A 401, 403 or a final 429 wraps ErrBlocked.
// Any other non-2xx status is a permanent error for the request.
func (c *Client) get(ctx context.Context, rawURL string) ([]byte, error) {
	attempts := max(c.MaxAttempts, 1)
	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept", "application/json")

		var retryAfter time.Duration
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("request failed: %w", err)
		} else {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
			_ = resp.Body.Close()
			switch {
			case readErr != nil:
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				lastErr = fmt.Errorf("reading response: %w", readErr)
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return body, nil
			case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
				return nil, fmt.Errorf("HTTP %d: %w", resp.StatusCode, ErrBlocked)
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				if resp.StatusCode == http.StatusTooManyRequests {
					lastErr = fmt.Errorf("HTTP %d: %w", resp.StatusCode, ErrBlocked)
				}
			default:
				return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}

		if attempt >= attempts {
			return nil, lastErr
		}
		wait := c.Backoff << (attempt - 1)
		if retryAfter > wait {
			wait = min(retryAfter, maxRetryWait)
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// parseRetryAfter reads a Retry-After header given in seconds. Other forms give 0.
func parseRetryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return min(time.Duration(n)*time.Second, maxRetryWait)
}

// sleepCtx waits for d or until ctx is done. A non-positive d returns at once.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
