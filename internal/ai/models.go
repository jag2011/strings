package ai

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Model is a model offered for AI users.
type Model struct {
	ID   string // the OpenRouter model name, like anthropic/claude-opus-5.5
	Name string
	// Prices in USD per million tokens; 0 if unknown.
	Input, Output float64
}

// Label describes the model in the model picker.
func (m Model) Label() string {
	if m.Input == 0 && m.Output == 0 {
		return m.Name
	}
	return fmt.Sprintf("%s · $%s / $%s per M tokens", m.Name, price(m.Input), price(m.Output))
}

func price(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Catalog lists the models on OpenRouter that runs can use: text models
// that call tools. The list is public, so no key is needed. It is cached
// and refreshed in the background once stale.
type Catalog struct {
	URL    string
	TTL    time.Duration
	Client *http.Client

	mu       sync.Mutex
	models   []Model
	fetched  time.Time
	fetching bool
}

func NewCatalog() *Catalog {
	return &Catalog{URL: "https://openrouter.ai/api/v1/models", TTL: 6 * time.Hour, Client: &http.Client{Timeout: 10 * time.Second}}
}

// Models returns the models, sorted by ID. The first call waits for the
// list, as long as ctx allows; later ones return the cached list at once
// and refresh it in the background when stale. On failure it returns nil,
// and the next call tries again.
func (c *Catalog) Models(ctx context.Context) []Model {
	c.mu.Lock()
	models, stale := c.models, time.Since(c.fetched) > c.TTL
	if models != nil {
		if stale && !c.fetching {
			c.fetching = true
			go c.refresh(context.Background())
		}
		c.mu.Unlock()
		return models
	}
	c.mu.Unlock()
	return c.refresh(ctx)
}

func (c *Catalog) refresh(ctx context.Context) []Model {
	models, err := c.fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetching = false
	if err != nil {
		return c.models
	}
	c.models, c.fetched = models, time.Now()
	return models
}

func (c *Catalog) fetch(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.URL, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: %s", res.Status)
	}
	var body struct {
		Data []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Architecture struct {
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	var out []Model
	for _, m := range body.Data {
		// Variants like ":free" or ":batch" are left out: free ones are
		// rate limited, batch ones don't answer right away.
		if strings.Contains(m.ID, ":") || !slices.Contains(m.SupportedParameters, "tools") ||
			!slices.Equal(m.Architecture.OutputModalities, []string{"text"}) {
			continue
		}
		out = append(out, Model{ID: m.ID, Name: m.Name, Input: perMillion(m.Pricing.Prompt), Output: perMillion(m.Pricing.Completion)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models: none usable")
	}
	slices.SortFunc(out, func(a, b Model) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// perMillion turns a price per token, as OpenRouter gives it, into one per
// million tokens.
func perMillion(perToken string) float64 {
	v, err := strconv.ParseFloat(perToken, 64)
	if err != nil || v < 0 {
		return 0
	}
	return float64(int64(v*1e8+0.5)) / 100 // cents precision
}
