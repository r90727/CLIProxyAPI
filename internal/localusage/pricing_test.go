package localusage

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCostCategoriesAndUnknownModels(t *testing.T) {
	v := Totals{Events: 1, Input: 1000000, Cached: 600000, CacheWrite: 100000, Output: 200000, Reasoning: 100000}
	for _, tc := range []struct {
		model string
		want  float64
	}{
		{"gpt-6-astra", 14.85},
		{"gpt-6-astra-2026-09-01", 14.85},
		{"claude-fable-5-1", 14.4},
		{"claude-opus-5-20260901", 7.425},
	} {
		got := estimateCost(tc.model, v)
		if math.Abs(got.USD-tc.want) > 1e-9 || got.PricedEvents != 1 || got.UnpricedEvents != 0 {
			t.Errorf("%s: %+v, want %f", tc.model, got, tc.want)
		}
	}
	for _, model := range []string{"", "gpt-6-astra-custom", "claude-fable-5-2"} {
		if got := estimateCost(model, v); got.PricedEvents != 0 || got.UnpricedEvents != 1 || got.USD != 0 {
			t.Errorf("unknown %s: %+v", model, got)
		}
	}
	v.Input = 1
	if got := estimateCost("gpt-6-astra", v); got.UnpricedEvents != 1 {
		t.Fatalf("inconsistent token counts must be unpriced: %+v", got)
	}
}

func TestCostAggregationFiltersAndExport(t *testing.T) {
	s, importer, _ := fixture(t)
	for n, model := range []string{"gpt-6-astra", "claude-fable-5-1", "unknown"} {
		e := Event{ID: model, Model: model, Source: "t3-codex", Session: "same", Provider: "test", At: "2026-09-18T12:00:00Z", Input: 1000000, Output: 100000, Total: 1100000}
		if err := s.Put(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		e.ID += "-proxy"
		e.Source = "proxy"
		if n == 0 {
			if err := s.Put(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := Filter{Source: "t3", From: "2026-09-18", To: "2026-09-19"}
	for _, group := range []string{"total", "providers", "projects", "threads", "days"} {
		rows, err := s.Aggregate(context.Background(), f, group)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Events != 3 || rows[0].Total != 3300000 || rows[0].Cost.USD != 30 || rows[0].Cost.PricedEvents != 2 || rows[0].Cost.UnpricedEvents != 1 {
			t.Fatalf("%s: %+v", group, rows)
		}
	}
	f.Source = "proxy"
	rows, err := s.Aggregate(context.Background(), f, "total")
	if err != nil || len(rows) != 1 || rows[0].Cost.USD != 15 {
		t.Fatalf("proxy separation: %+v %v", rows, err)
	}
	f.From = "2026-09-19"
	f.To = "2026-09-20"
	rows, err = s.Aggregate(context.Background(), f, "total")
	if err != nil || len(rows) != 0 {
		t.Fatalf("date filter: %+v %v", rows, err)
	}
	h := Handler(s, importer, "127.0.0.1:8318")
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "http://127.0.0.1:8318/api/summary", nil))
	var summary struct {
		Total   []Totals `json:"total"`
		Pricing struct {
			Checked string `json:"checked"`
		} `json:"pricing"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Pricing.Checked != pricingChecked || summary.Total[0].Cost.USD != 30 {
		t.Fatalf("summary: %+v", summary)
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "http://127.0.0.1:8318/api/export.csv", nil))
	records, err := csv.NewReader(strings.NewReader(r.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || len(records[0]) != 17 || records[0][14] != "estimated_api_cost_usd" {
		t.Fatalf("CSV: %+v", records)
	}
	var sum float64
	for _, row := range records[1:] {
		if row[3] == "unknown" {
			if row[14] != "" || row[15] != "unpriced" {
				t.Fatalf("unknown CSV: %+v", row)
			}
		} else {
			var amount float64
			if err := json.Unmarshal([]byte(row[14]), &amount); err != nil {
				t.Fatal(err)
			}
			sum += amount
		}
	}
	if sum != 30 {
		t.Fatalf("CSV cost %f", sum)
	}
}
