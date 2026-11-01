package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Venky-collab/podmortem/internal/agent"
)

type fakeInvestigator struct {
	mu    sync.Mutex
	calls []string
	done  chan struct{}
}

func (f *fakeInvestigator) Investigate(_ context.Context, ns, pod, trigger string) *agent.Report {
	f.mu.Lock()
	f.calls = append(f.calls, ns+"/"+pod+" "+trigger)
	f.mu.Unlock()
	defer func() { f.done <- struct{}{} }()
	now := time.Now()
	return &agent.Report{ID: "r1", Namespace: ns, Pod: pod, Trigger: trigger, Analysis: "## Summary\nbroken", StartedAt: now, FinishedAt: now}
}

const alertBody = `{"alerts":[
 {"status":"firing","labels":{"alertname":"KubePodCrashLooping","namespace":"shop","pod":"api-1"}},
 {"status":"resolved","labels":{"alertname":"KubePodCrashLooping","namespace":"shop","pod":"api-2"}},
 {"status":"firing","labels":{"alertname":"NodeDown"}}
]}`

func TestAlertWebhookQueuesOnlyFiringPodAlerts(t *testing.T) {
	inv := &fakeInvestigator{done: make(chan struct{}, 10)}
	s := New(inv, Options{Dedupe: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, 1)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(alertBody)))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"queued":1`) {
		t.Fatalf("POST /alerts = %d %s", rec.Code, rec.Body)
	}
	<-inv.done
	if len(inv.calls) != 1 || inv.calls[0] != "shop/api-1 alert KubePodCrashLooping" {
		t.Fatalf("calls = %v", inv.calls)
	}

	// The same alert again within the dedupe window is ignored.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(alertBody)))
	if !strings.Contains(rec.Body.String(), `"queued":0`) {
		t.Errorf("duplicate alert was queued: %s", rec.Body)
	}

	// The finished report is listed and viewable.
	waitFor(t, func() bool { return s.find("r1") != nil })
	for path, want := range map[string]string{
		"/api/reports":  `"Pod":"api-1"`,
		"/reports/r1":   "# podmortem: shop/api-1",
		"/":             "shop/api-1",
		"/metrics":      "podmortem_triggers_skipped_total 1",
		"/reports/nope": "404",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Body.String(); !strings.Contains(got, want) && !(want == "404" && rec.Code == 404) {
			t.Errorf("GET %s: want %q in\n%s", path, want, got)
		}
	}
}

func TestBadPayloads(t *testing.T) {
	h := New(&fakeInvestigator{done: make(chan struct{}, 1)}, Options{}).Handler()
	for path, body := range map[string]string{"/alerts": "not json", "/investigate": `{"namespace":"x"}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", path, rec.Code)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
