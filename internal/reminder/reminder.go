package reminder

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type Reminder struct {
	ID             string    `json:"id"`
	Text           string    `json:"text"`
	DueAt          time.Time `json:"due_at"`
	CreatedAt      time.Time `json:"created_at"`
	OutboundTopics []string  `json:"outbound_topics"`
}

type ArchivedReminder struct {
	Reminder
	FiredAt time.Time `json:"fired_at"`
}

func GenerateID() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
