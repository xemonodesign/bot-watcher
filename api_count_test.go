package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildApiStatsReportSortsLabelsAndSkipsZero(t *testing.T) {
	got := buildApiStatsReport("2026-10-16", map[string]int{
		"xemono.life": 12,
		"chisei":      3,
		"hibot":       0,
	})
	want := "📊 カウンター (2026-10-16)\nchisei : 3\nxemono.life : 12\n"
	if got != want {
		t.Fatalf("unexpected report:\n%q\nwant:\n%q", got, want)
	}
}

func TestBuildApiStatsReportEmpty(t *testing.T) {
	got := buildApiStatsReport("2026-10-16", map[string]int{})
	want := "📊 カウンター (2026-10-16)\n記録なし\n"
	if got != want {
		t.Fatalf("unexpected report: %q", got)
	}
}

func TestRolloverKeepsYesterdayAndResetsToday(t *testing.T) {
	statsMutex.Lock()
	apiStats = map[string]int{"hibot": 5}
	yesterdayStats, yesterdayDate = nil, ""
	statsMutex.Unlock()

	rolloverApiStats()

	statsMutex.Lock()
	defer statsMutex.Unlock()
	if yesterdayStats["hibot"] != 5 {
		t.Fatalf("yesterday stats not kept: %v", yesterdayStats)
	}
	if len(apiStats) != 0 {
		t.Fatalf("today stats not reset: %v", apiStats)
	}
	if len(yesterdayDate) != len("2006-01-02") {
		t.Fatalf("unexpected date %q", yesterdayDate)
	}
}

func TestHandleCountIncrementsAndSetsCORS(t *testing.T) {
	statsMutex.Lock()
	apiStats = make(map[string]int)
	statsMutex.Unlock()

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/count?label=chisei", nil)
		handleCount(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("missing CORS header")
		}
	}

	statsMutex.Lock()
	defer statsMutex.Unlock()
	if apiStats["chisei"] != 2 {
		t.Fatalf("expected 2, got %d", apiStats["chisei"])
	}
}

func TestHandleCountPreflightDoesNotCount(t *testing.T) {
	statsMutex.Lock()
	apiStats = make(map[string]int)
	statsMutex.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/count?label=hibot", nil)
	handleCount(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}

	statsMutex.Lock()
	defer statsMutex.Unlock()
	if apiStats["hibot"] != 0 {
		t.Fatalf("preflight must not count, got %d", apiStats["hibot"])
	}
}

func TestLoadLocationDefaultsToTokyo(t *testing.T) {
	if loadLocation("").String() != "Asia/Tokyo" {
		t.Fatalf("default timezone should be Asia/Tokyo")
	}
	if loadLocation("Not/AZone") != loadLocation("Not/AZone") {
		t.Fatalf("invalid zone should fall back deterministically")
	}
}
