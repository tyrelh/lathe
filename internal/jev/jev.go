// Package jev asks TypeSafe's Jev model one Score question over HTTP. There is
// no Go SDK; this is the one endpoint lathe uses, with the response checked
// strictly enough that anything malformed is an error rather than an answer.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Model is pinned: tier descriptions and the confidence floor are tuned
// against one version, so moving it is a lathe change that comes with
// recalibrating.
const Model = "jev-1.13.0"

// PricePerMTokIn is jev-1.13.0's price in USD per million input tokens. Output
// is free.
const PricePerMTokIn = 0.042

// Score's level limits.
const (
	MinLevels = 2
	MaxLevels = 10
)

// Variables only so a test can point them at a local server and compress the
// waits; nothing in production writes to them.
var (
	Endpoint = "https://api.typesafe.ai/v1/systemone"
	// Timeout bounds one HTTP request, retries each getting their own.
	Timeout = 30 * time.Second
	// Backoff is the first retry's wait when the response names none; it
	// doubles per retry.
	Backoff = time.Second
)

// retries is how many more requests a 429 or 529 earns after the first, and
// maxWait caps a retry-after so a busy API cannot park a run for minutes.
const (
	retries = 3
	maxWait = 30 * time.Second
)

// question is the only one asked, so its id is fixed.
const question = "level"

// Answer is a validated Score answer. Probabilities is indexed by level. Raw
// is the last response body, set whenever one arrived, even with an error.
type Answer struct {
	Raw           string
	Model         string
	Probabilities []float64
	Score         float64
	Confidence    float64
	InputTokens   int
	OutputTokens  int
}

// Cost is what the request was billed, in USD.
func (a Answer) Cost() float64 { return float64(a.InputTokens) * PricePerMTokIn / 1e6 }

// Score asks one Score question about state, with criteria ordered from level 0
// up. It retries 429 and 529, honouring retry-after, and returns an error for
// a missing key, any other failed request, or a response that fails Check.
// On error the Answer holds only Raw, if a response arrived.
func Score(ctx context.Context, key, instructions string, criteria []string, state any) (Answer, error) {
	if key == "" {
		return Answer{}, errors.New("TYPESAFE_API_KEY is not set")
	}
	body, err := json.Marshal(map[string]any{
		"model": Model,
		"state": state,
		"questions": map[string]any{
			question: map[string]any{"type": "score", "instructions": instructions, "criteria": criteria},
		},
	})
	if err != nil {
		return Answer{}, err
	}
	wait := Backoff
	for try := 0; ; try++ {
		status, header, b, err := post(ctx, key, body)
		if err != nil {
			return Answer{}, err
		}
		if status == http.StatusOK {
			a, err := Check(b, len(criteria))
			a.Raw = string(b)
			return a, err
		}
		if (status != http.StatusTooManyRequests && status != 529) || try == retries {
			return Answer{Raw: string(b)}, fmt.Errorf("typesafe: HTTP %d: %s", status, snippet(b))
		}
		d := wait
		if s, err := strconv.Atoi(header.Get("Retry-After")); err == nil && s >= 0 {
			d = time.Duration(s) * time.Second
		}
		select {
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		case <-time.After(min(d, maxWait)):
		}
		wait *= 2
	}
}

func post(ctx context.Context, key string, body []byte) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("typesafe: %w", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("typesafe: reading response: %w", err)
	}
	return res.StatusCode, res.Header, b, nil
}

// snippet is the start of an error body, for a message that stays one line.
func snippet(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return strconv.Quote(string(b))
}

// response mirrors the API's JSON. Pointers so a missing or null field is
// distinguishable from zero; a number sent as a string fails to decode.
type response struct {
	Model   *string `json:"model"`
	Answers map[string]struct {
		Type          *string             `json:"type"`
		Score         *float64            `json:"score"`
		Confidence    *float64            `json:"confidence"`
		Probabilities map[string]*float64 `json:"probabilities"`
	} `json:"answers"`
	Usage *struct {
		InputTokens  *int `json:"input_tokens"`
		OutputTokens *int `json:"output_tokens"`
	} `json:"usage"`
}

// Check decodes a 200 response for a Score question with levels levels. The
// model must be Model, the answer a score, the probability keys exactly the
// level numbers, each in [0, 1] and summing to about 1, and score and
// confidence in range.
func Check(b []byte, levels int) (Answer, error) {
	var r response
	if err := json.Unmarshal(b, &r); err != nil {
		return Answer{}, fmt.Errorf("typesafe: response did not parse: %w", err)
	}
	if r.Model == nil || *r.Model != Model {
		return Answer{}, fmt.Errorf("typesafe: response model is not %s", Model)
	}
	q, ok := r.Answers[question]
	switch {
	case !ok:
		return Answer{}, errors.New("typesafe: response has no answer")
	case q.Type == nil || *q.Type != "score":
		return Answer{}, errors.New("typesafe: answer is not a score")
	case q.Score == nil || *q.Score < 0 || *q.Score > float64(levels-1):
		return Answer{}, errors.New("typesafe: score is missing or out of range")
	case q.Confidence == nil || *q.Confidence < 0 || *q.Confidence > 1:
		return Answer{}, errors.New("typesafe: confidence is missing or out of range")
	case len(q.Probabilities) != levels:
		return Answer{}, fmt.Errorf("typesafe: %d probabilities for %d levels", len(q.Probabilities), levels)
	case r.Usage == nil || r.Usage.InputTokens == nil || r.Usage.OutputTokens == nil:
		return Answer{}, errors.New("typesafe: usage is missing")
	}
	a := Answer{
		Model: *r.Model, Score: *q.Score, Confidence: *q.Confidence,
		Probabilities: make([]float64, levels),
		InputTokens:   *r.Usage.InputTokens, OutputTokens: *r.Usage.OutputTokens,
	}
	sum := 0.0
	for i := range levels {
		p := q.Probabilities[strconv.Itoa(i)]
		if p == nil || *p < 0 || *p > 1 {
			return Answer{}, fmt.Errorf("typesafe: probability for level %d is missing or out of range", i)
		}
		a.Probabilities[i] = *p
		sum += *p
	}
	if math.Abs(sum-1) > 0.01 {
		return Answer{}, fmt.Errorf("typesafe: probabilities sum to %.3f", sum)
	}
	return a, nil
}
