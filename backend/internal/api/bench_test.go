package api

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hyd3dF/frame-social-2/internal/config"
)

type mockQueryer struct {
	mu      sync.Mutex
	latency time.Duration
	calls   int
	onQuery func(sql string, vars map[string]any, dest any) error
}

func (m *mockQueryer) Query(ctx context.Context, sql string, vars map[string]any, dest any) error {
	if m.latency > 0 {
		time.Sleep(m.latency)
	}
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if m.onQuery != nil {
		return m.onQuery(sql, vars, dest)
	}
	return nil
}

func (m *mockQueryer) Ping(ctx context.Context) error {
	return m.Query(ctx, "RETURN {ok:true}", nil, nil)
}

func percentile(durs []time.Duration, p float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	idx := int(float64(len(durs)) * p / 100)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(durs) {
		idx = len(durs) - 1
	}
	return durs[idx]
}

func BenchmarkSendMessage(b *testing.B) {
	mock := &mockQueryer{
		onQuery: func(sql string, vars map[string]any, dest any) error {
			return nil
		},
	}
	srv := &Server{
		cfg:     config.Config{JWTSecret: strings.Repeat("s", 32), OTPPepper: strings.Repeat("p", 32), AccessTokenMinutes: 15, RefreshTokenDays: 30, OTPMode: "development"},
		db:      mock,
		events:  newMessageEventBroker(),
		log:     slog.Default(),
		members: newMemberCache(),
		pending: newPendingStore(),
	}
	srv.persist = newPersister(mock, srv.pending, srv.members, slog.Default())
	srv.members.Set("conversation:abc", []string{"account:bench123", "account:other456"})
	body := `{"body":"hello bench","clientId":"client-12345678"}`
	b.ReportAllocs()
	b.ResetTimer()
	durs := make([]time.Duration, 0, b.N)
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", "abc")
		req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:bench123"))
		w := httptest.NewRecorder()
		start := time.Now()
		srv.sendMessage(w, req)
		durs = append(durs, time.Since(start))
		if w.Code != 201 {
			b.Fatalf("unexpected code %d body %s", w.Code, w.Body.String())
		}
	}
	b.StopTimer()
	p50 := percentile(durs, 50)
	p95 := percentile(durs, 95)
	p99 := percentile(durs, 99)
	b.ReportMetric(float64(p50.Microseconds()), "p50_us")
	b.ReportMetric(float64(p95.Microseconds()), "p95_us")
	b.ReportMetric(float64(p99.Microseconds()), "p99_us")
}

func BenchmarkSearchUsers(b *testing.B) {
	mock := &mockQueryer{
		onQuery: func(sql string, vars map[string]any, dest any) error {
			if strings.Contains(sql, "FROM account") {
				if d, ok := dest.(*[]userView); ok {
					*d = []userView{{ID: "account:u1", Username: "alice", DisplayName: "Alice", FullName: "Alice"}}
				}
			}
			return nil
		},
	}
	srv := &Server{
		cfg:     config.Config{JWTSecret: strings.Repeat("s", 32), OTPPepper: strings.Repeat("p", 32)},
		db:      mock,
		events:  newMessageEventBroker(),
		log:     slog.Default(),
		members: newMemberCache(),
		pending: newPendingStore(),
	}
	b.ReportAllocs()
	b.ResetTimer()
	durs := make([]time.Duration, 0, b.N)
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/v1/users/search?q=ali", nil)
		req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:bench123"))
		w := httptest.NewRecorder()
		start := time.Now()
		srv.searchUsers(w, req)
		durs = append(durs, time.Since(start))
		if w.Code != 200 {
			b.Fatalf("code %d", w.Code)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(percentile(durs, 50).Microseconds()), "p50_us")
	b.ReportMetric(float64(percentile(durs, 95).Microseconds()), "p95_us")
	b.ReportMetric(float64(percentile(durs, 99).Microseconds()), "p99_us")
}

func BenchmarkListConversations(b *testing.B) {
	mock := &mockQueryer{
		onQuery: func(sql string, vars map[string]any, dest any) error {
			if d, ok := dest.(*[]conversationView); ok {
				*d = make([]conversationView, 20)
			}
			return nil
		},
	}
	srv := &Server{
		cfg:     config.Config{JWTSecret: strings.Repeat("s", 32)},
		db:      mock,
		events:  newMessageEventBroker(),
		log:     slog.Default(),
		members: newMemberCache(),
		pending: newPendingStore(),
	}
	b.ReportAllocs()
	b.ResetTimer()
	durs := make([]time.Duration, 0, b.N)
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/v1/conversations", nil)
		req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:bench123"))
		w := httptest.NewRecorder()
		start := time.Now()
		srv.listConversations(w, req)
		durs = append(durs, time.Since(start))
		if w.Code != 200 {
			b.Fatalf("code %d", w.Code)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(percentile(durs, 50).Microseconds()), "p50_us")
	b.ReportMetric(float64(percentile(durs, 95).Microseconds()), "p95_us")
	b.ReportMetric(float64(percentile(durs, 99).Microseconds()), "p99_us")
}
