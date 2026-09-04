package api

import "context"

type groupView struct {
	Description string  `json:"description"`
	ID          string  `json:"id"`
	ImageURL    *string `json:"imageUrl"`
	JoinRule    string  `json:"joinRule"`
	Name        string  `json:"name"`
	Privacy     string  `json:"privacy"`
}

type groupMemberView struct {
	AvatarURL    *string `json:"avatarUrl"`
	DisplayName  string  `json:"displayName"`
	FullName     string  `json:"fullName"`
	ID           string  `json:"id"`
	IsPrivate    bool    `json:"isPrivate"`
	Relationship string  `json:"relationship"`
	Role         string  `json:"role"`
	Username     string  `json:"username"`
}

type groupStore struct {
	db queryer
}

func newGroupStore(db queryer) *groupStore {
	return &groupStore{db: db}
}

func groupConversationID(raw string) string {
	return normalizeRecordID(raw, "conversation")
}

func (s *groupStore) group(ctx context.Context, id string) (groupView, bool, error) {
	var groups []groupView
	err := s.db.Query(ctx, `SELECT <string>id AS id, group_name AS name, group_description AS description, group_image_url AS imageUrl, group_privacy AS privacy, group_join_rule AS joinRule FROM type::record($group) WHERE kind = 'group' LIMIT 1;`, map[string]any{"group": id}, &groups)
	if err != nil || len(groups) == 0 {
		return groupView{}, false, err
	}
	return groups[0], true, nil
}

func (s *groupStore) role(ctx context.Context, group, account string) (string, error) {
	var rows []struct {
		Role string `json:"role"`
	}
	err := s.db.Query(ctx, `SELECT role FROM conversation_member WHERE in = type::record($account) AND out = type::record($group) AND left_at IS NONE LIMIT 1;`, map[string]any{"account": account, "group": group}, &rows)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].Role, nil
}

func (s *groupStore) create(ctx context.Context, group, account string, view groupView, passwordHash string) error {
	rawID := view.ID
	if len(rawID) > len("conversation:") {
		rawID = rawID[len("conversation:"):]
	}
	return s.db.Query(ctx, `BEGIN TRANSACTION;
CREATE ONLY type::record($group) CONTENT {
	kind: 'group',
	group_id: $groupID,
	group_name: $name,
	group_description: $description,
	group_image_url: $image,
	group_privacy: $privacy,
	group_join_rule: $joinRule,
	group_password_hash: $passwordHash,
	created_by: type::record($account),
	created_at: time::now(),
	updated_at: time::now()
};
LET $account_record = type::record($account);
LET $group_record = type::record($group);
RELATE $account_record->conversation_member->$group_record CONTENT { role: 'owner', joined_at: time::now() };
COMMIT TRANSACTION;`, map[string]any{
		"group":        group,
		"groupID":      rawID,
		"name":         view.Name,
		"description":  view.Description,
		"image":        view.ImageURL,
		"privacy":      view.Privacy,
		"joinRule":     view.JoinRule,
		"passwordHash": passwordHash,
		"account":      account,
	}, nil)
}

func (s *groupStore) addMember(ctx context.Context, group, account, role string) error {
	return s.db.Query(ctx, `BEGIN TRANSACTION;
LET $existing = SELECT id FROM conversation_member WHERE in = type::record($account) AND out = type::record($group) AND left_at IS NONE LIMIT 1;
IF array::len($existing) = 0 {
	LET $account_record = type::record($account);
	LET $group_record = type::record($group);
	RELATE $account_record->conversation_member->$group_record CONTENT { role: $role, joined_at: time::now() };
};
COMMIT TRANSACTION;`, map[string]any{"group": group, "account": account, "role": role}, nil)
}

func (s *groupStore) update(ctx context.Context, group string, values map[string]any) error {
	return s.db.Query(ctx, `UPDATE type::record($group) SET
	group_name = IF $hasName THEN $name ELSE group_name END,
	group_description = IF $hasDescription THEN $description ELSE group_description END,
	group_image_url = IF $hasImage THEN $image ELSE group_image_url END,
	group_privacy = IF $hasPrivacy THEN $privacy ELSE group_privacy END,
	group_join_rule = IF $hasJoinRule THEN $joinRule ELSE group_join_rule END,
	group_password_hash = IF $hasPassword THEN $password ELSE group_password_hash END,
	updated_at = time::now()
	WHERE kind = 'group';`, values, nil)
}

func (s *groupStore) invitation(ctx context.Context, group, sender, recipient string) (string, error) {
	var existing []recordID
	err := s.db.Query(ctx, `SELECT <string>id AS id FROM group_invitation WHERE group = type::record($group) AND recipient = type::record($recipient) AND status = 'pending' LIMIT 1;`, map[string]any{"group": group, "recipient": recipient}, &existing)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return existing[0].ID, nil
	}
	var result []recordID
	err = s.db.Query(ctx, `LET $invite = CREATE group_invitation CONTENT { group: type::record($group), sender: type::record($sender), recipient: type::record($recipient), status: 'pending', created_at: time::now() }; RETURN [{ id: <string>$invite.id }];`, map[string]any{"group": group, "sender": sender, "recipient": recipient}, &result)
	if err != nil || len(result) == 0 {
		return "", err
	}
	return result[0].ID, nil
}

func (s *groupStore) joinRequest(ctx context.Context, group, account string) (string, error) {
	var existing []recordID
	err := s.db.Query(ctx, `SELECT <string>id AS id FROM group_join_request WHERE group = type::record($group) AND account = type::record($account) AND status = 'pending' LIMIT 1;`, map[string]any{"group": group, "account": account}, &existing)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return existing[0].ID, nil
	}
	var result []recordID
	err = s.db.Query(ctx, `LET $request = CREATE group_join_request CONTENT { group: type::record($group), account: type::record($account), status: 'pending', created_at: time::now() }; RETURN [{ id: <string>$request.id }];`, map[string]any{"group": group, "account": account}, &result)
	if err != nil || len(result) == 0 {
		return "", err
	}
	return result[0].ID, nil
}

func (s *groupStore) ensureSchema(ctx context.Context) error {
	// conversation and conversation_member are SCHEMAFULL in production. Keep
	// group-specific fields in the shared conversation model, but declare them
	// before any group request can use them. The role definition predates groups
	// and must be widened to accept the owner role.
	return s.db.Query(ctx, `
DEFINE FIELD IF NOT EXISTS group_id ON conversation TYPE none | string;
DEFINE FIELD IF NOT EXISTS group_name ON conversation TYPE none | string;
DEFINE FIELD IF NOT EXISTS group_description ON conversation TYPE none | string;
DEFINE FIELD IF NOT EXISTS group_image_url ON conversation TYPE none | string;
DEFINE FIELD IF NOT EXISTS group_privacy ON conversation TYPE none | string ASSERT $value = NONE OR $value INSIDE ['public', 'private'];
DEFINE FIELD IF NOT EXISTS group_join_rule ON conversation TYPE none | string ASSERT $value = NONE OR $value INSIDE ['open', 'invite', 'approval', 'password'];
DEFINE FIELD IF NOT EXISTS group_password_hash ON conversation TYPE none | string;
DEFINE FIELD OVERWRITE role ON conversation_member TYPE string DEFAULT 'member' ASSERT $value INSIDE ['member', 'admin', 'owner'];
DEFINE TABLE IF NOT EXISTS group_invitation SCHEMALESS;
DEFINE TABLE IF NOT EXISTS group_join_request SCHEMALESS;
`, nil, nil)
}

func startGroupSchema(db queryer) error {
	return newGroupStore(db).ensureSchema(context.Background())
}
