package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var ErrBindingConflict = errors.New("chat is already bound to another thread")

type QueuedMessage struct {
	EventID   string    `json:"event_id"`
	Text      string    `json:"text"`
	Interrupt bool      `json:"interrupt,omitempty"`
	AddedAt   time.Time `json:"added_at"`
}

type persisted struct {
	ThreadToChat       map[string]string          `json:"thread_to_chat"`
	ChatToThread       map[string]string          `json:"chat_to_thread"`
	Events             map[string]bool            `json:"events"`
	Queues             map[string][]QueuedMessage `json:"queues"`
	HistoryInitialized map[string]bool            `json:"history_initialized"`
	ManagedChats       map[string]ManagedChat     `json:"managed_chats,omitempty"`
}

type ManagedChat struct {
	ChatID        string    `json:"chat_id"`
	LastActivity  time.Time `json:"last_activity"`
	DeletePending bool      `json:"delete_pending,omitempty"`
	DeleteStarted bool      `json:"delete_started,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	dir  string
	data persisted
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	store := &Store{dir: dir, data: persisted{
		ThreadToChat:       make(map[string]string),
		ChatToThread:       make(map[string]string),
		Events:             make(map[string]bool),
		Queues:             make(map[string][]QueuedMessage),
		HistoryInitialized: make(map[string]bool),
		ManagedChats:       make(map[string]ManagedChat),
	}}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &store.data); err != nil {
			return nil, fmt.Errorf("decode state: %w", err)
		}
		store.normalize()
	}
	return store, nil
}

func BindingCount(dir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var snapshot struct {
		ThreadToChat map[string]string `json:"thread_to_chat"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return 0, fmt.Errorf("decode state: %w", err)
	}
	return len(snapshot.ThreadToChat), nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) Bind(threadID, chatID string) error {
	return s.bind(threadID, chatID, false)
}

func (s *Store) BindManaged(threadID, chatID string) error {
	return s.bind(threadID, chatID, true)
}

func (s *Store) bind(threadID, chatID string, managed bool) error {
	if threadID == "" || chatID == "" {
		return errors.New("thread_id and chat_id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.data.ChatToThread[chatID]; exists && current != threadID {
		return ErrBindingConflict
	}
	previousChat, hadThread := s.data.ThreadToChat[threadID]
	previousThread, hadChat := s.data.ChatToThread[chatID]
	previousManaged, hadManaged := s.data.ManagedChats[threadID]
	if previousChat != "" && previousChat != chatID {
		delete(s.data.ChatToThread, previousChat)
	}
	s.data.ThreadToChat[threadID] = chatID
	s.data.ChatToThread[chatID] = threadID
	if managed {
		s.data.ManagedChats[threadID] = ManagedChat{ChatID: chatID}
	} else {
		delete(s.data.ManagedChats, threadID)
	}
	if err := s.saveLocked(); err != nil {
		if hadManaged {
			s.data.ManagedChats[threadID] = previousManaged
		} else {
			delete(s.data.ManagedChats, threadID)
		}
		if hadThread {
			s.data.ThreadToChat[threadID] = previousChat
		} else {
			delete(s.data.ThreadToChat, threadID)
		}
		if hadChat {
			s.data.ChatToThread[chatID] = previousThread
		} else {
			delete(s.data.ChatToThread, chatID)
		}
		if previousChat != "" && previousChat != chatID {
			s.data.ChatToThread[previousChat] = threadID
		}
		return err
	}
	return nil
}

func (s *Store) ThreadForChat(chatID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	threadID, ok := s.data.ChatToThread[chatID]
	return threadID, ok
}

func (s *Store) ChatForThread(threadID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chatID, ok := s.data.ThreadToChat[threadID]
	return chatID, ok
}

func (s *Store) Bindings() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	bindings := make(map[string]string, len(s.data.ThreadToChat))
	for threadID, chatID := range s.data.ThreadToChat {
		bindings[threadID] = chatID
	}
	return bindings
}

func (s *Store) HistoryInitialized(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.HistoryInitialized[threadID]
}

func (s *Store) MarkHistoryInitialized(threadID string) error {
	if threadID == "" {
		return errors.New("thread_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.HistoryInitialized[threadID] {
		return nil
	}
	s.data.HistoryInitialized[threadID] = true
	if err := s.saveLocked(); err != nil {
		delete(s.data.HistoryInitialized, threadID)
		return err
	}
	return nil
}

func (s *Store) InitializeHistory(threadID string, eventIDs []string) error {
	if threadID == "" {
		return errors.New("thread_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.HistoryInitialized[threadID] {
		return nil
	}
	added := make([]string, 0, len(eventIDs))
	for _, eventID := range eventIDs {
		if eventID == "" || s.data.Events[eventID] {
			continue
		}
		s.data.Events[eventID] = true
		added = append(added, eventID)
	}
	s.data.HistoryInitialized[threadID] = true
	if err := s.saveLocked(); err != nil {
		for _, eventID := range added {
			delete(s.data.Events, eventID)
		}
		delete(s.data.HistoryInitialized, threadID)
		return err
	}
	return nil
}

func (s *Store) MarkEvent(eventID string) (bool, error) {
	if eventID == "" {
		return false, errors.New("event_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Events[eventID] {
		return false, nil
	}
	s.data.Events[eventID] = true
	if err := s.saveLocked(); err != nil {
		delete(s.data.Events, eventID)
		return false, err
	}
	return true, nil
}

func (s *Store) HasEvent(eventID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Events[eventID]
}

func (s *Store) Enqueue(threadID string, message QueuedMessage) error {
	if threadID == "" || message.EventID == "" || message.Text == "" {
		return errors.New("thread_id, event_id and message text are required")
	}
	if message.AddedAt.IsZero() {
		message.AddedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.data.Queues[threadID]
	queued := make([]QueuedMessage, 0, len(previous)+1)
	queued = append(queued, previous...)
	queued = append(queued, message)
	s.data.Queues[threadID] = queued
	if err := s.saveLocked(); err != nil {
		if len(previous) == 0 {
			delete(s.data.Queues, threadID)
		} else {
			s.data.Queues[threadID] = previous
		}
		return err
	}
	return nil
}

func (s *Store) EnqueueOnce(threadID string, message QueuedMessage, prepend bool) (bool, error) {
	if threadID == "" || message.EventID == "" || message.Text == "" {
		return false, errors.New("thread_id, event_id and message text are required")
	}
	if message.AddedAt.IsZero() {
		message.AddedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Events[message.EventID] {
		return false, nil
	}
	previous := s.data.Queues[threadID]
	queued := make([]QueuedMessage, 0, len(previous)+1)
	if prepend {
		queued = append(queued, message)
	}
	queued = append(queued, previous...)
	if !prepend {
		queued = append(queued, message)
	}
	s.data.Events[message.EventID] = true
	s.data.Queues[threadID] = queued
	if err := s.saveLocked(); err != nil {
		delete(s.data.Events, message.EventID)
		if len(previous) == 0 {
			delete(s.data.Queues, threadID)
		} else {
			s.data.Queues[threadID] = previous
		}
		return false, err
	}
	return true, nil
}

func (s *Store) Queued(threadID string) ([]QueuedMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.data.Queues[threadID]
	return append([]QueuedMessage(nil), items...), nil
}

func (s *Store) QueuedThreads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	threads := make([]string, 0, len(s.data.Queues))
	for threadID, messages := range s.data.Queues {
		if len(messages) != 0 {
			threads = append(threads, threadID)
		}
	}
	sort.Strings(threads)
	return threads
}

func (s *Store) Dequeue(threadID string) ([]QueuedMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := append([]QueuedMessage(nil), s.data.Queues[threadID]...)
	delete(s.data.Queues, threadID)
	if err := s.saveLocked(); err != nil {
		s.data.Queues[threadID] = items
		return nil, err
	}
	return items, nil
}

func (s *Store) PrependQueue(threadID string, messages []QueuedMessage) error {
	if threadID == "" || len(messages) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.data.Queues[threadID]
	combined := make([]QueuedMessage, 0, len(messages)+len(previous))
	combined = append(combined, messages...)
	combined = append(combined, previous...)
	s.data.Queues[threadID] = combined
	if err := s.saveLocked(); err != nil {
		s.data.Queues[threadID] = previous
		return err
	}
	return nil
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(s.dir, "state.json"))
}

func (s *Store) normalize() {
	if s.data.ThreadToChat == nil {
		s.data.ThreadToChat = make(map[string]string)
	}
	if s.data.ChatToThread == nil {
		s.data.ChatToThread = make(map[string]string)
	}
	if s.data.Events == nil {
		s.data.Events = make(map[string]bool)
	}
	if s.data.Queues == nil {
		s.data.Queues = make(map[string][]QueuedMessage)
	}
	if s.data.HistoryInitialized == nil {
		s.data.HistoryInitialized = make(map[string]bool)
	}
	if s.data.ManagedChats == nil {
		s.data.ManagedChats = make(map[string]ManagedChat)
	}
}
