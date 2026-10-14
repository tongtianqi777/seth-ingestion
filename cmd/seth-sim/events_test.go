package main

import (
	"strings"
	"testing"
	"time"
)

func TestDateText(t *testing.T) {
	sent := time.Date(2026, 10, 14, 10, 0, 0, 0, time.Local) // a Wednesday
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 30, 0, 0, time.Local) }
	for _, c := range []struct {
		d     time.Time
		style int
		want  string
	}{
		{day(15), 0, "tomorrow"},
		{day(15), 4, "Thursday 10/15/2026"},
		{day(16), 0, "this Friday"},
		{day(19), 0, "Monday, October 19"}, // next week, so not "this Monday"
		{day(19), 1, "Mon, Oct 19"},
		{day(19), 2, "10/19"},
		{day(19), 3, "Oct 19"},
		{day(19), 5, "Monday, October 19"},
	} {
		if got := dateText(sent, c.d, c.style); got != c.want {
			t.Errorf("dateText(%s, style %d) = %q, want %q", c.d.Format("Jan 2"), c.style, got, c.want)
		}
	}
}

func TestThanksgivingBreak(t *testing.T) {
	for year, want := range map[int]string{2026: "2026-11-25", 2027: "2027-11-24", 2028: "2028-11-22"} {
		if got := thanksgivingBreak(year).Format("2006-01-02"); got != want {
			t.Errorf("thanksgivingBreak(%d) = %s, want %s", year, got, want)
		}
	}
}

// TestEventAnnouncementsMatchTheirFacts checks every simulated event post
// against its row in simulated_events: the date it states, and that the
// facts make sense together.
func TestEventAnnouncementsMatchTheirFacts(t *testing.T) {
	conn := seeded(t, testConfig(t, 11), 90, 30)
	rows, err := conn.Query(`
		SELECT m.message_type, m.body, m.sent_at, g.is_announcement, e.event, e.action, e.starts_at,
		       COALESCE(e.ends_on, ''), e.all_day, COALESCE(e.previous_starts_at, ''), COALESCE(e.deadline_on, '')
		FROM simulated_events e
		JOIN messages m USING (chat_jid, message_id)
		JOIN chat_groups g ON g.group_jid = m.chat_jid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actions := map[string]int{}
	parse := func(s string) time.Time {
		if d, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
			return d
		}
		d, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("bad time %q", s)
		}
		return d.Local()
	}
	mentions := func(body string, sent, d time.Time) bool {
		for style := 0; style < 6; style++ {
			if strings.Contains(body, dateText(sent, d, style)) {
				return true
			}
		}
		return false
	}
	for rows.Next() {
		var typ, body, sentAt, event, action, starts, endsOn, previous, deadline string
		var announcement, allDay bool
		if err := rows.Scan(&typ, &body, &sentAt, &announcement, &event, &action, &starts, &endsOn, &allDay, &previous, &deadline); err != nil {
			t.Fatal(err)
		}
		actions[action]++
		sent, start := parse(sentAt), parse(starts)
		if action == "cancelled" && (strings.Contains(event, "fire drill") || strings.Contains(event, "no school")) {
			t.Errorf("%q: %s can't be cancelled", body, event)
		}
		if !announcement {
			t.Errorf("event post outside Announcements: %q", body)
		}
		if !mentions(body, sent, start) {
			t.Errorf("%q doesn't state its date %s", body, starts)
		}
		if allDay != (len(starts) == len("2006-01-02")) {
			t.Errorf("%q: all_day = %v but starts_at = %s", body, allDay, starts)
		}
		statesTime := action == "scheduled" || action == "reminder" || action == "rescheduled"
		if statesTime && !allDay && !strings.Contains(strings.ToLower(body), strings.ToLower(start.Format("3"))) {
			t.Errorf("%q doesn't state its time %s", body, start.Format("3:04pm"))
		}
		ahead := daysBetween(sent, start)
		switch action {
		case "reminder":
			if ahead < 1 || ahead > 11 {
				t.Errorf("reminder %q is %d days ahead", body, ahead)
			}
		case "deadline":
			if d := parse(deadline); !d.Before(midnight(start)) || d.Before(midnight(sent)) {
				t.Errorf("%q: slips due %s, trip %s, sent %s", body, deadline, starts, sentAt)
			}
		case "rescheduled":
			if previous == "" || !mentions(body, sent, parse(previous)) && typ != "edit" {
				t.Errorf("%q: previous date %q missing", body, previous)
			}
		}
		if ahead < 1 {
			t.Errorf("%q: event %s isn't after it was announced (%s)", body, starts, sentAt)
		}
		if endsOn != "" && !parse(endsOn).After(start) {
			t.Errorf("%q: ends %s before it starts %s", body, endsOn, starts)
		}
		for _, ev := range schoolEvents {
			if strings.HasSuffix(ev.name, event) && len(ev.months) > 0 && !containsMonth(ev.months, start.Month()) && action != "rescheduled" {
				t.Errorf("%q: %s in %s, out of season", body, event, start.Month())
			}
		}
	}
	for _, a := range []string{"scheduled", "reminder", "deadline", "rescheduled", "cancelled"} {
		if actions[a] == 0 {
			t.Errorf("no %s event posts in 90 days: %v", a, actions)
		}
	}
	var announcements, total int
	conn.QueryRow(`SELECT COUNT(*) FROM messages m JOIN chat_groups g ON g.group_jid = m.chat_jid WHERE g.is_announcement = 1`).Scan(&announcements)
	conn.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&total)
	if events := actions["scheduled"] + actions["reminder"] + actions["deadline"] + actions["rescheduled"] + actions["cancelled"]; events*2 < announcements {
		t.Errorf("only %d of %d announcements are about events", events, announcements)
	}
	if announcements*12 < total {
		t.Errorf("only %d of %d messages are announcements", announcements, total)
	}
}
