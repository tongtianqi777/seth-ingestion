package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Event announcements give calendar-building agents something to work with:
// school events with dates and times written the varied ways people write
// them, plus reminders, deadlines, reschedules and cancellations.
//
// Each one is also recorded in the simulated_events table, so a test can check
// what an agent extracted against what was actually announced.

// schoolEvent is a kind of event the school announces.
type schoolEvent struct {
	name    string       // as it appears mid-sentence, e.g. "the fall family picnic"
	hour    int          // start time; -1 for an all-day event
	minute  int          //
	weekday time.Weekday // the day it usually falls on; anyWeekday for Monday to Friday
	days    int          // how many days it lasts
	detail  string       // a closing sentence
	slips   bool         // needs permission slips, so it gets deadline reminders
	months  []time.Month // when it happens; empty for any time of year
	fixed   func(year int) time.Time
	// fixed, if set, gives the event's exact date in a year. Such events
	// (holidays) are never rescheduled or cancelled.
}

// thanksgivingBreak starts the Wednesday before the fourth Thursday of November.
func thanksgivingBreak(year int) time.Time {
	d := time.Date(year, time.November, 1, 0, 0, 0, 0, time.Local)
	for d.Weekday() != time.Thursday {
		d = d.AddDate(0, 0, 1)
	}
	return d.AddDate(0, 0, 21-1)
}

const anyWeekday time.Weekday = -1

var schoolEvents = []schoolEvent{
	{"the fall family picnic", 11, 0, time.Saturday, 1, "Bring a dish to share!", false, []time.Month{9, 10, 11}, nil},
	{"school picture day", -1, 0, time.Thursday, 1, "Please send your child in uniform.", false, nil, nil},
	{"parent-teacher conferences", -1, 0, anyWeekday, 2, "Sign up for a time slot at the front office.", false, nil, nil},
	{"chapel with a guest speaker", 9, 0, time.Wednesday, 1, "Parents are welcome to join.", false, nil, nil},
	{"the Christmas program", 18, 30, time.Friday, 1, "Doors open 30 minutes early.", false, []time.Month{12}, nil},
	{"the field trip to the pumpkin patch", 9, 30, time.Tuesday, 1, "Please pack a lunch and a water bottle.", true, []time.Month{10}, nil},
	{"the field trip to the science museum", 8, 45, time.Thursday, 1, "The bus leaves from the front gate.", true, nil, nil},
	{"the book fair", -1, 0, time.Monday, 3, "It's in the library during school hours.", false, nil, nil},
	{"the open house for new families", 10, 0, time.Saturday, 1, "Tell your friends!", false, nil, nil},
	{"a fire drill", 10, 0, anyWeekday, 1, "Please remind the kids it's only practice.", false, nil, nil},
	{"a teacher training day (no school)", -1, 0, time.Friday, 1, "Childcare isn't available that day.", false, nil, nil},
	{"the bake sale fundraiser", 8, 0, time.Friday, 1, "All proceeds go to the playground fund.", false, nil, nil},
	{"Thanksgiving break (no school)", -1, 0, time.Wednesday, 3, "Have a blessed holiday!", false, nil, thanksgivingBreak},
}

// eventFact is the ground truth behind one event announcement.
type eventFact struct {
	event       string
	action      string // scheduled | reminder | deadline | rescheduled | cancelled
	starts      time.Time
	allDay      bool
	days        int
	previous    time.Time // for rescheduled: the old start
	deadline    time.Time // for deadline: when the slips are due
	hasPrev     bool
	hasDeadline bool
}

// eventAnnouncement writes an announcement about an event, as if sent at
// `at`, and returns its text and what it means.
func (s *simulator) eventAnnouncement(at time.Time) (string, eventFact) {
	at = at.Local()
	action := "scheduled"
	switch n := s.rng.Intn(100); {
	case n < 12:
		action = "rescheduled"
	case n < 20:
		action = "cancelled"
	case n < 32:
		action = "deadline"
	case n < 52:
		action = "reminder"
	}
	minAhead, maxAhead := 3, 30
	if action == "reminder" {
		minAhead, maxAhead = 1, 5
	}
	ev, start := s.pickEvent(at, minAhead, maxAhead, action)
	f := eventFact{event: strings.TrimPrefix(strings.TrimPrefix(ev.name, "the "), "a "), action: action, starts: start, allDay: ev.hour < 0, days: ev.days}
	name := capitalize(ev.name)
	when := s.dateText(at, start)

	switch action {
	case "rescheduled":
		f.previous, f.hasPrev = start, true
		f.starts = s.eventDate(ev, start, 7+s.rng.Intn(10))
		moved := "has been moved"
		if is(ev.name) == "are" {
			moved = "have been moved"
		}
		return fmt.Sprintf("Update: %s "+moved+" from %s to %s%s. Sorry for any inconvenience!",
			ev.name, when, s.dateText(at, f.starts), s.timeText(start)), f
	case "cancelled":
		reason := "a scheduling conflict"
		switch {
		case strings.Contains(ev.name, "picnic"), strings.Contains(ev.name, "pumpkin"):
			reason = s.pick([]string{"the weather forecast", "rain in the forecast"})
		case strings.Contains(ev.name, "field trip"), strings.Contains(ev.name, "bake sale"):
			reason = s.pick([]string{"low sign-ups", "a scheduling conflict"})
		}
		cancelled := "is cancelled"
		if is(ev.name) == "are" {
			cancelled = "are cancelled"
		}
		return fmt.Sprintf("%s on %s %s because of %s. We'll share a new date soon.", name, when, cancelled, reason), f
	case "deadline":
		f.hasDeadline = true
		f.deadline = s.eventDate(schoolEvent{weekday: anyWeekday, hour: -1}, at, 1+s.rng.Intn(daysBetween(at, start)))
		if day := midnight(start); !f.deadline.Before(day) {
			f.deadline = day.AddDate(0, 0, -1) // slips are always due before the trip
		}
		return fmt.Sprintf("Permission slips for %s on %s are due by %s. Please return them to your child's teacher.",
			ev.name, when, s.dateText(at, f.deadline)), f
	case "reminder":
		return fmt.Sprintf("Reminder: %s %s %s%s. %s", ev.name, is(ev.name), when, s.timeText(start), ev.detail), f
	}
	if ev.days > 1 {
		return fmt.Sprintf("📅 %s: %s to %s. %s", name, when, s.dateText(at, start.AddDate(0, 0, ev.days-1)), ev.detail), f
	}
	tpl := s.pick([]string{
		"📅 %s " + is(ev.name) + " on %s%s. %s",
		"Save the date: %s, %s%s. %s",
		"Please mark your calendars! %s will be on %s%s. %s",
	})
	return fmt.Sprintf(tpl, name, when, s.timeText(start), ev.detail), f
}

// pickEvent picks an event that suits action and can happen between
// minAhead and maxAhead days after at, in its season, and returns its start.
func (s *simulator) pickEvent(at time.Time, minAhead, maxAhead int, action string) (schoolEvent, time.Time) {
	for try := 0; try < 50; try++ {
		ev := schoolEvents[s.rng.Intn(len(schoolEvents))]
		if action == "deadline" && !ev.slips || ev.fixed != nil && (action == "rescheduled" || action == "cancelled") {
			continue
		}
		if action == "cancelled" && (strings.Contains(ev.name, "fire drill") || strings.Contains(ev.name, "no school")) {
			continue // drills and days off don't get cancelled
		}
		var start time.Time
		if ev.fixed != nil {
			start = ev.fixed(at.Year())
		} else {
			start = s.eventDate(ev, at, minAhead+s.rng.Intn(maxAhead-minAhead+1))
		}
		if ahead := daysBetween(at, start); ahead < minAhead || ahead > maxAhead+6 {
			continue
		}
		if len(ev.months) > 0 && !containsMonth(ev.months, start.Month()) {
			continue
		}
		return ev, start
	}
	// Nothing seasonal fits: a fire drill can happen any time.
	drill := schoolEvents[0]
	for _, ev := range schoolEvents {
		if strings.Contains(ev.name, "fire drill") {
			drill = ev
		}
	}
	if action == "deadline" {
		for _, ev := range schoolEvents {
			if ev.slips && len(ev.months) == 0 {
				drill = ev
			}
		}
	}
	return drill, s.eventDate(drill, at, minAhead)
}

func containsMonth(months []time.Month, m time.Month) bool {
	for _, x := range months {
		if x == m {
			return true
		}
	}
	return false
}

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

// daysBetween counts calendar days from a's date to b's date.
func daysBetween(a, b time.Time) int {
	return int(midnight(b.Local()).Sub(midnight(a.Local())).Hours()/24 + 0.5)
}

// eventDate returns a date about `ahead` days after from, moved forward to the
// event's usual weekday, with the event's start time (midnight if all day).
func (s *simulator) eventDate(ev schoolEvent, from time.Time, ahead int) time.Time {
	d := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, ahead)
	for {
		wd := d.Weekday()
		if ev.weekday == anyWeekday && wd != time.Saturday && wd != time.Sunday || ev.weekday == wd {
			break
		}
		d = d.AddDate(0, 0, 1)
	}
	if ev.hour >= 0 {
		d = d.Add(time.Duration(ev.hour)*time.Hour + time.Duration(ev.minute)*time.Minute)
	}
	return d
}

// dateText writes d the way a school office might, relative to when the
// message was sent: "tomorrow", "this Friday", "Sat, Oct 25", "10/25", ….
func (s *simulator) dateText(sent, d time.Time) string {
	return dateText(sent, d, s.rng.Intn(6))
}

func dateText(sent, d time.Time, style int) string {
	sentDay := time.Date(sent.Year(), sent.Month(), sent.Day(), 0, 0, 0, 0, time.Local)
	day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.Local)
	ahead := int(day.Sub(sentDay).Hours()/24 + 0.5)
	switch {
	case ahead == 1 && style < 3:
		return "tomorrow"
	case ahead > 1 && ahead < 7 && day.Weekday() > sentDay.Weekday() && style == 0:
		return "this " + day.Weekday().String()
	}
	switch style % 5 {
	case 0:
		return day.Format("Monday, January 2")
	case 1:
		return day.Format("Mon, Jan 2")
	case 2:
		return day.Format("1/2")
	case 3:
		return day.Format("Jan 2")
	default:
		return day.Format("Monday 1/2/2006")
	}
}

// timeText writes the time of day of start, as " at 11am", " at 6:30pm" or
// " at 6:30 PM"; nothing for an all-day event (start at midnight).
func (s *simulator) timeText(start time.Time) string {
	if start.Hour() == 0 && start.Minute() == 0 {
		return ""
	}
	if s.rng.Intn(2) == 0 && start.Minute() == 0 {
		return " at " + strings.ToLower(start.Format("3PM"))
	}
	if s.rng.Intn(2) == 0 {
		return " at " + strings.ToLower(start.Format("3:04pm"))
	}
	return " at " + start.Format("3:04 PM")
}

// eventCorrection turns an edit of an event announcement into a corrected
// date, recorded as a reschedule. ok is false if t isn't about an event.
func (s *simulator) eventCorrection(ctx context.Context, chatJID, messageID string, at time.Time) (string, eventFact, bool) {
	var event, starts string
	var endsOn sql.NullString
	var allDay bool
	err := s.read.QueryRowContext(ctx, `SELECT event, starts_at, ends_on, all_day FROM simulated_events
		WHERE chat_jid = ? AND message_id = ? AND action IN ('scheduled', 'reminder', 'rescheduled')`, chatJID, messageID).Scan(&event, &starts, &endsOn, &allDay)
	if err != nil {
		return "", eventFact{}, false
	}
	var old time.Time
	if allDay {
		old, err = time.ParseInLocation("2006-01-02", starts, time.Local)
	} else {
		old, err = time.Parse(time.RFC3339, starts)
	}
	if err != nil {
		return "", eventFact{}, false
	}
	old = old.Local()
	days := 1
	if end, err := time.ParseInLocation("2006-01-02", endsOn.String, time.Local); err == nil {
		days = daysBetween(old, end) + 1
	}
	moved := old.AddDate(0, 0, 1+s.rng.Intn(7))
	f := eventFact{event: event, action: "rescheduled", starts: moved, allDay: allDay, days: days, previous: old, hasPrev: true}
	text := fmt.Sprintf("Correction: %s %s now on %s%s.", event, is(event), s.dateText(at, moved), s.timeText(moved))
	if days > 1 {
		text = fmt.Sprintf("Correction: %s %s now %s to %s.", event, is(event), s.dateText(at, moved), s.dateText(at, moved.AddDate(0, 0, days-1)))
	}
	return text, f, true
}

// is gives the verb for an event name: "conferences are", "the picnic is".
func is(name string) string {
	if strings.Contains(name, "conferences") {
		return "are"
	}
	return "is"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

const eventsSchema = `
CREATE TABLE IF NOT EXISTS simulated_events (
	chat_jid           TEXT NOT NULL,
	message_id         TEXT NOT NULL,
	event              TEXT NOT NULL,
	action             TEXT NOT NULL,
	starts_at          TEXT NOT NULL,
	ends_on            TEXT,
	all_day            INTEGER NOT NULL,
	previous_starts_at TEXT,
	deadline_on        TEXT,
	PRIMARY KEY (chat_jid, message_id)
)`

// recordEvent stores the ground truth for an event announcement.
func recordEvent(ctx context.Context, conn *sql.DB, chatJID, messageID string, f eventFact) error {
	format := func(t time.Time) string {
		if f.allDay {
			return t.Format("2006-01-02")
		}
		return t.Format(time.RFC3339)
	}
	var endsOn, previous, deadline any
	if f.days > 1 {
		endsOn = f.starts.AddDate(0, 0, f.days-1).Format("2006-01-02")
	}
	if f.hasPrev {
		previous = format(f.previous)
	}
	if f.hasDeadline {
		deadline = f.deadline.Format("2006-01-02")
	}
	_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO simulated_events
		(chat_jid, message_id, event, action, starts_at, ends_on, all_day, previous_starts_at, deadline_on)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		chatJID, messageID, f.event, f.action, format(f.starts), endsOn, f.allDay, previous, deadline)
	return err
}
