// Package calendar connects Mirrin to Google Calendar.
//
// Setup: connect Google from the Accounts page (or run `mirrin calendar
// login` with an OAuth client saved at ~/.mirrin/google-credentials.json).
// The sign-in is shared with Gmail and Drive through the google package, so
// the token is renewed once and saved once for all of them.
package calendar

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	gauth "github.com/MavrkAI/Mirrin/internal/skills/google"
	gcal "google.golang.org/api/calendar/v3"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// upcomingTTL is how long the next few events are kept for screens, which
// ask every few seconds; the calendar's own tools always ask Google.
const upcomingTTL = time.Minute

// Client wraps the Calendar API.
type Client struct {
	cfg  config.Calendar
	loc  *time.Location
	auth *gauth.Auth

	mu       sync.Mutex
	upcoming []Event
	upN      int
	upAt     time.Time

	spans map[string][2]time.Time // event id → times, from the last Snapshot (span.go)
}

// New builds a client (lazy; nothing is contacted until a tool runs).
func New(cfg config.Calendar, loc *time.Location) *Client {
	if loc == nil {
		loc = time.Local
	}
	return &Client{cfg: cfg, loc: loc, auth: gauth.NewAuth(cfg)}
}

// Location is the time zone the client reads and shows times in.
func (c *Client) Location() *time.Location { return c.loc }

func (c *Client) service(ctx context.Context) (*gcal.Service, error) {
	opts, err := c.auth.ClientOptions()
	if err != nil {
		if errors.Is(err, gauth.ErrNotConnected) {
			return nil, errors.New("Google Calendar isn't connected yet. The owner can connect it from Accounts in the Mirrin menu (or run `mirrin calendar login`)")
		}
		return nil, c.auth.Explain("calendar", err)
	}
	return gcal.NewService(ctx, opts...)
}

// forget drops the cached upcoming events after the calendar changed.
func (c *Client) forget() {
	c.mu.Lock()
	c.upcoming, c.upAt = nil, time.Time{}
	c.mu.Unlock()
}

// Login runs the OAuth flow in the browser and saves the token.
func (c *Client) Login(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr().String())
	url, err := c.auth.BeginURL(redirect)
	if err != nil {
		return err
	}
	type result struct{ err error }
	done := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, gauth.ExplainCallback(e), http.StatusBadRequest)
			select {
			case done <- result{errors.New(gauth.ExplainCallback(e))}:
			default:
			}
			return
		}
		if q.Get("code") == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		err := c.auth.Finish(r.Context(), q.Get("state"), q.Get("code"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			fmt.Fprintln(w, "Connected to Google. You can close this tab.")
		}
		select {
		case done <- result{err}:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Println("Open this URL in your browser:\n\n" + url + "\n")
	select {
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		fmt.Println("Calendar authorised.")
		return nil
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for Google sign-in")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Tools returns list_events / create_event / delete_event.
func (c *Client) Tools() []tools.Tool {
	calID := c.cfg.CalendarID
	if calID == "" {
		calID = "primary"
	}
	return []tools.Tool{
		tools.New("list_events", "List calendar events in a time window.",
			tools.Schema(map[string]tools.Prop{
				"from": {Type: "string", Description: "Start, RFC3339 or 'YYYY-MM-DD' (default: now)"},
				"to":   {Type: "string", Description: "End, RFC3339 or 'YYYY-MM-DD' (default: from + 1 day)"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ From, To string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				from := time.Now()
				if in.From != "" {
					t, err := parseTime(in.From, c.loc)
					if err != nil {
						return "", err
					}
					from = t
				}
				to := from.Add(24 * time.Hour)
				if in.To != "" {
					t, err := parseTime(in.To, c.loc)
					if err != nil {
						return "", err
					}
					to = t
				}
				srv, err := c.service(ctx)
				if err != nil {
					return "", err
				}
				evs, err := srv.Events.List(calID).ShowDeleted(false).SingleEvents(true).
					TimeMin(from.Format(time.RFC3339)).TimeMax(to.Format(time.RFC3339)).
					OrderBy("startTime").MaxResults(50).Context(ctx).Do()
				if err != nil {
					return "", c.auth.Explain("calendar", err)
				}
				if len(evs.Items) == 0 {
					return "no events", nil
				}
				var b strings.Builder
				for _, e := range evs.Items {
					fmt.Fprintf(&b, "- %s | %s", c.when(e), e.Summary)
					if e.Location != "" {
						fmt.Fprintf(&b, " @ %s", e.Location)
					}
					fmt.Fprintf(&b, " (id %s)\n", e.Id)
				}
				return b.String(), nil
			}),
		tools.New("create_event", "Create a calendar event.",
			tools.Schema(map[string]tools.Prop{
				"title":       {Type: "string", Description: "Event title", Required: true},
				"start":       {Type: "string", Description: "Start, RFC3339 or 'YYYY-MM-DD HH:MM' in the user's timezone", Required: true},
				"end":         {Type: "string", Description: "End (default: start + 1 hour)"},
				"location":    {Type: "string", Description: "Location"},
				"description": {Type: "string", Description: "Notes"},
				"attendees":   {Type: "string", Description: "Comma-separated email addresses to invite"},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Title, Start, End, Location, Description, Attendees string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				start, err := parseTime(in.Start, c.loc)
				if err != nil {
					return "", err
				}
				end := start.Add(time.Hour)
				if in.End != "" {
					if end, err = parseTime(in.End, c.loc); err != nil {
						return "", err
					}
				}
				ev := &gcal.Event{
					Summary:     in.Title,
					Location:    in.Location,
					Description: in.Description,
					Start:       &gcal.EventDateTime{DateTime: start.Format(time.RFC3339)},
					End:         &gcal.EventDateTime{DateTime: end.Format(time.RFC3339)},
				}
				for _, a := range strings.Split(in.Attendees, ",") {
					if a = strings.TrimSpace(a); a != "" {
						ev.Attendees = append(ev.Attendees, &gcal.EventAttendee{Email: a})
					}
				}
				srv, err := c.service(ctx)
				if err != nil {
					return "", err
				}
				created, err := srv.Events.Insert(calID, ev).SendUpdates("all").Context(ctx).Do()
				if err != nil {
					return "", c.auth.Explain("calendar", err)
				}
				c.forget()
				return fmt.Sprintf("created %q %s (id %s)", created.Summary, c.when(created), created.Id), nil
			}),
		tools.New("update_event", "Move or edit a calendar event: new start/end, title, location or notes. Attendees are notified.",
			tools.Schema(map[string]tools.Prop{
				"id":          {Type: "string", Description: "Event id from list_events", Required: true},
				"start":       {Type: "string", Description: "New start, RFC3339 or 'YYYY-MM-DD HH:MM'"},
				"end":         {Type: "string", Description: "New end (default: keeps the original duration)"},
				"title":       {Type: "string", Description: "New title"},
				"location":    {Type: "string", Description: "New location"},
				"description": {Type: "string", Description: "New notes"},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID, Start, End, Title, Location, Description string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				srv, err := c.service(ctx)
				if err != nil {
					return "", err
				}
				ev, err := srv.Events.Get(calID, in.ID).Context(ctx).Do()
				if err != nil {
					return "", c.auth.Explain("calendar", err)
				}
				if in.Start != "" {
					start, err := parseTime(in.Start, c.loc)
					if err != nil {
						return "", err
					}
					dur := time.Hour
					if ev.Start != nil && ev.End != nil && ev.Start.DateTime != "" && ev.End.DateTime != "" {
						if st, e1 := time.Parse(time.RFC3339, ev.Start.DateTime); e1 == nil {
							if et, e2 := time.Parse(time.RFC3339, ev.End.DateTime); e2 == nil {
								dur = et.Sub(st)
							}
						}
					}
					end := start.Add(dur)
					if in.End != "" {
						if end, err = parseTime(in.End, c.loc); err != nil {
							return "", err
						}
					}
					ev.Start = &gcal.EventDateTime{DateTime: start.Format(time.RFC3339)}
					ev.End = &gcal.EventDateTime{DateTime: end.Format(time.RFC3339)}
				}
				if in.Title != "" {
					ev.Summary = in.Title
				}
				if in.Location != "" {
					ev.Location = in.Location
				}
				if in.Description != "" {
					ev.Description = in.Description
				}
				updated, err := srv.Events.Update(calID, in.ID, ev).SendUpdates("all").Context(ctx).Do()
				if err != nil {
					return "", c.auth.Explain("calendar", err)
				}
				c.forget()
				return fmt.Sprintf("updated %q → %s", updated.Summary, c.when(updated)), nil
			}),
		tools.New("delete_event", "Delete a calendar event by id.",
			tools.Schema(map[string]tools.Prop{"id": {Type: "string", Description: "Event id from list_events", Required: true}}), tools.RiskDangerous,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				srv, err := c.service(ctx)
				if err != nil {
					return "", err
				}
				if err := srv.Events.Delete(calID, in.ID).Context(ctx).Do(); err != nil {
					return "", c.auth.Explain("calendar", err)
				}
				c.forget()
				return "deleted", nil
			}),
	}
}

// Event is a structured upcoming event for screens.
type Event struct {
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	AllDay   bool      `json:"all_day"`
	Location string    `json:"location,omitempty"`
	// The rest is filled by Soon only (soon.go): screens never carry who a
	// meeting is with or what its invite says.
	ID          string     `json:"id,omitempty"`
	Attendees   []Attendee `json:"attendees,omitempty"`
	Description string     `json:"description,omitempty"` // the invite's notes: someone else's words
	Link        string     `json:"link,omitempty"`        // the video call, if any
}

// Upcoming returns the next n events from now. Screens ask every few
// seconds, so the answer is kept for a minute (and dropped when the
// calendar's own tools change something).
func (c *Client) Upcoming(ctx context.Context, n int) ([]Event, error) {
	c.mu.Lock()
	if !c.upAt.IsZero() && c.upN == n && time.Since(c.upAt) < upcomingTTL {
		out := append([]Event{}, c.upcoming...)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()
	srv, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	calID := c.cfg.CalendarID
	if calID == "" {
		calID = "primary"
	}
	now := time.Now()
	evs, err := srv.Events.List(calID).ShowDeleted(false).SingleEvents(true).
		TimeMin(now.Format(time.RFC3339)).TimeMax(now.Add(3 * 24 * time.Hour).Format(time.RFC3339)).
		OrderBy("startTime").MaxResults(int64(n)).Context(ctx).Do()
	if err != nil {
		return nil, c.auth.Explain("calendar", err)
	}
	out := []Event{}
	for _, e := range evs.Items {
		ev := Event{Title: e.Summary, Location: e.Location}
		if e.Start != nil && e.Start.Date != "" {
			ev.AllDay = true
			ev.Start, _ = time.ParseInLocation("2006-01-02", e.Start.Date, c.loc)
			ev.End = ev.Start.Add(24 * time.Hour)
		} else if e.Start != nil {
			ev.Start, _ = time.Parse(time.RFC3339, e.Start.DateTime)
			if e.End != nil {
				ev.End, _ = time.Parse(time.RFC3339, e.End.DateTime)
			}
		}
		out = append(out, ev)
	}
	c.mu.Lock()
	c.upcoming, c.upN, c.upAt = append([]Event(nil), out...), n, time.Now()
	c.mu.Unlock()
	return out, nil
}

// Name identifies the watch source.
func (c *Client) Name() string { return "calendar" }

// Snapshot lists the next 7 days of events keyed by event id, for change detection.
func (c *Client) Snapshot(ctx context.Context) (map[string]string, error) {
	srv, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	calID := c.cfg.CalendarID
	if calID == "" {
		calID = "primary"
	}
	now := time.Now()
	evs, err := srv.Events.List(calID).ShowDeleted(false).SingleEvents(true).
		TimeMin(now.Format(time.RFC3339)).TimeMax(now.Add(7 * 24 * time.Hour).Format(time.RFC3339)).
		OrderBy("startTime").MaxResults(200).Context(ctx).Do()
	if err != nil {
		return nil, c.auth.Explain("calendar", err)
	}
	out := map[string]string{}
	spans := map[string][2]time.Time{}
	for _, e := range evs.Items {
		addSpan(spans, e)
		desc := fmt.Sprintf("%s | %s", c.when(e), e.Summary)
		if e.Location != "" {
			desc += " @ " + e.Location
		}
		if e.Status == "cancelled" {
			desc += " (cancelled)"
		}
		out[e.Id] = desc
	}
	c.keepSpans(spans)
	return out, nil
}

func (c *Client) when(e *gcal.Event) string {
	if e.Start == nil {
		return "?"
	}
	if e.Start.Date != "" {
		return e.Start.Date + " (all day)"
	}
	st, err := time.Parse(time.RFC3339, e.Start.DateTime)
	if err != nil {
		return e.Start.DateTime
	}
	out := st.In(c.loc).Format("Mon 2 Jan 15:04")
	if e.End != nil && e.End.DateTime != "" {
		if et, err := time.Parse(time.RFC3339, e.End.DateTime); err == nil {
			out += "–" + et.In(c.loc).Format("15:04")
		}
	}
	return out
}

func parseTime(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("could not parse time %q", s)
}
