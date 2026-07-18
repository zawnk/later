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
	FiredAt time.Time `json:"fired_at"`
}

func GenerateID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}
