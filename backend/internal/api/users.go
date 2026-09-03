package api

import (
	"net/http"
	"strings"
	"sync"

	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type userView struct {
	AvatarURL    *string `json:"avatarUrl"`
	DisplayName  string  `json:"displayName"`
	FullName     string  `json:"fullName"`
	ID           string  `json:"id"`
	IsPrivate    bool    `json:"isPrivate"`
	Relationship string  `json:"relationship"`
	Username     string  `json:"username"`
}

func (s *Server) searchUsers(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len([]rune(query)) < 2 || len([]rune(query)) > 50 {
		respondError(w, http.StatusBadRequest, "invalid_query", "Arama için en az iki karakter girin.")
		return
	}
	var users []userView
	err := s.db.Query(r.Context(), `SELECT <string>id AS id, full_name AS fullName,
display_name AS displayName, username, avatar.public_url AS avatarUrl,
(SELECT VALUE is_private FROM privacy_setting WHERE account = $parent.id LIMIT 1)[0] ?? false AS isPrivate
FROM account WHERE status = 'active' AND id != type::record($account)
AND (string::lowercase(username) CONTAINS $query OR string::lowercase(display_name) CONTAINS $query)
ORDER BY username LIMIT 30;`, map[string]any{"account": accountID(r), "query": query}, &users)
	if err != nil {
		s.databaseError(w, "search users", err)
		return
	}
	if len(users) == 0 {
		respondJSON(w, http.StatusOK, map[string]any{"users": users})
		return
	}

	actor := accountID(r)
	pairs := make([]string, 0, len(users))
	userByPair := make(map[string]int, len(users))
	for index := range users {
		users[index].Relationship = "none"
		pair := security.PairKey(actor, users[index].ID)
		pairs = append(pairs, pair)
		userByPair[pair] = index
	}

	var friendships []struct {
		PairKey string `json:"pairKey"`
	}
	var requests []struct {
		PairKey string `json:"pairKey"`
		Sender  string `json:"sender"`
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var firstOp string
	wg.Add(2)
	go func() {
		defer wg.Done()
		var found []struct {
			PairKey string `json:"pairKey"`
		}
		err := s.db.Query(r.Context(), `SELECT pair_key AS pairKey FROM friendship WHERE pair_key IN $pairs;`, map[string]any{"pairs": pairs}, &found)
		mu.Lock()
		defer mu.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
			firstOp = "search friendship status"
		}
		friendships = found
	}()
	go func() {
		defer wg.Done()
		var found []struct {
			PairKey string `json:"pairKey"`
			Sender  string `json:"sender"`
		}
		err := s.db.Query(r.Context(), `SELECT pair_key AS pairKey, <string>sender AS sender FROM friend_request WHERE pair_key IN $pairs AND status = 'pending';`, map[string]any{"pairs": pairs}, &found)
		mu.Lock()
		defer mu.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
			firstOp = "search friend request status"
		}
		requests = found
	}()
	wg.Wait()
	if firstErr != nil {
		s.databaseError(w, firstOp, firstErr)
		return
	}
	for _, friendship := range friendships {
		if index, ok := userByPair[friendship.PairKey]; ok {
			users[index].Relationship = "friend"
		}
	}
	for _, request := range requests {
		if index, ok := userByPair[request.PairKey]; ok && users[index].Relationship == "none" {
			if request.Sender == actor {
				users[index].Relationship = "outgoing_request"
			} else {
				users[index].Relationship = "incoming_request"
			}
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"users": users})
}
