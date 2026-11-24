package handler

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jdmc43/kelma-sync/internal/studyday"
)

// Reviews per study day, for Kelma's review streak, and the day each card was
// first known (first answered into review, i.e. graduated), for the streak of
// new words. Rebuilt from the review history (the revlog), so it covers every
// past day and any client:
//
//   - due:     cards due when the day began (overdue ones included)
//   - arrived: of those, the cards that came due that day
//   - done:    of those, the cards answered that day
//   - reviews: answers that day (any card)
//
// A card's due day after its last answer of a day is that day plus the answer's
// interval (negative intervals are seconds: learning steps). A card suspended or
// deleted since still counts the days it was answered (started as a new card, or
// answered when due: suspending or deleting a card never undoes a day's work), and
// as handled the day it was suspended or deleted when it was due or new then; it
// isn't counted otherwise. Answers undone (retracted) don't count. A day
// starts at the account's study-day policy (time zone and hour), else at 4 am
// at offset_minutes from UTC (Anki's default day start).
//
// With groups (POST, {"offset_minutes", "groups": {"German": ["Meine Wörter"]}},
// a group per language with its decks, subdecks included), each group's days
// as Kelma Review serves them:
//
//   - new_served: new cards it serves: first, up to the decks' new-card limits
//     (the deck's own, and the group deck's over all of it); they also count
//     against the review limit (Kelma Review's default)
//   - served:     reviews it serves: due cards within what is left of the decks'
//     daily review limits
//   - done, new_done: due cards answered and new cards started that day
//
// Only decks the learner reviews count: decks answered in the 14 days up to the
// day. Limits are the decks' synced limits (newLimit, reviewLimit; Kelma
// Review writes them), else Kelma Review's defaults (20 new, 200 reviews).
//
// GET  /v2/summary/review-days?offset_minutes=
// POST /v2/summary/review-days {"offset_minutes": n, "groups": {name: [deck, ...]}}
type reviewDayRow struct {
	Day     string `json:"day"`
	Due     int64  `json:"due"`
	Arrived int64  `json:"arrived"`
	Done    int64  `json:"done"`
	Reviews int64  `json:"reviews"`
}

type firstKnownRow struct {
	CardID int64  `json:"card_id"`
	Day    string `json:"day"`
}

type reviewGroupDay struct {
	Day       string `json:"day"`
	Due       int64  `json:"due"`
	Served    int64  `json:"served"`
	Done      int64  `json:"done"`
	NewServed int64  `json:"new_served"`
	NewDone   int64  `json:"new_done"`
	Reviews   int64  `json:"reviews"`
}

type reviewGroup struct {
	Name        string           `json:"name"`
	Decks       []string         `json:"decks"`
	ReviewLimit int64            `json:"review_limit"`
	NewLimit    int64            `json:"new_limit"`
	Days        []reviewGroupDay `json:"days"`
}

type reviewDaysResponse struct {
	TimezoneID   string          `json:"timezone_id"`
	DayStartHour int             `json:"day_start_hour"`
	Today        string          `json:"today"`
	Days         []reviewDayRow  `json:"days"`
	FirstKnown   []firstKnownRow `json:"first_known"`
	Groups       []reviewGroup   `json:"groups,omitempty"`
}

type reviewDaysRequest struct {
	OffsetMinutes json.Number         `json:"offset_minutes"`
	Groups        map[string][]string `json:"groups"`
}

const (
	reviewDaysWindow        = 400
	reviewDaysActiveDays    = 14
	reviewDaysMaxGroups     = 20
	reviewDaysMaxGroupDecks = 50
	defaultNewLimit         = 20
	defaultReviewLimit      = 200
)

func (h *Handler) GetReviewDaysSummary(w http.ResponseWriter, r *http.Request) {
	offset, ok := reviewDaysOffset(w, r.URL.Query().Get("offset_minutes"))
	if !ok {
		return
	}
	h.reviewDaysSummary(w, r, offset, nil)
}

func (h *Handler) PostReviewDaysSummary(w http.ResponseWriter, r *http.Request) {
	var request reviewDaysRequest
	if err := decode(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body must be {offset_minutes, groups}")
		return
	}
	offset, ok := reviewDaysOffset(w, request.OffsetMinutes.String())
	if !ok {
		return
	}
	if len(request.Groups) > reviewDaysMaxGroups {
		writeError(w, http.StatusBadRequest, "bad_request", "at most 20 groups")
		return
	}
	for _, decks := range request.Groups {
		if len(decks) > reviewDaysMaxGroupDecks {
			writeError(w, http.StatusBadRequest, "bad_request", "at most 50 decks a group")
			return
		}
	}
	h.reviewDaysSummary(w, r, offset, request.Groups)
}

func reviewDaysOffset(w http.ResponseWriter, raw string) (int64, bool) {
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < -reviewTimeMaxOffsetMin || value > reviewTimeMaxOffsetMin {
		writeError(w, http.StatusBadRequest, "bad_request", "offset_minutes must be between -840 and 840")
		return 0, false
	}
	return value, true
}

// A deck's daily limits from its synced config, else Kelma Review's defaults.
func deckDailyLimits(config []byte) (newLimit, reviewLimit int64) {
	newLimit, reviewLimit = defaultNewLimit, defaultReviewLimit
	var parsed map[string]any
	if json.Unmarshal(config, &parsed) != nil {
		return
	}
	read := func(name string, fallback int64) int64 {
		value, ok := parsed[name].(float64)
		if !ok || value < 0 || value > 9999 || value != float64(int64(value)) {
			return fallback
		}
		return int64(value)
	}
	return read("newLimit", newLimit), read("reviewLimit", reviewLimit)
}

func (h *Handler) reviewDaysSummary(w http.ResponseWriter, r *http.Request, offset int64, groups map[string][]string) {
	claims := claimsFrom(r)
	policy, err := studyday.Get(r.Context(), h.DB, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	location := time.FixedZone("offset", int(offset)*60)
	dayStart := 4
	zoneName := ""
	if policy.Version > 0 {
		if loaded, err := time.LoadLocation(policy.TimezoneID); err == nil {
			location = loaded
			zoneName = policy.TimezoneID
			dayStart = policy.DayStartHour
		}
	}
	dayOf := func(ms int64) int64 {
		local := time.UnixMilli(ms).In(location).Add(-time.Duration(dayStart) * time.Hour)
		year, month, day := local.Date()
		return time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix() / 86_400
	}
	dayText := func(day int64) string { return time.Unix(day*86_400, 0).UTC().Format("2006-01-02") }
	today := dayOf(time.Now().UnixMilli())
	from := today - reviewDaysWindow
	size := int(today - from + 1)
	slot := func(day int64) int { return int(day - from) }

	// Decks and their daily limits.
	limits := map[string][2]int64{}
	deckRows, err := h.DB.Query(r.Context(), `SELECT name, config FROM decks WHERE user_id=$1`, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for deckRows.Next() {
		var name string
		var config []byte
		if err := deckRows.Scan(&name, &config); err != nil {
			deckRows.Close()
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		newLimit, reviewLimit := deckDailyLimits(config)
		limits[name] = [2]int64{newLimit, reviewLimit}
	}
	deckRows.Close()
	limitOf := func(deck string) (int64, int64) {
		if value, ok := limits[deck]; ok {
			return value[0], value[1]
		}
		return defaultNewLimit, defaultReviewLimit
	}
	// The limits each day had: a change records the limits before it
	// (deck_limit_history), so a day before a change keeps the old limits —
	// raising a limit today doesn't make yesterday short.
	type limitChange struct {
		day    int64 // the study day it was changed
		limits [2]int64
	}
	changes := map[string][]limitChange{}
	historyRows, err := h.DB.Query(r.Context(),
		`SELECT deck_name, limits, changed_at FROM deck_limit_history WHERE user_id=$1 ORDER BY changed_at, id`, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for historyRows.Next() {
		var name string
		var before []byte
		var changedAt time.Time
		if err := historyRows.Scan(&name, &before, &changedAt); err != nil {
			historyRows.Close()
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		newLimit, reviewLimit := deckDailyLimits(before)
		changes[name] = append(changes[name], limitChange{day: dayOf(changedAt.UnixMilli()), limits: [2]int64{newLimit, reviewLimit}})
	}
	historyRows.Close()
	if err := historyRows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// A day's limits: those before the first change made after that day, else the
	// current ones (a change during a day counts for it: its last setting).
	limitOn := func(deck string, day int64) (int64, int64) {
		for _, change := range changes[deck] {
			if change.day > day {
				return change.limits[0], change.limits[1]
			}
		}
		return limitOf(deck)
	}

	type cardKey struct {
		guid string
		ord  int
	}
	type cardInfo struct {
		id           int64
		suspended    bool
		suspendedDay int64 // the study day it was suspended or deleted (when known), else -1
		deleted      bool  // deleted since (no card row: from its answers and tombstone)
		deck         int
		created      int64 // study day, or from when not known
		answered     bool
		// Its schedule now as Kelma Review and the website have it: the FSRS
		// projection's due time (through review id), a due date set by hand (and
		// when), the review its schedule was reset through.
		projDue, projThrough, override, overrideAt, resetThrough int64
	}
	deckIndex := map[string]int{}
	deckNames := []string{}
	deckOf := func(name string) int {
		if index, ok := deckIndex[name]; ok {
			return index
		}
		deckIndex[name] = len(deckNames)
		deckNames = append(deckNames, name)
		return len(deckNames) - 1
	}
	cards := map[cardKey]*cardInfo{}
	cardRows, err := h.DB.Query(r.Context(),
		`SELECT note_guid, ord, card_id, study_state='suspended', deck_name, created_at,
		        LEAST(study_state_client_modified_at, study_state_modified_at),
		        CASE WHEN scheduling->'_kelma_fsrs'->>'due_at_millis' ~ '^[0-9]{1,15}$'
		             THEN (scheduling->'_kelma_fsrs'->>'due_at_millis')::bigint ELSE 0 END,
		        CASE WHEN scheduling->'_kelma_fsrs'->>'through_review_id' ~ '^[0-9]{1,15}$'
		             THEN (scheduling->'_kelma_fsrs'->>'through_review_id')::bigint ELSE 0 END,
		        due_date_override_ms,
		        (EXTRACT(EPOCH FROM due_date_override_client_modified_at) * 1000)::bigint,
		        schedule_reset_through_review_id
		 FROM cards WHERE user_id=$1`, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for cardRows.Next() {
		var key cardKey
		var info cardInfo
		var deck string
		var created, suspendedAt *time.Time
		if err := cardRows.Scan(&key.guid, &key.ord, &info.id, &info.suspended, &deck, &created, &suspendedAt,
			&info.projDue, &info.projThrough, &info.override, &info.overrideAt, &info.resetThrough); err != nil {
			cardRows.Close()
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		info.deck = deckOf(deck)
		info.created = from
		if created != nil {
			info.created = dayOf(created.UnixMilli())
		}
		info.suspendedDay = -1
		if info.suspended && suspendedAt != nil {
			info.suspendedDay = dayOf(suspendedAt.UnixMilli())
		}
		copied := info
		cards[key] = &copied
	}
	// Cards deleted since: the day each was deleted (tombstones that name the card).
	deletedOn := map[cardKey]int64{}
	graveRows, err := h.DB.Query(r.Context(),
		`SELECT note_guid, card_ord, deleted_at FROM tombstones
		 WHERE user_id=$1 AND type='card' AND note_guid IS NOT NULL AND card_ord IS NOT NULL`, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for graveRows.Next() {
		var key cardKey
		var deletedAt time.Time
		if err := graveRows.Scan(&key.guid, &key.ord, &deletedAt); err != nil {
			graveRows.Close()
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if day := dayOf(deletedAt.UnixMilli()); day > deletedOn[key] {
			deletedOn[key] = day
		}
	}
	graveRows.Close()
	if err := graveRows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	cardRows.Close()
	if err := cardRows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Per deck and day.
	type deckDays struct{ due, arrived, done, newAvail, newDone, answers []int64 }
	perDeck := map[int]*deckDays{}
	daysOf := func(deck int) *deckDays {
		if found, ok := perDeck[deck]; ok {
			return found
		}
		made := &deckDays{make([]int64, size), make([]int64, size), make([]int64, size), make([]int64, size), make([]int64, size), make([]int64, size)}
		perDeck[deck] = made
		return made
	}
	answers := make([]int64, size)
	firstKnown := []firstKnownRow{}
	newRange := func(deck *deckDays, start, end int64) {
		for day := max(start, from); day <= min(end, today); day++ {
			deck.newAvail[slot(day)]++
		}
	}

	type event struct {
		day      int64
		at       int64
		interval int
		answer   bool
		kind     int
		deck     string // the deck it was answered in (for a card deleted since)
	}
	var current cardKey
	var events []event
	flush := func() {
		defer func() { events = events[:0] }()
		info, exists := cards[current]
		if !exists {
			// Deleted since: from its answers (the deck of its last one), until the day
			// it was deleted (when a tombstone says), else through its last answer.
			if len(events) == 0 || events[len(events)-1].deck == "" {
				return
			}
			last := events[len(events)-1]
			info = &cardInfo{deck: deckOf(last.deck), created: events[0].day, deleted: true, suspendedDay: -1}
			if day, ok := deletedOn[current]; ok && day >= last.day {
				info.suspendedDay = day
			}
		}
		info.answered = true
		deck := daysOf(info.deck)
		// Gone (suspended or deleted since): only the days it was answered, up to the
		// day before (that day is handled below); nothing after.
		gone := info.suspended || info.deleted
		cut := today
		if gone {
			cut = info.suspendedDay - 1
			if info.deleted && info.suspendedDay < 0 {
				cut = events[len(events)-1].day
			}
		}
		// First known: its first answer into review — unless it was reset to new
		// since (forgotten: not known now).
		resetSince := false
		for _, e := range events {
			if e.answer {
				resetSince = false
			} else if e.kind == 4 && e.interval == 0 {
				resetSince = true
			}
		}
		for _, e := range events {
			if e.answer && e.interval > 0 && !resetSince && !info.deleted {
				firstKnown = append(firstKnown, firstKnownRow{CardID: info.id, Day: dayText(e.day)})
				break
			}
		}
		// New: from creation (or a reset) until its first answer. (A card gone since:
		// only the day it was started, before the day it went; that day: below.)
		isNew, newStart := true, info.created
		startedDay := int64(-1) // the day it was last started (first answer after being new)
		for _, e := range events {
			if e.answer && isNew {
				startedDay = e.day
				switch {
				case e.day > cut:
				case gone:
					if e.day >= from && e.day <= today {
						deck.newAvail[slot(e.day)]++
						deck.newDone[slot(e.day)]++
					}
				default:
					if e.day >= newStart {
						newRange(deck, newStart, e.day)
					}
					if e.day >= from && e.day <= today {
						deck.newDone[slot(e.day)]++
					}
				}
				isNew = false
			} else if !e.answer && e.kind == 4 && e.interval == 0 {
				isNew, newStart = true, e.day+1
			}
		}
		// Due: the last state each day it was answered.
		type dayState struct {
			day     int64
			dueDay  int64
			pending bool
			answer  bool // answered that day
		}
		states := []dayState{}
		for _, e := range events {
			state := dayState{day: e.day, pending: true, answer: e.answer}
			switch {
			case e.interval > 0:
				state.dueDay = e.day + int64(e.interval)
			case e.interval < 0:
				state.dueDay = dayOf(e.at + int64(-e.interval)*1000)
			case e.kind == 4:
				state.pending = false // reset to new
			default:
				state.dueDay = e.day
			}
			if n := len(states); n > 0 && states[n-1].day == e.day {
				state.answer = state.answer || states[n-1].answer
				states[n-1] = state
			} else {
				states = append(states, state)
			}
		}
		// Its current state as Kelma Review and the website serve it (they work the
		// due date out again from the whole history, with the current settings):
		// a schedule reset since its last answer makes it new; a due date set by
		// hand since then is its due date; else the FSRS projection's, when it is
		// up to date with its history. (Else its last answer's interval.)
		if n := len(states); n > 0 && states[n-1].pending {
			last := &states[n-1]
			lastAt := events[len(events)-1].at
			switch {
			case info.resetThrough > 0 && info.resetThrough >= lastAt:
				last.pending = false
			case info.override > 0 && lastAt < info.overrideAt:
				// (A due date set by hand is a day: its UTC midnight.)
				last.dueDay = info.override / 86_400_000
			case info.projDue > 0 && info.projThrough >= lastAt:
				last.dueDay = dayOf(info.projDue)
			}
		}
		// Due: each state until its next answer (or today). A card gone since: only the
		// days it was answered, before the day it went.
		if isNew && !gone {
			newRange(deck, newStart, today)
		}
		for i, state := range states {
			if !state.pending {
				continue
			}
			start := max(state.dueDay, state.day+1)
			end, answered := cut, false
			if i+1 < len(states) && states[i+1].day <= cut {
				end, answered = states[i+1].day, true
			}
			if start > end {
				// Answered before its due day here: Kelma Review and the website work
				// a card's due date out again from its whole history (FSRS, the
				// current settings), so they can serve it a day or so earlier. They
				// served it that day: it was due, and done.
				if answered && states[i+1].answer && end >= from && end <= today {
					deck.due[slot(end)]++
					deck.done[slot(end)]++
				}
				continue
			}
			if gone {
				if answered && states[i+1].answer && end >= from {
					if start == end {
						deck.arrived[slot(end)]++
					}
					deck.due[slot(end)]++
					deck.done[slot(end)]++
				}
				continue
			}
			if start >= from {
				deck.arrived[slot(start)]++
			}
			for day := max(start, from); day <= end; day++ {
				deck.due[slot(day)]++
				if answered && day == end {
					deck.done[slot(day)]++
				}
			}
		}
		// The day it was suspended or deleted: handled, when it was due or new that day
		// (Kelma Review had served it: suspending it mid-review uses up its place in
		// the day). After it: not counted.
		if !gone {
			return
		}
		sd := info.suspendedDay
		if sd < from || sd > today {
			return
		}
		if (isNew && sd >= newStart) || startedDay == sd {
			// New that day (or started that day): a new card handled.
			deck.newAvail[slot(sd)]++
			deck.newDone[slot(sd)]++
			return
		}
		for i, state := range states {
			if !state.pending || state.day >= sd {
				continue
			}
			if i+1 < len(states) && states[i+1].day < sd {
				continue
			}
			answeredThatDay := i+1 < len(states) && states[i+1].day == sd && states[i+1].answer
			if max(state.dueDay, state.day+1) <= sd || answeredThatDay {
				deck.due[slot(sd)]++
				deck.done[slot(sd)]++
			}
		}
	}

	rows, err := h.DB.Query(r.Context(),
		`SELECT note_guid, card_ord, review_id, interval, ease, review_kind, COALESCE(deck_name, '')
		 FROM reviews
		 WHERE user_id = $1
		   AND NOT EXISTS (SELECT 1 FROM review_retractions rr WHERE rr.user_id = reviews.user_id AND rr.review_id = reviews.review_id)
		 ORDER BY note_guid, card_ord, review_id`, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var key cardKey
		var reviewID int64
		var interval, ease, kind int
		var reviewDeck string
		if err := rows.Scan(&key.guid, &key.ord, &reviewID, &interval, &ease, &kind, &reviewDeck); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if first || key != current {
			if !first {
				flush()
			}
			current, first = key, false
		}
		day := dayOf(reviewID)
		answer := ease >= 1 && ease <= 4 && kind <= 3
		if answer && day >= from && day <= today {
			answers[slot(day)]++
			if info, ok := cards[key]; ok {
				daysOf(info.deck).answers[slot(day)]++
			} else if reviewDeck != "" {
				// A card deleted since: its answer still happened, in that deck.
				daysOf(deckOf(reviewDeck)).answers[slot(day)]++
			}
		}
		events = append(events, event{day: day, at: reviewID, interval: interval, answer: answer, kind: kind, deck: reviewDeck})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !first {
		flush()
	}
	// Cards never answered: new since they were made.
	for _, info := range cards {
		if info.answered {
			continue
		}
		if !info.suspended {
			newRange(daysOf(info.deck), info.created, today)
			continue
		}
		// A new card suspended since: handled the day it was suspended.
		deck := daysOf(info.deck)
		sd := info.suspendedDay
		if sd >= from && sd <= today && sd >= info.created {
			deck.newAvail[slot(sd)]++
			deck.newDone[slot(sd)]++
		}
	}

	response := reviewDaysResponse{TimezoneID: zoneName, DayStartHour: dayStart, Today: dayText(today), Days: []reviewDayRow{}, FirstKnown: firstKnown}
	for day := from; day <= today; day++ {
		var row reviewDayRow
		for _, deck := range perDeck {
			row.Due += deck.due[slot(day)]
			row.Arrived += deck.arrived[slot(day)]
			row.Done += deck.done[slot(day)]
		}
		row.Reviews = answers[slot(day)]
		if row.Due == 0 && row.Reviews == 0 {
			continue
		}
		row.Day = dayText(day)
		response.Days = append(response.Days, row)
	}

	// Groups: each language's decks as Kelma Review serves them.
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	under := func(deck, root string) bool { return deck == root || strings.HasPrefix(deck, root+"::") }
	for _, name := range names {
		// Roots: the group's decks, without those inside another of them.
		roots := []string{}
		for _, deck := range groups[name] {
			deck = strings.TrimSpace(deck)
			if deck == "" {
				continue
			}
			nested := false
			for _, other := range groups[name] {
				other = strings.TrimSpace(other)
				if other != "" && other != deck && under(deck, other) {
					nested = true
				}
			}
			if !nested && !containsString(roots, deck) {
				roots = append(roots, deck)
			}
		}
		group := reviewGroup{Name: name, Decks: []string{}, Days: []reviewGroupDay{}}
		members := map[string][]int{}
		for _, root := range roots {
			newLimit, reviewLimit := limitOf(root)
			group.NewLimit += newLimit
			group.ReviewLimit += reviewLimit
			for index, deck := range deckNames {
				if under(deck, root) {
					members[root] = append(members[root], index)
					group.Decks = append(group.Decks, deck)
				}
			}
		}
		sort.Strings(group.Decks)
		// Answers per deck, summed over the 14 days up to each day (active decks).
		active := func(deck *deckDays, day int64) bool {
			for d := max(day-reviewDaysActiveDays+1, from); d <= day; d++ {
				if deck.answers[slot(d)] > 0 {
					return true
				}
			}
			return false
		}
		for day := from; day <= today; day++ {
			var row reviewGroupDay
			for _, root := range roots {
				// As Kelma Review allots the day: new cards first (within the new
				// and review limits), then due reviews in what is left.
				rootNew, rootReview := limitOn(root, day)
				var served, newServed int64
				for _, index := range members[root] {
					deck, ok := perDeck[index]
					if !ok {
						continue
					}
					at := slot(day)
					row.Done += deck.done[at]
					row.NewDone += deck.newDone[at]
					row.Reviews += deck.answers[at]
					if !active(deck, day) {
						continue
					}
					deckNew, deckReview := limitOn(deckNames[index], day)
					row.Due += deck.due[at]
					deckNewServed := min(deckNew, deck.newAvail[at], deckReview)
					newServed += deckNewServed
					served += min(deck.due[at], deckReview-deckNewServed)
				}
				newServed = min(newServed, rootNew, rootReview)
				served = max(0, min(served, rootReview-newServed))
				row.Served += served
				row.NewServed += newServed
			}
			if row.Served == 0 && row.NewServed == 0 && row.Reviews == 0 && row.Done == 0 && row.NewDone == 0 {
				continue
			}
			row.Day = dayText(day)
			group.Days = append(group.Days, row)
		}
		response.Groups = append(response.Groups, group)
	}
	writeJSON(w, http.StatusOK, response)
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
