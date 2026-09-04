package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Hyd3dF/frame-social-2/internal/config"
	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type groupMockDB struct {
	mu            sync.Mutex
	groups        map[string]groupView
	groupPass     map[string]string
	members       map[string]map[string]string // conversation -> account -> role
	invitations   map[string]map[string]string // inviteID -> status
	inviteLookup  map[string]string            // group:recipient -> inviteID
	joinReq       map[string]map[string]string
	joinLookup    map[string]string
	blocked       map[string]bool
	conversations map[string]string // id -> kind
}

type groupSchemaCaptureDB struct {
	queries []string
}

func (d *groupSchemaCaptureDB) Ping(context.Context) error { return nil }

func (d *groupSchemaCaptureDB) Query(_ context.Context, sql string, _ map[string]any, _ any) error {
	d.queries = append(d.queries, sql)
	return nil
}

func TestGroupSchemaSupportsProductionSchemafullTables(t *testing.T) {
	db := &groupSchemaCaptureDB{}
	if err := newGroupStore(db).ensureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(db.queries, "\n")
	for _, required := range []string{
		"group_id ON conversation",
		"group_name ON conversation",
		"group_description ON conversation",
		"group_image_url ON conversation",
		"group_privacy ON conversation",
		"group_join_rule ON conversation",
		"group_password_hash ON conversation",
		"['member', 'admin', 'owner']",
	} {
		if !strings.Contains(all, required) {
			t.Fatalf("group schema is missing %q", required)
		}
	}
}

func newGroupMockDB() *groupMockDB {
	return &groupMockDB{
		groups:        make(map[string]groupView),
		groupPass:     make(map[string]string),
		members:       make(map[string]map[string]string),
		invitations:   make(map[string]map[string]string),
		inviteLookup:  make(map[string]string),
		joinReq:       make(map[string]map[string]string),
		joinLookup:    make(map[string]string),
		blocked:       make(map[string]bool),
		conversations: make(map[string]string),
	}
}

func (m *groupMockDB) Ping(context.Context) error { return nil }

func (m *groupMockDB) Query(_ context.Context, sql string, vars map[string]any, dest any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	write := func(v any) {
		if dest == nil {
			return
		}
		b, _ := json.Marshal(v)
		_ = json.Unmarshal(b, dest)
	}
	if strings.Contains(sql, "DEFINE TABLE") {
		return nil
	}
	// group lookup
	if strings.Contains(sql, "FROM type::record($group) WHERE kind = 'group'") {
		g, _ := vars["group"].(string)
		if gv, ok := m.groups[g]; ok {
			write([]groupView{gv})
			return nil
		}
		write([]groupView{})
		return nil
	}
	// role
	if strings.Contains(sql, "SELECT role FROM conversation_member WHERE in = type::record($account) AND out = type::record($group)") {
		g, _ := vars["group"].(string)
		a, _ := vars["account"].(string)
		if members, ok := m.members[g]; ok {
			if role, ok := members[a]; ok {
				write([]struct {
					Role string `json:"role"`
				}{{Role: role}})
				return nil
			}
		}
		write([]struct {
			Role string `json:"role"`
		}{})
		return nil
	}
	// conversation kind
	if strings.Contains(sql, "SELECT kind FROM type::record($conversation)") {
		c, _ := vars["conversation"].(string)
		if k, ok := m.conversations[c]; ok {
			write([]struct {
				Kind string `json:"kind"`
			}{{Kind: k}})
			return nil
		}
		if members, ok := m.members[c]; ok && len(members) > 2 {
			write([]struct {
				Kind string `json:"kind"`
			}{{Kind: "group"}})
			return nil
		}
		write([]struct {
			Kind string `json:"kind"`
		}{{Kind: "direct"}})
		return nil
	}
	if strings.Contains(sql, "SELECT <string>id AS id FROM conversation_member WHERE in = type::record($account) AND out = type::record($conversation) AND left_at IS NONE") {
		a, _ := vars["account"].(string)
		c, _ := vars["conversation"].(string)
		if members, ok := m.members[c]; ok {
			if _, ok := members[a]; ok {
				write([]recordID{{ID: "member:test"}})
				return nil
			}
		}
		write([]recordID{})
		return nil
	}
	// create group
	if strings.Contains(sql, "CREATE ONLY type::record($group) CONTENT") && strings.Contains(sql, "kind: 'group'") {
		g, _ := vars["group"].(string)
		if _, exists := m.groups[g]; exists {
			return &mockAlreadyExistsError{}
		}
		rawID, _ := vars["groupID"].(string)
		_ = rawID
		name, _ := vars["name"].(string)
		desc, _ := vars["description"].(string)
		var img *string
		if v, ok := vars["image"]; ok && v != nil {
			if s, ok := v.(*string); ok {
				img = s
			} else if s, ok := v.(string); ok && s != "" {
				img = &s
			}
		}
		privacy, _ := vars["privacy"].(string)
		joinRule, _ := vars["joinRule"].(string)
		hash, _ := vars["passwordHash"].(string)
		view := groupView{ID: g, Name: name, Description: desc, ImageURL: img, Privacy: privacy, JoinRule: joinRule}
		m.groups[g] = view
		m.groupPass[g] = hash
		m.conversations[g] = "group"
		if _, ok := m.members[g]; !ok {
			m.members[g] = make(map[string]string)
		}
		account, _ := vars["account"].(string)
		m.members[g][account] = "owner"
		return nil
	}
	// addMember
	if strings.Contains(sql, "RELATE $account_record->conversation_member->$group_record") {
		g, _ := vars["group"].(string)
		a, _ := vars["account"].(string)
		role, _ := vars["role"].(string)
		if _, ok := m.members[g]; !ok {
			m.members[g] = make(map[string]string)
		}
		if _, exists := m.members[g][a]; !exists {
			m.members[g][a] = role
		}
		return nil
	}
	// update group
	if strings.Contains(sql, "UPDATE type::record($group) SET") && strings.Contains(sql, "group_name") {
		g, _ := vars["group"].(string)
		if view, ok := m.groups[g]; ok {
			if v, ok := vars["hasName"].(bool); ok && v {
				if s, _ := vars["name"].(string); s != "" {
					view.Name = s
				}
			}
			if v, ok := vars["hasDescription"].(bool); ok && v {
				if s, _ := vars["description"].(string); true {
					view.Description = s
				}
			}
			if v, ok := vars["hasImage"].(bool); ok && v {
				if img, _ := vars["image"].(*string); true {
					view.ImageURL = img
				}
			}
			if v, ok := vars["hasPrivacy"].(bool); ok && v {
				if s, _ := vars["privacy"].(string); s != "" {
					view.Privacy = s
				}
			}
			if v, ok := vars["hasJoinRule"].(bool); ok && v {
				if s, _ := vars["joinRule"].(string); s != "" {
					view.JoinRule = s
				}
			}
			if v, ok := vars["hasPassword"].(bool); ok && v {
				if hash, _ := vars["password"].(string); hash != "" {
					m.groupPass[g] = hash
				}
			}
			m.groups[g] = view
		}
		return nil
	}
	// search groups
	if strings.Contains(sql, "FROM conversation WHERE kind = 'group'") && strings.Contains(sql, "group_id") {
		q, _ := vars["query"].(string)
		account, _ := vars["account"].(string)
		var out []groupView
		for _, gv := range m.groups {
			raw := strings.TrimPrefix(gv.ID, "conversation:")
			if !strings.Contains(strings.ToLower(raw), strings.ToLower(q)) {
				continue
			}
			if gv.Privacy == "private" {
				if members, ok := m.members[gv.ID]; !ok || members[account] == "" {
					continue
				}
			}
			out = append(out, gv)
			if len(out) >= 20 {
				break
			}
		}
		write(out)
		return nil
	}
	// blocked check
	if strings.Contains(sql, "FROM blocked_account WHERE pair_key = $pair") {
		pair, _ := vars["pair"].(string)
		if m.blocked[pair] {
			write([]recordID{{ID: "blocked_account:test"}})
		} else {
			write([]recordID{})
		}
		return nil
	}
	// invitation lookup existing
	if strings.Contains(sql, "FROM group_invitation WHERE group = type::record($group) AND recipient = type::record($recipient)") {
		g, _ := vars["group"].(string)
		r, _ := vars["recipient"].(string)
		key := g + ":" + r
		if id, ok := m.inviteLookup[key]; ok {
			write([]recordID{{ID: id}})
		} else {
			write([]recordID{})
		}
		return nil
	}
	// create invitation
	if strings.Contains(sql, "CREATE group_invitation CONTENT") {
		g, _ := vars["group"].(string)
		r, _ := vars["recipient"].(string)
		key := g + ":" + r
		if id, ok := m.inviteLookup[key]; ok {
			write([]recordID{{ID: id}})
			return nil
		}
		id := "group_invitation:" + strings.ReplaceAll(key, ":", "_")
		m.inviteLookup[key] = id
		if _, ok := m.invitations[id]; !ok {
			m.invitations[id] = make(map[string]string)
		}
		m.invitations[id]["group"] = g
		m.invitations[id]["recipient"] = r
		m.invitations[id]["status"] = "pending"
		sender, _ := vars["sender"].(string)
		m.invitations[id]["sender"] = sender
		write([]recordID{{ID: id}})
		return nil
	}
	// read invitation
	if strings.Contains(sql, "SELECT <string>recipient AS recipient, status FROM type::record($invite)") {
		invite, _ := vars["invite"].(string)
		if inv, ok := m.invitations[invite]; ok {
			write([]struct {
				Recipient string `json:"recipient"`
				Status    string `json:"status"`
			}{{Recipient: inv["recipient"], Status: inv["status"]}})
		} else {
			write([]struct {
				Recipient string `json:"recipient"`
				Status    string `json:"status"`
			}{})
		}
		return nil
	}
	// update invitation status
	if strings.Contains(sql, "UPDATE type::record($invite) SET status") {
		invite, _ := vars["invite"].(string)
		status, _ := vars["status"].(string)
		if inv, ok := m.invitations[invite]; ok {
			inv["status"] = status
		}
		return nil
	}
	// join request lookup existing
	if strings.Contains(sql, "FROM group_join_request WHERE group = type::record($group) AND account = type::record($account)") {
		g, _ := vars["group"].(string)
		a, _ := vars["account"].(string)
		key := g + ":" + a
		if id, ok := m.joinLookup[key]; ok {
			write([]recordID{{ID: id}})
		} else {
			write([]recordID{})
		}
		return nil
	}
	// create join request
	if strings.Contains(sql, "CREATE group_join_request CONTENT") {
		g, _ := vars["group"].(string)
		a, _ := vars["account"].(string)
		key := g + ":" + a
		if id, ok := m.joinLookup[key]; ok {
			write([]recordID{{ID: id}})
			return nil
		}
		id := "group_join_request:" + strings.ReplaceAll(key, ":", "_")
		m.joinLookup[key] = id
		if _, ok := m.joinReq[id]; !ok {
			m.joinReq[id] = make(map[string]string)
		}
		m.joinReq[id]["group"] = g
		m.joinReq[id]["account"] = a
		m.joinReq[id]["status"] = "pending"
		write([]recordID{{ID: id}})
		return nil
	}
	// read join request
	if strings.Contains(sql, "SELECT <string>account AS account, status FROM type::record($request)") {
		req, _ := vars["request"].(string)
		if jr, ok := m.joinReq[req]; ok {
			write([]struct {
				Account string `json:"account"`
				Status  string `json:"status"`
			}{{Account: jr["account"], Status: jr["status"]}})
		} else {
			write([]struct {
				Account string `json:"account"`
				Status  string `json:"status"`
			}{})
		}
		return nil
	}
	// update join request
	if strings.Contains(sql, "UPDATE type::record($request) SET status") {
		req, _ := vars["request"].(string)
		status, _ := vars["status"].(string)
		if jr, ok := m.joinReq[req]; ok {
			jr["status"] = status
		}
		return nil
	}
	// group password hash
	if strings.Contains(sql, "SELECT group_password_hash AS hash") {
		g, _ := vars["group"].(string)
		hash := m.groupPass[g]
		write([]struct {
			Hash string `json:"hash"`
		}{{Hash: hash}})
		return nil
	}
	// list members
	if strings.Contains(sql, "FROM conversation_member WHERE out = type::record($group)") && strings.Contains(sql, "SELECT <string>in.id") {
		g, _ := vars["group"].(string)
		var out []groupMemberView
		if members, ok := m.members[g]; ok {
			for acc, role := range members {
				out = append(out, groupMemberView{
					Role: role,
				})
				_ = acc
			}
		}
		// simplified: return members with IDs via json directly
		// Build actual slice with proper fields for test
		var actual []groupMemberView
		if members, ok := m.members[g]; ok {
			for acc, role := range members {
				actual = append(actual, groupMemberView{
					Role: role,
				})
				// set ID via reflection hack: use map
				_ = acc
			}
		}
		// Use generic map to allow ID fill: we need to handle via dest being []groupMemberView with embedded userView
		// Instead write using groupMemberView with ID
		var result []groupMemberView
		if members, ok := m.members[g]; ok {
			for acc, role := range members {
				result = append(result, groupMemberView{
					Role: role,
				})
				// overwrite ID after marshal? Instead construct via json map
				_ = acc
			}
		}
		// Simpler: write result with IDs populated via direct assignment after write
		// We'll just write with IDs using underlying logic: we cannot set ID due to embedded struct, so we handle via map
		if dest != nil {
			// Build slice of maps then unmarshal to groupMemberView should set ID
			type tmp struct {
				ID   string `json:"id"`
				Role string `json:"role"`
			}
			var tmps []tmp
			if members, ok := m.members[g]; ok {
				for acc, role := range members {
					tmps = append(tmps, tmp{ID: acc, Role: role})
				}
			}
			b, _ := json.Marshal(tmps)
			// dest is *[]groupMemberView; unmarshal via b will set Role and ID via embedded userView?
			// groupMemberView embeds userView which has ID json:"id", so it should map.
			_ = json.Unmarshal(b, dest)
			return nil
		}
		write(out)
		return nil
	}
	// leave / remove
	if strings.Contains(sql, "UPDATE conversation_member SET left_at") {
		g, _ := vars["group"].(string)
		var account string
		if v, ok := vars["account"].(string); ok {
			account = v
		} else if v, ok := vars["target"].(string); ok {
			account = v
		}
		if members, ok := m.members[g]; ok {
			delete(members, account)
		}
		return nil
	}
	// ownership transfer
	if strings.Contains(sql, "UPDATE conversation_member SET role = 'owner'") {
		g, _ := vars["group"].(string)
		target, _ := vars["target"].(string)
		owner, _ := vars["owner"].(string)
		if members, ok := m.members[g]; ok {
			if _, ok := members[target]; ok {
				members[target] = "owner"
				members[owner] = "admin"
			}
		}
		return nil
	}
	// change role
	if strings.Contains(sql, "UPDATE conversation_member SET role = $role") {
		g, _ := vars["group"].(string)
		target, _ := vars["target"].(string)
		role, _ := vars["role"].(string)
		if members, ok := m.members[g]; ok {
			if _, ok := members[target]; ok {
				members[target] = role
			}
		}
		return nil
	}
	// count owners
	if strings.Contains(sql, "SELECT <string>in.id AS id FROM conversation_member WHERE out = type::record($group) AND role = 'owner'") {
		g, _ := vars["group"].(string)
		var ids []recordID
		if members, ok := m.members[g]; ok {
			for acc, role := range members {
				if role == "owner" {
					ids = append(ids, recordID{ID: acc})
				}
			}
		}
		write(ids)
		return nil
	}
	// generic fallback for message related queries: allow sending group messages
	if strings.Contains(sql, "SELECT <string>in AS id FROM conversation_member WHERE out = type::record($conversation)") {
		conv, _ := vars["conversation"].(string)
		if members, ok := m.members[conv]; ok {
			var ids []recordID
			for acc := range members {
				ids = append(ids, recordID{ID: acc})
			}
			write(ids)
			return nil
		}
		// fallback for direct conversation members via m.members map keyed by conversation
		write([]recordID{})
		return nil
	}
	if strings.Contains(sql, "FROM message") || strings.Contains(sql, "LET $deletion") {
		// allow message creation/persistence
		return nil
	}
	if strings.Contains(sql, "FROM conversation_member WHERE in = type::record($account) AND left_at IS NONE") && strings.Contains(sql, "SELECT <string>out.id") {
		write([]conversationView{})
		return nil
	}
	// fallback
	if dest != nil {
		b, _ := json.Marshal([]interface{}{})
		_ = json.Unmarshal(b, dest)
	}
	return nil
}

type mockAlreadyExistsError struct{}

func (e *mockAlreadyExistsError) Error() string { return "already exists" }

func newGroupServer(db *groupMockDB) (*Server, http.Handler) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	pending := newPendingStore()
	members := newMemberCache()
	srv := &Server{
		cfg:     cfg,
		db:      db,
		events:  newMessageEventBroker(),
		log:     logger,
		members: members,
		pending: pending,
	}
	srv.persist = newPersister(db, pending, members, logger)
	srv.limiter = newMemoryMessageRateLimiter()
	srv.messageDeletion = newMessageDeletionStore(db)
	return srv, srv.handler()
}

func groupToken(account string) string {
	t, _ := security.AccessToken(strings.Repeat("s", 32), account, 15)
	return t
}

func TestGroupCreateAndSearch(t *testing.T) {
	db := newGroupMockDB()
	owner := "account:alice"
	_, handler := newGroupServer(db)
	// create public group
	body := `{"id":"mygroup","name":"My Group","description":"desc","privacy":"public","joinRule":"open"}`
	req := httptest.NewRequest("POST", "/v1/groups", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create group expected 201 got %d %s", w.Code, w.Body.String())
	}
	// duplicate should be 409
	req = httptest.NewRequest("POST", "/v1/groups", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatalf("duplicate should be 409 got %d", w.Code)
	}
	// search as non-member should find public
	req = httptest.NewRequest("GET", "/v1/groups/search?q=mygroup", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken("account:bob"))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("search public %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Groups []groupView `json:"groups"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Groups) != 1 || resp.Groups[0].Name != "My Group" {
		t.Fatalf("search result %#v", resp)
	}
	// private group not visible to non-member
	db.groups["conversation:priv"] = groupView{ID: "conversation:priv", Name: "Priv", Privacy: "private", JoinRule: "invite"}
	db.conversations["conversation:priv"] = "group"
	db.members["conversation:priv"] = map[string]string{owner: "owner"}
	req = httptest.NewRequest("GET", "/v1/groups/search?q=priv", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken("account:bob"))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Groups) != 0 {
		t.Fatalf("private should be hidden got %d", len(resp.Groups))
	}
	// member can see private
	req = httptest.NewRequest("GET", "/v1/groups/search?q=priv", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Groups) != 1 {
		t.Fatalf("owner should see private got %d", len(resp.Groups))
	}
}

func TestGroupUpdateAuthorization(t *testing.T) {
	db := newGroupMockDB()
	owner := "account:alice"
	member := "account:bob"
	outsider := "account:eve"
	db.groups["conversation:g1"] = groupView{ID: "conversation:g1", Name: "G1", Privacy: "public", JoinRule: "open"}
	db.conversations["conversation:g1"] = "group"
	db.members["conversation:g1"] = map[string]string{owner: "owner", member: "member"}
	_, handler := newGroupServer(db)
	// member cannot update name
	req := httptest.NewRequest("PATCH", "/v1/groups/g1/name", strings.NewReader(`{"name":"NewName"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(member))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("member update should be 403 got %d", w.Code)
	}
	// outsider cannot update
	req = httptest.NewRequest("PATCH", "/v1/groups/g1/name", strings.NewReader(`{"name":"NewName"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(outsider))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("outsider should be 403 got %d", w.Code)
	}
	// promote member to admin then can update
	db.members["conversation:g1"][member] = "admin"
	req = httptest.NewRequest("PATCH", "/v1/groups/g1/name", strings.NewReader(`{"name":"NewName"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(member))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("admin update %d %s", w.Code, w.Body.String())
	}
	// owner can change access
	req = httptest.NewRequest("PATCH", "/v1/groups/g1/access", strings.NewReader(`{"privacy":"private","joinRule":"invite"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("owner access %d", w.Code)
	}
}

func TestGroupJoinRules(t *testing.T) {
	db := newGroupMockDB()
	owner := "account:alice"
	bob := "account:bob"
	// open
	db.groups["conversation:open"] = groupView{ID: "conversation:open", Name: "Open", Privacy: "public", JoinRule: "open"}
	db.conversations["conversation:open"] = "group"
	db.members["conversation:open"] = map[string]string{owner: "owner"}
	// invite only
	db.groups["conversation:inv"] = groupView{ID: "conversation:inv", Name: "Inv", Privacy: "public", JoinRule: "invite"}
	db.conversations["conversation:inv"] = "group"
	db.members["conversation:inv"] = map[string]string{owner: "owner"}
	// password
	db.groups["conversation:pwd"] = groupView{ID: "conversation:pwd", Name: "Pwd", Privacy: "public", JoinRule: "password"}
	db.conversations["conversation:pwd"] = "group"
	db.members["conversation:pwd"] = map[string]string{owner: "owner"}
	hash, _ := hashGroupPassword("secret123")
	db.groupPass["conversation:pwd"] = hash
	// approval
	db.groups["conversation:appr"] = groupView{ID: "conversation:appr", Name: "Appr", Privacy: "public", JoinRule: "approval"}
	db.conversations["conversation:appr"] = "group"
	db.members["conversation:appr"] = map[string]string{owner: "owner"}
	_, handler := newGroupServer(db)
	// open join success
	req := httptest.NewRequest("POST", "/v1/groups/open/join", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("open join %d %s", w.Code, w.Body.String())
	}
	if db.members["conversation:open"][bob] == "" {
		t.Fatal("open join not added")
	}
	// idempotent second join
	req = httptest.NewRequest("POST", "/v1/groups/open/join", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("idempotent join %d", w.Code)
	}
	// invite required should be 403
	req = httptest.NewRequest("POST", "/v1/groups/inv/join", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("invite join should be 403 got %d", w.Code)
	}
	// password wrong
	req = httptest.NewRequest("POST", "/v1/groups/pwd/join", strings.NewReader(`{"password":"wrong"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("wrong password %d", w.Code)
	}
	// password correct
	req = httptest.NewRequest("POST", "/v1/groups/pwd/join", strings.NewReader(`{"password":"secret123"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("correct password %d %s", w.Code, w.Body.String())
	}
	// approval creates request 202/201
	req = httptest.NewRequest("POST", "/v1/groups/appr/join", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 202 && w.Code != 201 {
		t.Fatalf("approval join %d %s", w.Code, w.Body.String())
	}
	// second approval should be idempotent (same id)
	req = httptest.NewRequest("POST", "/v1/groups/appr/join", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 202 && w.Code != 201 {
		t.Fatalf("approval second %d", w.Code)
	}
}

func TestGroupInvitationsAndBlocked(t *testing.T) {
	db := newGroupMockDB()
	owner := "account:alice"
	bob := "account:bob"
	eve := "account:eve"
	db.groups["conversation:g1"] = groupView{ID: "conversation:g1", Name: "G1", Privacy: "public", JoinRule: "invite"}
	db.conversations["conversation:g1"] = "group"
	db.members["conversation:g1"] = map[string]string{owner: "owner"}
	// block eve
	pair := security.PairKey(owner, eve)
	db.blocked[pair] = true
	_, handler := newGroupServer(db)
	// blocked invite should be 403
	req := httptest.NewRequest("POST", "/v1/groups/g1/invitations", strings.NewReader(`{"userId":"`+eve+`"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("blocked invite should be 403 got %d %s", w.Code, w.Body.String())
	}
	// valid invite
	req = httptest.NewRequest("POST", "/v1/groups/g1/invitations", strings.NewReader(`{"userId":"`+bob+`"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("invite %d %s", w.Code, w.Body.String())
	}
	var invResp recordID
	_ = json.Unmarshal(w.Body.Bytes(), &invResp)
	// idempotent second invite should return same
	req = httptest.NewRequest("POST", "/v1/groups/g1/invitations", strings.NewReader(`{"userId":"`+bob+`"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("second invite %d", w.Code)
	}
	var invResp2 recordID
	_ = json.Unmarshal(w.Body.Bytes(), &invResp2)
	if invResp.ID != invResp2.ID {
		t.Fatalf("idempotent invite different %s vs %s", invResp.ID, invResp2.ID)
	}
	inviteID := strings.TrimPrefix(invResp.ID, "group_invitation:")
	// bob accepts
	req = httptest.NewRequest("POST", "/v1/groups/g1/invitations/"+inviteID+"/accept", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("accept %d %s", w.Code, w.Body.String())
	}
	if db.members["conversation:g1"][bob] == "" {
		t.Fatal("bob not member after accept")
	}
	// non-member cannot invite
	req = httptest.NewRequest("POST", "/v1/groups/g1/invitations", strings.NewReader(`{"userId":"`+eve+`"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	req.Header.Set("Content-Type", "application/json")
	// bob is member but not admin/owner -> should be 403
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("member invite should be 403 got %d", w.Code)
	}
}

func TestGroupJoinRequestLifecycle(t *testing.T) {
	db := newGroupMockDB()
	owner := "account:alice"
	bob := "account:bob"
	db.groups["conversation:g1"] = groupView{ID: "conversation:g1", Name: "G1", Privacy: "public", JoinRule: "approval"}
	db.conversations["conversation:g1"] = "group"
	db.members["conversation:g1"] = map[string]string{owner: "owner"}
	_, handler := newGroupServer(db)
	// bob sends join request
	req := httptest.NewRequest("POST", "/v1/groups/g1/join-requests", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("send join %d %s", w.Code, w.Body.String())
	}
	var jr recordID
	_ = json.Unmarshal(w.Body.Bytes(), &jr)
	joinID := strings.TrimPrefix(jr.ID, "group_join_request:")
	// owner approves
	req = httptest.NewRequest("POST", "/v1/groups/g1/join-requests/"+joinID+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("approve %d %s", w.Code, w.Body.String())
	}
	if db.members["conversation:g1"][bob] == "" {
		t.Fatal("bob not added after approve")
	}
	// bob cannot send again after member
	req = httptest.NewRequest("POST", "/v1/groups/g1/join-requests", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 && w.Code != 201 {
		t.Fatalf("member re-request should be no-op 204 got %d", w.Code)
	}
}

func TestGroupMembersAndOwnership(t *testing.T) {
	db := newGroupMockDB()
	owner := "account:alice"
	bob := "account:bob"
	carol := "account:carol"
	db.groups["conversation:g1"] = groupView{ID: "conversation:g1", Name: "G1", Privacy: "public", JoinRule: "open"}
	db.conversations["conversation:g1"] = "group"
	db.members["conversation:g1"] = map[string]string{owner: "owner", bob: "member", carol: "member"}
	_, handler := newGroupServer(db)
	// bob lists members
	req := httptest.NewRequest("GET", "/v1/groups/g1/members", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list %d %s", w.Code, w.Body.String())
	}
	// non-member cannot list
	req = httptest.NewRequest("GET", "/v1/groups/g1/members", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken("account:eve"))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("non-member list should be 403 got %d", w.Code)
	}
	// owner changes bob to admin
	req = httptest.NewRequest("PATCH", "/v1/groups/g1/members/bob/role", strings.NewReader(`{"role":"admin"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("change role %d %s", w.Code, w.Body.String())
	}
	if db.members["conversation:g1"][bob] != "admin" {
		t.Fatal("bob not admin")
	}
	// admin cannot remove owner
	req = httptest.NewRequest("DELETE", "/v1/groups/g1/members/alice", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("admin remove owner should be 403 got %d", w.Code)
	}
	// admin can remove member carol
	req = httptest.NewRequest("DELETE", "/v1/groups/g1/members/carol", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("admin remove member %d", w.Code)
	}
	if _, ok := db.members["conversation:g1"][carol]; ok {
		t.Fatal("carol not removed")
	}
	// owner transfers to bob
	req = httptest.NewRequest("POST", "/v1/groups/g1/ownership", strings.NewReader(`{"userId":"`+bob+`"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("transfer %d %s", w.Code, w.Body.String())
	}
	if db.members["conversation:g1"][bob] != "owner" || db.members["conversation:g1"][owner] != "admin" {
		t.Fatalf("ownership not transferred %#v", db.members["conversation:g1"])
	}
	// previous owner (now admin) can leave (owner now bob)
	req = httptest.NewRequest("POST", "/v1/groups/g1/leave", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("admin leave %d", w.Code)
	}
	// bob is sole owner now, cannot leave without transfer
	req = httptest.NewRequest("POST", "/v1/groups/g1/leave", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatalf("sole owner leave should be 409 got %d", w.Code)
	}
}

func TestGroupMessagesAndDeletionIntegration(t *testing.T) {
	db := newGroupMockDB()
	owner := "account:alice"
	bob := "account:bob"
	carol := "account:carol"
	group := "conversation:g1"
	db.groups[group] = groupView{ID: group, Name: "G1", Privacy: "public", JoinRule: "open"}
	db.conversations[group] = "group"
	db.members[group] = map[string]string{owner: "owner", bob: "member", carol: "member"}
	srv, handler := newGroupServer(db)
	srv.members.Set(group, []string{owner, bob, carol})
	// non-member eve cannot send
	req := httptest.NewRequest("POST", "/v1/conversations/g1/messages", strings.NewReader(`{"body":"hello","clientId":"client-12345678"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken("account:eve"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("non-member send should be 403 got %d %s", w.Code, w.Body.String())
	}
	// member can send
	req = httptest.NewRequest("POST", "/v1/conversations/g1/messages", strings.NewReader(`{"body":"hello group","clientId":"client-12345678"}`))
	req.Header.Set("Authorization", "Bearer "+groupToken(owner))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("group send %d %s", w.Code, w.Body.String())
	}
	var sent messageView
	_ = json.Unmarshal(w.Body.Bytes(), &sent)
	if sent.ID == "" {
		t.Fatal("no id")
	}
	// simulate persisted message for listing
	db2 := &groupMockDB{}
	_ = db2
	// Ensure pending visible to members, hidden not
	// Test push deduplication via mock pusher? Use srv with mock pusher
	// For now ensure list requires membership
	req = httptest.NewRequest("GET", "/v1/conversations/g1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("group list %d %s", w.Code, w.Body.String())
	}
	// Ensure non-member cannot list
	req = httptest.NewRequest("GET", "/v1/conversations/g1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken("account:eve"))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("non-member list should be 403 got %d", w.Code)
	}
	// Test deletion for group message: use existing deletion handler (needs message stored)
	// Insert message into mock for deletion test
	fakeMsg := "message:testgroup"
	// Use srv.pending for deletion target fallback? Insert into pending
	body := "to delete"
	srv.pending.TryAppend(group, messageView{ID: fakeMsg, Conversation: group, SenderID: owner, Body: &body, Kind: "text", CreatedAt: "2020-01-01T00:00:00Z"})
	// delete for me as bob (member) should hide only for bob
	req = httptest.NewRequest("DELETE", "/v1/messages/testgroup/for-me", nil)
	req.Header.Set("Authorization", "Bearer "+groupToken(bob))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("group delete for me %d %s", w.Code, w.Body.String())
	}
	// ensure pending hidden for bob, not for carol
	if !srv.pending.IsHidden(bob, fakeMsg) {
		t.Fatal("hidden not set")
	}
	if srv.pending.IsHidden(carol, fakeMsg) {
		t.Fatal("should not be hidden for carol")
	}
	_ = io.Discard
}

func TestGroupConcurrentJoinIdempotent(t *testing.T) {
	db := newGroupMockDB()
	owner := "account:alice"
	bob := "account:bob"
	db.groups["conversation:g1"] = groupView{ID: "conversation:g1", Name: "G1", Privacy: "public", JoinRule: "open"}
	db.conversations["conversation:g1"] = "group"
	db.members["conversation:g1"] = map[string]string{owner: "owner"}
	_, handler := newGroupServer(db)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/v1/groups/g1/join", nil)
			req.Header.Set("Authorization", "Bearer "+groupToken(bob))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 204 {
				t.Errorf("concurrent join %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if db.members["conversation:g1"][bob] != "member" {
		t.Fatal("bob should be member after concurrent joins")
	}
	// count members should be 2, not more
	if len(db.members["conversation:g1"]) != 2 {
		t.Fatalf("member count %d", len(db.members["conversation:g1"]))
	}
}
