package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/kong"
	"github.com/zawnk/later/internal/reminder"
)

var version = "dev"

type CLI struct {
	Version kong.VersionFlag `help:"Print version and exit."`
	URL     string           `env:"LATER_URL" default:"http://localhost:8080" help:"Base URL of the later server."`
	Token   string           `env:"LATER_TOKEN" help:"Bearer token (one of the server's auth_tokens)."`
	JSON    bool             `help:"Output machine-readable JSON on stdout instead of text (for jq/scripting)."`

	Create      CreateCmd      `cmd:"" default:"withargs" help:"Create a reminder from free text (no quotes needed). This is the default command."`
	List        ListCmd        `cmd:"" help:"List pending reminders."`
	Archive     ArchiveCmd     `cmd:"" help:"List fired (archived) reminders."`
	Search      SearchCmd      `cmd:"" help:"Search reminder text."`
	Next        NextCmd        `cmd:"" help:"Show the next upcoming reminder."`
	Last        LastCmd        `cmd:"" help:"Show the most recently fired reminder."`
	Cancel      CancelCmd      `cmd:"" help:"Cancel a pending reminder."`
	Postpone    PostponeCmd    `cmd:"" help:"Re-schedule a fired reminder."`
	Test        TestCmd        `cmd:"" help:"Diagnostic commands that don't create or change anything."`
	Healthcheck HealthcheckCmd `cmd:"" help:"Probe the server's /healthz; exit 0 if healthy. Needs no token -- usable as a Docker HEALTHCHECK."`
}

type app struct {
	out        io.Writer
	url, token string
	json       bool
	stdin      io.Reader
}

func (a *app) printJSON(v any) error {
	return json.NewEncoder(a.out).Encode(v)
}

func (a *app) client() (*client, error) {
	if a.token == "" {
		return nil, errors.New("no token configured: set LATER_TOKEN or add token=... to ~/.config/later/config (required for everything except healthcheck)")
	}
	return newClient(a.url, a.token), nil
}

func main() {
	resolver, err := configFileResolver()
	if err != nil {
		fmt.Fprintf(os.Stderr, "later: error: %v\n", err)
		os.Exit(1)
	}

	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("later"),
		kong.Description("Reminders via ntfy. Free text creates a reminder: later in 3d buy milk"),
		kong.UsageOnError(),
		kong.Resolvers(resolver),
		kong.Vars{"version": version},
	)

	var stdin io.Reader
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		stdin = os.Stdin
	}

	err = ctx.Run(&app{
		out:   os.Stdout,
		url:   cli.URL,
		token: cli.Token,
		json:  cli.JSON,
		stdin: stdin,
	})
	ctx.FatalIfErrorf(err)
}

type CreateCmd struct {
	Text     []string `arg:"" optional:"" help:"Reminder text, e.g.: later in 3 hours water the plants. When omitted, text is read from piped stdin: echo \"in 3d call xyz\" | later"`
	Topic    []string `help:"Outbound topic(s) for this reminder, repeatable or comma-separated. Default: the token's default_outbound if configured, otherwise all the token's outbound topics." placeholder:"TOPIC"`
	Tag      []string `help:"ntfy tag(s) for the notification (emoji shortcodes like partying_face, or plain labels), repeatable or comma-separated." placeholder:"TAG"`
	Priority string   `short:"p" help:"Notification priority: min, low, default, high, urgent, max. Late reminders are bumped to at least high." enum:",min,low,default,high,urgent,max" default:""`
	Click    string   `help:"URL the ntfy client opens when the notification is tapped." placeholder:"URL"`
}

func (c *CreateCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}

	text := strings.Join(c.Text, " ")
	if text == "" && a.stdin != nil {
		data, err := io.ReadAll(a.stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		text = strings.TrimSpace(string(data))
	}
	if text == "" {
		return errors.New(`no reminder text: pass it as arguments (later in 3d buy milk) or pipe it (echo "in 3d buy milk" | later)`)
	}

	rem, err := cl.create(createRequest{
		Text:     text,
		Topics:   c.Topic,
		Tags:     c.Tag,
		Priority: c.Priority,
		Click:    c.Click,
	})
	if err != nil {
		return err
	}
	saveLastID(rem.ID)
	if a.json {
		return a.printJSON(rem)
	}
	fmt.Fprintf(a.out, "set for %s: %s (%s)\n", formatTime(rem.DueAt), rem.Text, rem.ID)
	return nil
}

type TestCmd struct {
	Parse ParseCmd `cmd:"" help:"Preview how free text would be parsed - task text and resolved due time - without creating a reminder."`
}

type ParseCmd struct {
	Text []string `arg:"" help:"Text to preview, e.g.: later test parse tomorrow at 2am go to bed"`
}

func (p *ParseCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}

	text := strings.Join(p.Text, " ")
	preview, err := cl.parseTest(text)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(preview)
	}
	fmt.Fprintf(a.out, "%q -> %s\n", preview.Text, formatTime(preview.DueAt))
	return nil
}

type ListCmd struct {
	By      string `help:"Sort order: soonest due first, or creation order." enum:"due,create" default:"due"`
	Verbose bool   `short:"v" help:"Group by ETA and show priority/tags/topics."`
}

func (l *ListCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	reminders, err := cl.listPending(l.By)
	if err != nil {
		return err
	}

	if a.json {
		if reminders == nil {
			reminders = []reminder.Reminder{}
		}
		return a.printJSON(reminders)
	}
	if len(reminders) == 0 {
		fmt.Fprintln(a.out, "no pending reminders")
		return nil
	}
	printPendingEntries(a.out, reminders, l.By, l.Verbose)
	return nil
}

type ArchiveCmd struct {
	Limit   int  `help:"Show only the N most recent entries (0 = all)." default:"20"`
	Verbose bool `short:"v" help:"Also show each outbound topic and its ntfy message id."`
}

func (c *ArchiveCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	archived, total, err := cl.listArchive(c.Limit)
	if err != nil {
		return err
	}

	if a.json {
		if archived == nil {
			archived = []reminder.ArchivedReminder{}
		}
		return a.printJSON(archived)
	}
	if total == 0 {
		fmt.Fprintln(a.out, "no archived reminders")
		return nil
	}
	for _, r := range archived {
		printArchiveEntry(a.out, &r, c.Verbose)
	}
	if len(archived) < total {
		fmt.Fprintf(a.out, "(showing %d of %d, use --limit to adjust)\n", len(archived), total)
	}
	return nil
}

// printArchiveEntry prints one archived reminder in the CLI's plain text
// format, optionally followed by a tree of the topics it was sent to and
// their ntfy message ids.
func printArchiveEntry(w io.Writer, r *reminder.ArchivedReminder, verbose bool) {
	fmt.Fprintf(w, "%s  fired %s  %s\n", r.ID, formatTime(r.FiredAt), r.Text)
	if !verbose {
		return
	}
	for i, topic := range r.OutboundTopics {
		branch := "├──"
		if i == len(r.OutboundTopics)-1 {
			branch = "└──"
		}
		ntfyID, ok := r.NtfyMessageIDs[topic]
		if !ok {
			ntfyID = "unknown"
		}
		fmt.Fprintf(w, "    %s sent to %s (ntfy id: %s)\n", branch, topic, ntfyID)
	}
}

// printPendingEntries prints pending reminders, optionally (verbose) grouped
// into ETA buckets with aligned priority/tags/topics columns. by selects the
// bucketing dimension: "due" (default) buckets by DueAt, "create" buckets by
// CreatedAt. Item order within each bucket is preserved from the input.
func printPendingEntries(w io.Writer, reminders []reminder.Reminder, by string, verbose bool) {
	if !verbose {
		for _, r := range reminders {
			fmt.Fprintf(w, "%s  due %s  %s\n", r.ID, formatTime(r.DueAt), r.Text)
		}
		return
	}

	var buckets []pendingBucket
	if by == "create" {
		buckets = bucketByCreate(reminders)
	} else {
		buckets = bucketByDue(reminders)
	}

	showPriority, showTags, showTopics := false, false, false
	for _, r := range reminders {
		showPriority = showPriority || r.Priority != ""
		showTags = showTags || len(r.Tags) > 0
		showTopics = showTopics || len(r.OutboundTopics) > 0
	}

	idWidth, dueWidth, priorityWidth, tagsWidth, topicsWidth := 0, 0, 0, 0, 0
	rowsByBucket := make([][]pendingRow, len(buckets))
	for bi, b := range buckets {
		rows := make([]pendingRow, len(b.items))
		for i, r := range b.items {
			row := pendingRow{id: r.ID, due: dueCell(r), text: r.Text}
			if showPriority {
				row.priority = priorityCell(r)
			}
			if showTags {
				row.tags = tagsCell(r)
			}
			if showTopics {
				row.topics = topicsCell(r)
			}
			rows[i] = row

			idWidth = max(idWidth, utf8.RuneCountInString(row.id))
			dueWidth = max(dueWidth, utf8.RuneCountInString(row.due))
			priorityWidth = max(priorityWidth, utf8.RuneCountInString(row.priority))
			tagsWidth = max(tagsWidth, utf8.RuneCountInString(row.tags))
			topicsWidth = max(topicsWidth, utf8.RuneCountInString(row.topics))
		}
		rowsByBucket[bi] = rows
	}

	for bi, b := range buckets {
		if bi > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, b.label)
		for _, row := range rowsByBucket[bi] {
			fmt.Fprintf(w, "  %-*s  %-*s", idWidth, row.id, dueWidth, row.due)
			if showPriority {
				fmt.Fprintf(w, "  %-*s", priorityWidth, row.priority)
			}
			if showTags {
				fmt.Fprintf(w, "  %-*s", tagsWidth, row.tags)
			}
			if showTopics {
				fmt.Fprintf(w, "  %-*s", topicsWidth, row.topics)
			}
			fmt.Fprintf(w, "  %s\n", row.text)
		}
	}
}

type pendingBucket struct {
	label string
	items []reminder.Reminder
}

type pendingRow struct {
	id, due, priority, tags, topics, text string
}

// dayStart truncates t to local midnight, for calendar-day bucket math.
func dayStart(t time.Time) time.Time {
	t = t.Local()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// dueProximityLabel classifies r.DueAt as Overdue/Today/Tomorrow/Later
// relative to now, independent of which dimension the surrounding output is
// grouped by. This is used both for --by=due bucketing and, unconditionally,
// to decide the due column's rendering (dueCell) -- the due column always
// shows DueAt (decision 9), so its near/far classification must be based on
// DueAt itself, not on whatever bucket label the row happens to be grouped
// under when --by=create.
func dueProximityLabel(r reminder.Reminder, now time.Time) string {
	if r.DueAt.Before(now) {
		return "Overdue"
	}
	switch days := int(dayStart(r.DueAt).Sub(dayStart(now)).Hours() / 24); {
	case days == 0:
		return "Today"
	case days == 1:
		return "Tomorrow"
	default:
		return "Later"
	}
}

// bucketByDue partitions reminders into Overdue/Today/Tomorrow/Later buckets
// by DueAt, preserving input order within each bucket.
func bucketByDue(reminders []reminder.Reminder) []pendingBucket {
	now := time.Now()

	grouped := make(map[string][]reminder.Reminder, 4)
	for _, r := range reminders {
		label := dueProximityLabel(r, now)
		grouped[label] = append(grouped[label], r)
	}
	return orderedBuckets([]string{"Overdue", "Today", "Tomorrow", "Later"}, grouped)
}

// bucketByCreate partitions reminders into Today/Yesterday/Last week/Last
// month/Earlier buckets by CreatedAt, preserving input order within each
// bucket.
func bucketByCreate(reminders []reminder.Reminder) []pendingBucket {
	todayStart := dayStart(time.Now())

	grouped := make(map[string][]reminder.Reminder, 5)
	for _, r := range reminders {
		var label string
		switch days := int(todayStart.Sub(dayStart(r.CreatedAt)).Hours() / 24); {
		case days <= 0:
			label = "Today"
		case days == 1:
			label = "Yesterday"
		case days <= 7:
			label = "Last week"
		case days <= 30:
			label = "Last month"
		default:
			label = "Earlier"
		}
		grouped[label] = append(grouped[label], r)
	}
	return orderedBuckets([]string{"Today", "Yesterday", "Last week", "Last month", "Earlier"}, grouped)
}

// orderedBuckets returns non-empty buckets from grouped in the given label
// order, omitting any label with no entries.
func orderedBuckets(order []string, grouped map[string][]reminder.Reminder) []pendingBucket {
	buckets := make([]pendingBucket, 0, len(order))
	for _, label := range order {
		if items := grouped[label]; len(items) > 0 {
			buckets = append(buckets, pendingBucket{label: label, items: items})
		}
	}
	return buckets
}

// dueCell renders the due column. Today/Tomorrow are narrow, single-day-span
// buckets where a bare "weekday time" reading (no date) is still
// unambiguous; Overdue/Later are open-ended, so they use the full
// formatTime instead, since a weekday name alone doesn't say which week it
// falls in. This is always based on r.DueAt's own proximity to now, never on
// the row's grouping bucket -- when --by=create, the row's grouping label
// (e.g. "Yesterday") describes CreatedAt and says nothing about how near or
// far DueAt is.
func dueCell(r reminder.Reminder) string {
	switch dueProximityLabel(r, time.Now()) {
	case "Today", "Tomorrow":
		return r.DueAt.Local().Format("Mon 15:04")
	default:
		return formatTime(r.DueAt)
	}
}

func priorityCell(r reminder.Reminder) string {
	if r.Priority == "" {
		return "-"
	}
	return r.Priority
}

func tagsCell(r reminder.Reminder) string {
	if len(r.Tags) == 0 {
		return "-"
	}
	parts := make([]string, len(r.Tags))
	for i, t := range r.Tags {
		parts[i] = "#" + t
	}
	return strings.Join(parts, " ")
}

func topicsCell(r reminder.Reminder) string {
	if len(r.OutboundTopics) == 0 {
		return "-"
	}
	return "→" + strings.Join(r.OutboundTopics, ",")
}

type SearchCmd struct {
	Text    []string `arg:"" help:"Search query, no quotes needed. Matched as a case-insensitive substring against reminder text."`
	Pending bool     `help:"Search pending reminders (the default)." xor:"bucket"`
	Archive bool     `help:"Search archived reminders instead." xor:"bucket"`
	Verbose bool     `short:"v" help:"With --archive, also show each outbound topic and its ntfy message id. Otherwise, group pending results by ETA and show priority/tags/topics."`
}

func (c *SearchCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	query := strings.Join(c.Text, " ")

	if c.Archive {
		archived, err := cl.searchArchive(query)
		if err != nil {
			return err
		}
		if a.json {
			if archived == nil {
				archived = []reminder.ArchivedReminder{}
			}
			return a.printJSON(archived)
		}
		if len(archived) == 0 {
			fmt.Fprintln(a.out, "no archived reminders match")
			return nil
		}
		for _, r := range archived {
			printArchiveEntry(a.out, &r, c.Verbose)
		}
		return nil
	}

	reminders, err := cl.searchPending(query)
	if err != nil {
		return err
	}
	if a.json {
		if reminders == nil {
			reminders = []reminder.Reminder{}
		}
		return a.printJSON(reminders)
	}
	if len(reminders) == 0 {
		fmt.Fprintln(a.out, "no pending reminders match")
		return nil
	}
	printPendingEntries(a.out, reminders, "due", c.Verbose)
	return nil
}

type NextCmd struct{}

func (n *NextCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	rem, err := cl.next()
	if err != nil {
		if isNotFound(err) {
			if a.json {
				return a.printJSON(nil)
			}
			fmt.Fprintln(a.out, err.Error())
			return nil
		}
		return err
	}
	if a.json {
		return a.printJSON(rem)
	}
	fmt.Fprintf(a.out, "%s  due %s  %s\n", rem.ID, formatTime(rem.DueAt), rem.Text)
	return nil
}

type LastCmd struct{}

func (l *LastCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	rem, err := cl.last()
	if err != nil {
		if isNotFound(err) {
			if a.json {
				return a.printJSON(nil)
			}
			fmt.Fprintln(a.out, err.Error())
			return nil
		}
		return err
	}
	if a.json {
		return a.printJSON(rem)
	}
	fmt.Fprintf(a.out, "%s  fired %s  %s\n", rem.ID, formatTime(rem.FiredAt), rem.Text)
	return nil
}

type CancelCmd struct {
	ID string `arg:"" help:"Reminder id, or 'last' for the one most recently created by this CLI."`
}

func (c *CancelCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	id, err := resolveID(c.ID)
	if err != nil {
		return err
	}
	if err := cl.cancel(id); err != nil {
		return err
	}
	if a.json {
		return nil
	}
	fmt.Fprintf(a.out, "reminder %s cancelled\n", id)
	return nil
}

type PostponeCmd struct {
	ID       string `arg:"" help:"Reminder id, or 'last' for the one most recently created by this CLI."`
	Duration string `arg:"" help:"How far to push it from now: a compact duration (1d, 2h30m) or natural language (tomorrow morning, next monday)."`
}

func (p *PostponeCmd) Run(a *app) error {
	cl, err := a.client()

	if err != nil {
		return err
	}
	id, err := resolveID(p.ID)

	if err != nil {
		return err
	}
	rem, err := cl.postpone(id, p.Duration)

	if err != nil {
		return err
	}

	saveLastID(rem.ID)
	if a.json {
		return a.printJSON(rem)
	}
	fmt.Fprintf(a.out, "reminder %s postponed to %s as %s: %s\n", id, formatTime(rem.DueAt), rem.ID, rem.Text)
	return nil
}

type HealthcheckCmd struct{}

func (h *HealthcheckCmd) Run(a *app) error {
	httpc := &http.Client{Timeout: 3 * time.Second}
	resp, err := httpc.Get(strings.TrimRight(a.url, "/") + "/healthz")

	if err != nil {
		return fmt.Errorf("unhealthy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("unhealthy: /healthz returned %s", resp.Status)
	}
	return nil
}

func formatTime(t time.Time) string {
	return t.Local().Format("Mon 2006-01-02 15:04")
}
