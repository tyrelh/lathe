package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const good = `{"model":"jev-1.13.0","answers":{"level":{"type":"score","score":1.2,"confidence":0.4,
	"legend":{"0":"a","1":"b","2":"c"},"probabilities":{"0":0.1,"1":0.6,"2":0.3}}},
	"usage":{"input_tokens":1000,"output_tokens":3}}`

// serve points Endpoint at a server that answers each request with the next of
// replies, as status and body, and counts requests.
func serve(t *testing.T, replies ...[2]string) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var req struct {
			Model     string `json:"model"`
			Questions map[string]struct {
				Type     string   `json:"type"`
				Criteria []string `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != Model ||
			req.Questions["level"].Type != "score" || len(req.Questions["level"].Criteria) != 3 {
			t.Errorf("request: %+v %v", req, err)
		}
		reply := replies[min(i, len(replies)-1)]
		w.Header().Set("Retry-After", "0")
		var status int
		json.Unmarshal([]byte(reply[0]), &status)
		w.WriteHeader(status)
		w.Write([]byte(reply[1]))
	}))
	t.Cleanup(srv.Close)
	old, oldBackoff := Endpoint, Backoff
	Endpoint, Backoff = srv.URL, time.Millisecond
	t.Cleanup(func() { Endpoint, Backoff = old, oldBackoff })
	return &n
}

var criteria = []string{"a", "b", "c"}

func TestScore(t *testing.T) {
	n := serve(t, [2]string{"529", "busy"}, [2]string{"429", "slow"}, [2]string{"200", good})
	a, err := Score(context.Background(), "k", "how hard?", criteria, map[string]string{"request": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 || a.Probabilities[1] != 0.6 || a.Confidence != 0.4 || a.InputTokens != 1000 {
		t.Fatalf("requests %d, answer %+v", n.Load(), a)
	}
	if got := a.Cost(); got != 0.000042 {
		t.Fatalf("cost = %v", got)
	}
}

func TestScoreRetriesAreBounded(t *testing.T) {
	n := serve(t, [2]string{"529", "busy"})
	if _, err := Score(context.Background(), "k", "q", criteria, "s"); err == nil || !strings.Contains(err.Error(), "529") {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != retries+1 {
		t.Fatalf("requests = %d", n.Load())
	}
}

func TestScoreDoesNotRetryOtherErrors(t *testing.T) {
	n := serve(t, [2]string{"401", "no"})
	if _, err := Score(context.Background(), "k", "q", criteria, "s"); err == nil {
		t.Fatal("401 accepted")
	}
	if n.Load() != 1 {
		t.Fatalf("requests = %d", n.Load())
	}
}

func TestScoreNeedsAKey(t *testing.T) {
	n := serve(t, [2]string{"200", good})
	if _, err := Score(context.Background(), "", "q", criteria, "s"); err == nil || !strings.Contains(err.Error(), "TYPESAFE_API_KEY") {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 0 {
		t.Fatal("called the API without a key")
	}
}

func TestCheckRejects(t *testing.T) {
	for name, edit := range map[string][2]string{
		"other model":       {`"jev-1.13.0"`, `"jev-1.14.0"`},
		"choice":            {`"type":"score"`, `"type":"choice"`},
		"score range":       {`"score":1.2`, `"score":2.5`},
		"string score":      {`"score":1.2`, `"score":"1.2"`},
		"null confidence":   {`"confidence":0.4`, `"confidence":null`},
		"confidence range":  {`"confidence":0.4`, `"confidence":1.4`},
		"missing level":     {`"2":0.3}}`, `"3":0.3}}`},
		"extra level":       {`"2":0.3}}`, `"2":0.3,"3":0}}`},
		"negative p":        {`"0":0.1`, `"0":-0.1`},
		"sum":               {`"2":0.3}}`, `"2":0.5}}`},
		"no usage":          {`"usage":{"input_tokens":1000,"output_tokens":3}`, `"usage":null`},
		"other question id": {`"level":{`, `"other":{`},
	} {
		t.Run(name, func(t *testing.T) {
			b := strings.Replace(good, edit[0], edit[1], 1)
			if b == good {
				t.Fatal("edit did not apply")
			}
			if _, err := Check([]byte(b), 3); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := Check([]byte(good), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := Check([]byte(good), 4); err == nil {
		t.Fatal("accepted the wrong number of levels")
	}
}
