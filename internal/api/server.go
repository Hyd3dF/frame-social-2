package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Hyd3dF/frame-social-2/internal/config"
	"github.com/Hyd3dF/frame-social-2/internal/database"
)

type queryer interface {
	Query(context.Context, string, map[string]any, any) error
	Ping(context.Context) error
}

type Server struct {
	cfg             config.Config
	db              queryer
	deletedAccounts sync.Map
	events          *messageEventBroker
	log             *slog.Logger
	members         *memberCache
	pending         *pendingStore
	persist         *persister
	limiter         messageRateLimiter
	messageDeletion *messageDeletionStore
	messageLocks    [64]sync.Mutex
	pushStore       *pushStore
	pusher          Pusher
	pushQueue       chan pushJob
}

type contextKey string

const (
	accountKey   contextKey = "accountID"
	requestIDKey contextKey = "requestID"
)

func New(cfg config.Config, db *database.Client, logger *slog.Logger) http.Handler {
	members := newMemberCache()
	pending := newPendingStore()
	server := &Server{cfg: cfg, db: db, events: newMessageEventBroker(), log: logger, members: members, pending: pending}
	server.persist = newPersister(db, pending, members, logger)
	server.limiter = newMemoryMessageRateLimiter()
	server.messageDeletion = newMessageDeletionStore(db)
	if err := startGroupSchema(db); err != nil {
		logger.Error("group schema initialization failed", "error", err)
	}
	server.pushStore = newPushStore(db, logger)
	server.pusher = initPusher(cfg, logger)
	if _, disabled := server.pusher.(*noopPusher); !disabled {
		server.pushQueue = make(chan pushJob, 1000)
		for range 4 {
			go server.pushLoop()
		}
	}
	return server.handler()
}

func (s *Server) handler() http.Handler {
	return recoverer(s.log, securityHeaders(requestIDMiddleware(s.routes())))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.log.Error("database health check failed", "error", err)
		w.Header().Set("Retry-After", "5")
		respondError(w, http.StatusServiceUnavailable, "service_unavailable", "Servis geçici olarak kullanılamıyor.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
