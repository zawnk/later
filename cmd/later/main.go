package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/zawnk/later/internal/reminder"
)

type CLI struct {
	URL   string `env:"LATER_URL" default:"http://localhost:8080" help:"Base URL of the later server."`
	Token string `env:"LATER_TOKEN" help:"Bearer token (one of the server's auth_tokens)."`
	JSON  bool   `help:"Output machine-readable JSON on stdout instead of text (for jq/scripting)."`

	Create      CreateCmd      `cmd:"" default:"withargs" help:"Create a reminder from free text (no quotes needed). This is the default command."`
	List        ListCmd        `cmd:"" help:"List pending reminders."`
	Archive     ArchiveCmd     `cmd:"" help:"List fired (archived) reminders."`
	Next        NextCmd        `cmd:"" help:"Show the next upcoming reminder."`
	Last        LastCmd        `cmd:"" help:"Show the most recently fired reminder."`
	Cancel      CancelCmd      `cmd:"" help:"Cancel a pending reminder."`
	Postpone    PostponeCmd    `cmd:"" help:"Re-schedule a fired reminder."`
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
	Priority string   `short:"p" help:"Notification priority: min, low, default, high, urgent. Late reminders are bumped to at least high." enum:",min,low,default,high,urgent" default:""`
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
	fmt.Fprintf(a.out, "reminder %s set for %s: %s\n", rem.ID, formatTime(rem.DueAt), rem.Text)
	return nil
}

type ListCmd struct {
	By string `help:"Sort order: soonest due first, or creation order." enum:"due,create" default:"due"`
}

func (l *ListCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	reminders, err := cl.listPending()
	if err != nil {
		return err
	}

	switch l.By {
	case "create":
		slices.SortFunc(reminders, func(x, y reminder.Reminder) int { return x.CreatedAt.Compare(y.CreatedAt) })
	default:
		slices.SortFunc(reminders, func(x, y reminder.Reminder) int { return x.DueAt.Compare(y.DueAt) })
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
	for _, r := range reminders {
		fmt.Fprintf(a.out, "%s  due %s  %s\n", r.ID, formatTime(r.DueAt), r.Text)
	}
	return nil
}

type ArchiveCmd struct {
	Limit int `help:"Show only the N most recent entries (0 = all)." default:"20"`
}

func (c *ArchiveCmd) Run(a *app) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	archived, err := cl.listArchive()
	if err != nil {
		return err
	}

	total := len(archived)
	if c.Limit > 0 && total > c.Limit {
		archived = archived[total-c.Limit:]
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
		fmt.Fprintf(a.out, "%s  fired %s  %s\n", r.ID, formatTime(r.FiredAt), r.Text)
	}
	if len(archived) < total {
		fmt.Fprintf(a.out, "(showing %d of %d, use --limit to adjust)\n", len(archived), total)
	}
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
	Duration string `arg:"" help:"How far to push it from now, e.g. 1d, 2h30m."`
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
