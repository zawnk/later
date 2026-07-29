package reminder

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

type Reminder struct {
	ID             string    `json:"id"`
	Text           string    `json:"text"`
	DueAt          time.Time `json:"due_at"`
	CreatedAt      time.Time `json:"created_at"`
	OutboundTopics []string  `json:"outbound_topics"`
	Tags           []string  `json:"tags,omitempty"`
	Priority       string    `json:"priority,omitempty"`
	Click          string    `json:"click,omitempty"`
}

type ArchivedReminder struct {
	Reminder
	FiredAt        time.Time         `json:"fired_at"`
	NtfyMessageIDs map[string]string `json:"ntfy_message_ids,omitempty"`
}

func GenerateID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

var validPriorities = map[string]struct{}{
	"min": {}, "low": {}, "default": {}, "high": {}, "urgent": {}, "max": {},
	"1": {}, "2": {}, "3": {}, "4": {}, "5": {},
}

func IsValidPriority(p string) bool {
	_, ok := validPriorities[p]
	return ok
}

func DedupeStrings(items []string) []string {
	if len(items) == 0 {
		return items
	}
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, s := range items {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		result = append(result, s)
	}
	return result
}
