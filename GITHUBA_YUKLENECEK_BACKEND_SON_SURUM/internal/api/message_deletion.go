package api

import (
	"net/http"
)

func (s *Server) deleteMessageForMe(w http.ResponseWriter, r *http.Request) {
	target, store, ok := s.messageDeletionTarget(w, r)
	if !ok {
		return
	}
	if err := store.hide(r.Context(), accountID(r), target.ID); err != nil {
		s.databaseError(w, "hide message", err)
		return
	}
	s.pending.Hide(accountID(r), target.ID)
	s.publishConversation(r.Context(), target.Conversation)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteMessageForEveryone(w http.ResponseWriter, r *http.Request) {
	s.deleteMessageShared(w, r, "everyone")
}

func (s *Server) retractMessage(w http.ResponseWriter, r *http.Request) {
	s.deleteMessageShared(w, r, "retracted")
}

func (s *Server) deleteMessageShared(w http.ResponseWriter, r *http.Request, mode string) {
	target, store, ok := s.messageDeletionTarget(w, r)
	if !ok {
		return
	}
	if target.Sender != accountID(r) {
		respondError(w, http.StatusForbidden, "forbidden", "Bu mesajı silemezsiniz.")
		return
	}
	if err := s.withPersistenceLock(func() error {
		if err := store.mark(r.Context(), accountID(r), target.ID, mode); err != nil {
			return err
		}
		if mode == "everyone" {
			s.pending.DeleteEverywhere(target.ID)
		} else {
			s.pending.Retract(target.ID)
		}
		return nil
	}); err != nil {
		s.databaseError(w, "delete message", err)
		return
	}
	s.publishConversation(r.Context(), target.Conversation)
	w.WriteHeader(http.StatusNoContent)
}

type deletionTarget struct {
	Conversation string
	ID           string
	Sender       string
}

func (s *Server) messageDeletionTarget(w http.ResponseWriter, r *http.Request) (deletionTarget, *messageDeletionStore, bool) {
	message := normalizeRecordID(r.PathValue("id"), "message")
	if !validRecord(message, "message") {
		respondError(w, http.StatusBadRequest, "invalid_message", "Mesaj geçersiz.")
		return deletionTarget{}, nil, false
	}
	store := s.messageDeletion
	if store == nil {
		store = newMessageDeletionStore(s.db)
	}
	target, found, err := store.lookup(r.Context(), message)
	if err != nil {
		s.databaseError(w, "find message for deletion", err)
		return deletionTarget{}, nil, false
	}
	if !found {
		if pending, exists := s.pending.Find(message); exists {
			target = messageDeletionTarget{Conversation: pending.Conversation, Sender: pending.SenderID}
		} else {
			respondError(w, http.StatusNotFound, "message_not_found", "Mesaj bulunamadı.")
			return deletionTarget{}, nil, false
		}
	}
	member, err := s.isConversationMember(r.Context(), accountID(r), target.Conversation)
	if err != nil {
		s.databaseError(w, "verify message deletion membership", err)
		return deletionTarget{}, nil, false
	}
	if !member {
		respondError(w, http.StatusForbidden, "forbidden", "Mesaja erişiminiz yok.")
		return deletionTarget{}, nil, false
	}
	return deletionTarget{Conversation: target.Conversation, ID: message, Sender: target.Sender}, store, true
}

func (s *Server) withPersistenceLock(action func() error) error {
	if s.persist == nil {
		return action()
	}
	// ponytail: one writer already serializes queued messages; share its lock with deletion to prevent a queued write from restoring content.
	s.persist.mu.Lock()
	defer s.persist.mu.Unlock()
	return action()
}
