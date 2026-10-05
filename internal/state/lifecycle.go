package state

import "time"

func (s *Store) ManagedChats() map[string]ManagedChat {
	s.mu.Lock()
	defer s.mu.Unlock()
	chats := make(map[string]ManagedChat, len(s.data.ManagedChats))
	for threadID, chat := range s.data.ManagedChats {
		chats[threadID] = chat
	}
	return chats
}

func (s *Store) RecordActivity(threadID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, ok := s.data.ManagedChats[threadID]
	if !ok || !at.After(chat.LastActivity) {
		return nil
	}
	previous := chat
	chat.LastActivity = at
	s.data.ManagedChats[threadID] = chat
	if err := s.saveLocked(); err != nil {
		s.data.ManagedChats[threadID] = previous
		return err
	}
	return nil
}

// Remember a successful remote deletion before unbinding. A failed save can
// retry without losing the binding or creating another group prematurely.
func (s *Store) MarkChatDeleted(threadID, chatID string) error {
	return s.setDeletion(threadID, chatID, true, true)
}

func (s *Store) BeginChatDeletion(threadID, chatID string) error {
	return s.setDeletion(threadID, chatID, true, false)
}

func (s *Store) CancelChatDeletion(threadID, chatID string) error {
	return s.setDeletion(threadID, chatID, false, false)
}

func (s *Store) setDeletion(threadID, chatID string, started, confirmed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, ok := s.data.ManagedChats[threadID]
	if !ok || chat.ChatID != chatID || s.data.ThreadToChat[threadID] != chatID {
		return ErrBindingConflict
	}
	previous := chat
	chat.DeleteStarted = started
	chat.DeletePending = confirmed
	s.data.ManagedChats[threadID] = chat
	if err := s.saveLocked(); err != nil {
		s.data.ManagedChats[threadID] = previous
		return err
	}
	return nil
}

func (s *Store) UnbindDeletedChat(threadID, chatID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, ok := s.data.ManagedChats[threadID]
	if !ok || !chat.DeletePending || chat.ChatID != chatID || s.data.ThreadToChat[threadID] != chatID {
		return ErrBindingConflict
	}
	delete(s.data.ThreadToChat, threadID)
	delete(s.data.ChatToThread, chatID)
	delete(s.data.ManagedChats, threadID)
	// Keep conversation history deduplication and old queues intact.
	if err := s.saveLocked(); err != nil {
		s.data.ThreadToChat[threadID] = chatID
		s.data.ChatToThread[chatID] = threadID
		s.data.ManagedChats[threadID] = chat
		return err
	}
	return nil
}
