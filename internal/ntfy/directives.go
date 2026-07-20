package ntfy

import (
	"errors"
	"regexp"
	"strings"

	"github.com/zawnk/later/internal/reminder"
)

var ErrConflictingPriority = errors.New("multiple priority directives in message")

var tagTokenRegex = regexp.MustCompile(`^#[A-Za-z0-9_]+$`)

func parseDirectives(text string) (cleanText string, tags []string, priority string, err error) {
	tokens := strings.Fields(text)

	end := len(tokens)
	var collectedTags []string
	seenTags := make(map[string]struct{})
	seenPriority := false

loop:
	for end > 0 {
		tok := tokens[end-1]
		switch {
		case tagTokenRegex.MatchString(tok):
			tag := tok[1:]
			if _, dup := seenTags[tag]; !dup {
				seenTags[tag] = struct{}{}
				collectedTags = append(collectedTags, tag)
			}
			end--
		case strings.HasPrefix(tok, "!") && reminder.IsValidPriority(tok[1:]):
			if seenPriority {
				return "", nil, "", ErrConflictingPriority
			}
			priority = tok[1:]
			seenPriority = true
			end--
		default:
			break loop
		}
	}

	for i, j := 0, len(collectedTags)-1; i < j; i, j = i+1, j-1 {
		collectedTags[i], collectedTags[j] = collectedTags[j], collectedTags[i]
	}

	return strings.Join(tokens[:end], " "), collectedTags, priority, nil
}
