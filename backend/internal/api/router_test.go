package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoutesPreserveV1Contract(t *testing.T) {
	routes := (&Server{}).routes()
	for _, route := range []struct {
		method  string
		path    string
		pattern string
	}{
		{"GET", "/health", "GET /health"},
		{"POST", "/v1/auth/signup/request", "POST /v1/auth/signup/request"},
		{"POST", "/v1/auth/signup/verify", "POST /v1/auth/signup/verify"},
		{"POST", "/v1/auth/login/request", "POST /v1/auth/login/request"},
		{"POST", "/v1/auth/login/verify", "POST /v1/auth/login/verify"},
		{"POST", "/v1/auth/refresh", "POST /v1/auth/refresh"},
		{"POST", "/v1/auth/logout", "POST /v1/auth/logout"},
		{"GET", "/v1/me", "GET /v1/me"},
		{"PATCH", "/v1/me", "PATCH /v1/me"},
		{"DELETE", "/v1/me", "DELETE /v1/me"},
		{"GET", "/v1/me/privacy", "GET /v1/me/privacy"},
		{"PATCH", "/v1/me/privacy", "PATCH /v1/me/privacy"},
		{"GET", "/v1/users/search", "GET /v1/users/search"},
		{"POST", "/v1/friends/requests", "POST /v1/friends/requests"},
		{"GET", "/v1/friends/requests", "GET /v1/friends/requests"},
		{"POST", "/v1/friends/requests/request/respond", "POST /v1/friends/requests/{id}/respond"},
		{"DELETE", "/v1/friends/account", "DELETE /v1/friends/{id}"},
		{"POST", "/v1/users/account/block", "POST /v1/users/{id}/block"},
		{"DELETE", "/v1/users/account/block", "DELETE /v1/users/{id}/block"},
		{"GET", "/v1/me/blocked-users", "GET /v1/me/blocked-users"},
		{"GET", "/v1/conversations", "GET /v1/conversations"},
		{"POST", "/v1/conversations/direct", "POST /v1/conversations/direct"},
		{"GET", "/v1/conversations/conversation/messages", "GET /v1/conversations/{id}/messages"},
		{"POST", "/v1/conversations/conversation/messages", "POST /v1/conversations/{id}/messages"},
		{"POST", "/v1/conversations/conversation/read", "POST /v1/conversations/{id}/read"},
		{"POST", "/v1/conversations/conversation/delivered", "POST /v1/conversations/{id}/delivered"},
		{"PUT", "/v1/messages/message/reactions", "PUT /v1/messages/{id}/reactions"},
		{"DELETE", "/v1/messages/message/reactions/heart", "DELETE /v1/messages/{id}/reactions/{emoji}"},
		{"PUT", "/v1/messages/message/saved", "PUT /v1/messages/{id}/saved"},
		{"DELETE", "/v1/messages/message/saved", "DELETE /v1/messages/{id}/saved"},
		{"POST", "/v1/messages/message/receipt", "POST /v1/messages/{id}/receipt"},
		{"DELETE", "/v1/messages/message/for-me", "DELETE /v1/messages/{id}/for-me"},
		{"DELETE", "/v1/messages/message/for-everyone", "DELETE /v1/messages/{id}/for-everyone"},
		{"POST", "/v1/messages/message/retract", "POST /v1/messages/{id}/retract"},
		{"POST", "/v1/groups", "POST /v1/groups"},
		{"GET", "/v1/groups/search", "GET /v1/groups/search"},
		{"PATCH", "/v1/groups/conversation/name", "PATCH /v1/groups/{id}/name"},
		{"PATCH", "/v1/groups/conversation/description", "PATCH /v1/groups/{id}/description"},
		{"PATCH", "/v1/groups/conversation/image", "PATCH /v1/groups/{id}/image"},
		{"PATCH", "/v1/groups/conversation/access", "PATCH /v1/groups/{id}/access"},
		{"POST", "/v1/groups/conversation/join", "POST /v1/groups/{id}/join"},
		{"POST", "/v1/groups/conversation/invitations", "POST /v1/groups/{id}/invitations"},
		{"POST", "/v1/groups/conversation/invitations/group_invitation/accept", "POST /v1/groups/{id}/invitations/{invitationId}/accept"},
		{"POST", "/v1/groups/conversation/invitations/group_invitation/reject", "POST /v1/groups/{id}/invitations/{invitationId}/reject"},
		{"DELETE", "/v1/groups/conversation/invitations/group_invitation", "DELETE /v1/groups/{id}/invitations/{invitationId}"},
		{"POST", "/v1/groups/conversation/join-requests", "POST /v1/groups/{id}/join-requests"},
		{"POST", "/v1/groups/conversation/join-requests/group_join_request/approve", "POST /v1/groups/{id}/join-requests/{requestId}/approve"},
		{"POST", "/v1/groups/conversation/join-requests/group_join_request/reject", "POST /v1/groups/{id}/join-requests/{requestId}/reject"},
		{"DELETE", "/v1/groups/conversation/join-requests/group_join_request", "DELETE /v1/groups/{id}/join-requests/{requestId}"},
		{"GET", "/v1/groups/conversation/members", "GET /v1/groups/{id}/members"},
		{"POST", "/v1/groups/conversation/leave", "POST /v1/groups/{id}/leave"},
		{"DELETE", "/v1/groups/conversation/members/account", "DELETE /v1/groups/{id}/members/{userId}"},
		{"POST", "/v1/groups/conversation/ownership", "POST /v1/groups/{id}/ownership"},
		{"PATCH", "/v1/groups/conversation/members/account/role", "PATCH /v1/groups/{id}/members/{userId}/role"},
		{"PUT", "/v1/me/push-devices", "PUT /v1/me/push-devices"},
		{"DELETE", "/v1/me/push-devices/device", "DELETE /v1/me/push-devices/{deviceId}"},
		{"GET", "/v1/events/messages", "GET /v1/events/messages"},
		{"GET", "/v1/events/stream", "GET /v1/events/stream"},
	} {
		t.Run(route.pattern, func(t *testing.T) {
			request := httptest.NewRequest(route.method, route.path, nil)
			_, pattern := routes.Handler(request)
			if pattern != route.pattern {
				t.Fatalf("%s %s matched %q, want %q", route.method, route.path, pattern, route.pattern)
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/not-a-route", nil)
	_, pattern := routes.Handler(request)
	if pattern != "" {
		t.Fatalf("unknown route matched %q", pattern)
	}
}
