// Package api 提供 HTTP 接口。鉴权模型：
//   - admin：写入事件、确认结算、故障注入；
//   - auditor：审计接口（快照、差异、链校验、事件流）；
//   - creator/cooperative/channel/platform：只能读自己的分录与公开依据。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"

	"example.com/culture-sharing/internal/store"
)

// Config 是 API 层配置。
type Config struct {
	PlatformID       string
	EnableFaultHooks bool // 仅演示/测试环境开启
}

// Server 是 HTTP 服务。
type Server struct {
	st  *store.Store
	cfg Config
	mux *http.ServeMux
}

// NewServer 注册全部路由。
func NewServer(st *store.Store, cfg Config) *Server {
	s := &Server{st: st, cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.HandleFunc("POST /v1/events", s.requireAuth(s.handleIngestEvent, "admin"))
	s.mux.HandleFunc("POST /v1/settlements/confirm", s.requireAuth(s.handleConfirm, "admin"))
	s.mux.HandleFunc("GET /v1/statements", s.requireAuth(s.handleStatements))
	s.mux.HandleFunc("GET /v1/settlements/{id}", s.requireAuth(s.handleGetSettlement, "admin", "auditor"))
	s.mux.HandleFunc("GET /v1/audit/events", s.requireAuth(s.handleAuditEvents, "admin", "auditor"))
	s.mux.HandleFunc("GET /v1/audit/snapshot", s.requireAuth(s.handleSnapshot, "admin", "auditor"))
	s.mux.HandleFunc("GET /v1/audit/diff", s.requireAuth(s.handleDiff, "admin", "auditor"))
	s.mux.HandleFunc("GET /v1/audit/verify-chain", s.requireAuth(s.handleVerifyChain, "admin", "auditor"))
	s.mux.HandleFunc("GET /v1/watermarks", s.requireAuth(s.handleWatermarks, "admin", "auditor"))
	s.mux.HandleFunc("POST /v1/admin/fault", s.requireAuth(s.handleFault, "admin"))
	return s
}

// Handler 返回根处理器。
func (s *Server) Handler() http.Handler { return s.mux }

type ctxKey int

const principalKey ctxKey = iota

type principal struct {
	ID   string
	Kind string
}

// requireAuth 校验 Bearer token（sha256 比对），并按角色限制访问。
func (s *Server) requireAuth(next http.HandlerFunc, kinds ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		sum := sha256.Sum256([]byte(token))
		p, err := s.st.ParticipantByTokenHash(r.Context(), sum[:])
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if len(kinds) > 0 {
			allowed := false
			for _, k := range kinds {
				if p.Kind == k {
					allowed = true
					break
				}
			}
			if !allowed {
				writeErr(w, http.StatusForbidden, "insufficient role")
				return
			}
		}
		ctx := context.WithValue(r.Context(), principalKey, principal{ID: p.ID, Kind: p.Kind})
		next(w, r.WithContext(ctx))
	}
}

func principalOf(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey).(principal)
	return p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
