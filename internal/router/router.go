package router

import (
	"errors"
	"strings"

	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

type ActionKind string

const (
	Ignored   ActionKind = "ignored"
	Submit    ActionKind = "submit"
	Queued    ActionKind = "queued"
	Interrupt ActionKind = "interrupt"
)

type Incoming struct {
	EventID  string
	ChatID   string
	SenderID string
	Text     string
}

type Action struct {
	Kind     ActionKind
	ThreadID string
	Content  string
}

type Router struct {
	store       *state.Store
	ownerOpenID string
}

func New(store *state.Store, ownerOpenID string) *Router {
	return &Router{store: store, ownerOpenID: ownerOpenID}
}

func (r *Router) Route(message Incoming, busy bool) (Action, error) {
	if r.store == nil {
		return Action{}, errors.New("state store is required")
	}
	if message.EventID == "" || message.ChatID == "" || message.SenderID == "" || strings.TrimSpace(message.Text) == "" {
		return Action{Kind: Ignored}, nil
	}
	if r.ownerOpenID == "" || message.SenderID != r.ownerOpenID {
		return Action{Kind: Ignored}, nil
	}
	threadID, ok := r.store.ThreadForChat(message.ChatID)
	if !ok {
		return Action{Kind: Ignored}, nil
	}
	content := strings.TrimSpace(message.Text)
	interrupt := strings.HasPrefix(content, "/interrupt ")
	if interrupt {
		content = strings.TrimSpace(strings.TrimPrefix(content, "/interrupt "))
		if content == "" {
			return Action{Kind: Ignored}, nil
		}
	}
	if content == "/interrupt" {
		return Action{Kind: Ignored}, nil
	}
	if busy {
		queued := state.QueuedMessage{EventID: message.EventID, Text: content, Interrupt: interrupt}
		accepted, err := r.store.EnqueueOnce(threadID, queued, interrupt)
		if err != nil {
			return Action{}, err
		}
		if !accepted {
			return Action{Kind: Ignored}, nil
		}
		if interrupt {
			return Action{Kind: Interrupt, ThreadID: threadID, Content: content}, nil
		}
		return Action{Kind: Queued, ThreadID: threadID}, nil
	}
	accepted, err := r.store.MarkEvent(message.EventID)
	if err != nil {
		return Action{}, err
	}
	if !accepted {
		return Action{Kind: Ignored}, nil
	}
	if interrupt {
		return Action{Kind: Interrupt, ThreadID: threadID, Content: content}, nil
	}
	return Action{Kind: Submit, ThreadID: threadID, Content: content}, nil
}
