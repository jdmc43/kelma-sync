package model

import "time"

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

type Client struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Label     string     `json:"label"`
	LastSeen  *time.Time `json:"last_seen"`
	CreatedAt time.Time  `json:"created_at"`
}

type Token struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	ClientID  string     `json:"client_id"`
	TokenHash string     `json:"-"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

type NoteMark struct {
	Marked           bool      `json:"marked"`
	IntentID         string    `json:"intent_id"`
	ModifiedAt       time.Time `json:"modified_at"`
	ClientModifiedAt time.Time `json:"client_modified_at"`
	LastClientID     string    `json:"last_client_id,omitempty"`
	LastClientLabel  string    `json:"last_client_label,omitempty"`
}

type Note struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	GUID             string    `json:"guid"`
	NotetypeID       int64     `json:"notetype_id"`
	Fields           []string  `json:"fields"`
	Tags             []string  `json:"tags"`
	Checksum         string    `json:"checksum"`
	ModifiedAt       time.Time `json:"modified_at"`
	ClientModifiedAt time.Time `json:"client_modified_at"`
	LastClientID     string    `json:"last_client_id"`
	LastClientLabel  string    `json:"last_client_label"`
	Mark             *NoteMark `json:"mark,omitempty"`
}

type SchedulingProjection struct {
	Status                 string `json:"status"`
	DesiredHistoryHash     string `json:"desired_history_hash"`
	DesiredThroughReviewID int64  `json:"desired_through_review_id"`
	DesiredProfileVersion  int64  `json:"desired_profile_version"`
	AppliedHistoryHash     string `json:"applied_history_hash,omitempty"`
	AppliedThroughReviewID *int64 `json:"applied_through_review_id,omitempty"`
	AppliedProfileVersion  *int64 `json:"applied_profile_version,omitempty"`
	LastErrorCode          string `json:"last_error_code,omitempty"`
}

type CardFlag struct {
	Flag             int       `json:"flag"`
	IntentID         string    `json:"intent_id"`
	ModifiedAt       time.Time `json:"modified_at"`
	ClientModifiedAt time.Time `json:"client_modified_at"`
	LastClientID     string    `json:"last_client_id,omitempty"`
	LastClientLabel  string    `json:"last_client_label,omitempty"`
}

type Card struct {
	ID                              string                `json:"id"`
	UserID                          string                `json:"user_id"`
	CardID                          int64                 `json:"card_id"`
	NoteGUID                        string                `json:"note_guid"`
	DeckName                        string                `json:"deck_name"`
	Ord                             int                   `json:"ord"`
	Scheduling                      map[string]any        `json:"scheduling"`
	CreatedAt                       *time.Time            `json:"created_at"`
	StudyState                      string                `json:"study_state"`
	StudyStateModifiedAt            time.Time             `json:"study_state_modified_at"`
	StudyStateClientModifiedAt      time.Time             `json:"study_state_client_modified_at"`
	StudyStateClientID              string                `json:"study_state_client_id,omitempty"`
	ScheduleResetThroughReviewID    int64                 `json:"schedule_reset_through_review_id"`
	ScheduleResetModifiedAt         time.Time             `json:"schedule_reset_modified_at"`
	ScheduleResetClientModifiedAt   time.Time             `json:"schedule_reset_client_modified_at"`
	ScheduleResetClientID           string                `json:"schedule_reset_client_id,omitempty"`
	DueDateOverrideMS               int64                 `json:"due_date_override_ms"`
	DueDateOverrideModifiedAt       time.Time             `json:"due_date_override_modified_at"`
	DueDateOverrideClientModifiedAt time.Time             `json:"due_date_override_client_modified_at"`
	DueDateOverrideClientID         string                `json:"due_date_override_client_id,omitempty"`
	Flag                            *CardFlag             `json:"flag,omitempty"`
	ModifiedAt                      time.Time             `json:"modified_at"`
	ClientModifiedAt                time.Time             `json:"client_modified_at"`
	LastClientID                    string                `json:"last_client_id"`
	LastClientLabel                 string                `json:"last_client_label"`
	Projection                      *SchedulingProjection `json:"projection,omitempty"`
}

type Notetype struct {
	ID               string         `json:"id"`
	UserID           string         `json:"user_id"`
	NotetypeID       int64          `json:"notetype_id"`
	Name             string         `json:"name"`
	Definition       map[string]any `json:"definition"`
	Checksum         string         `json:"checksum"`
	ModifiedAt       time.Time      `json:"modified_at"`
	ClientModifiedAt time.Time      `json:"client_modified_at"`
	LastClientID     string         `json:"last_client_id"`
	LastClientLabel  string         `json:"last_client_label"`
}

type Deck struct {
	ID               string         `json:"id"`
	UserID           string         `json:"user_id"`
	Name             string         `json:"name"`
	Config           map[string]any `json:"config"`
	Checksum         string         `json:"checksum"`
	ModifiedAt       time.Time      `json:"modified_at"`
	ClientModifiedAt time.Time      `json:"client_modified_at"`
	LastClientID     string         `json:"last_client_id"`
	LastClientLabel  string         `json:"last_client_label"`
}

// Review is an immutable Anki revlog row. Card ids are collection-local, so
// NoteGUID + CardOrd carry the portable card identity used when another client
// inserts the row into its own collection.
type Review struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	ReviewID        int64     `json:"review_id"`
	SourceCardID    int64     `json:"source_card_id"`
	NoteGUID        string    `json:"note_guid"`
	CardOrd         int       `json:"card_ord"`
	DeckName        string    `json:"deck_name"`
	Ease            int       `json:"ease"`
	Interval        int       `json:"interval"`
	LastInterval    int       `json:"last_interval"`
	Factor          int       `json:"factor"`
	TakenMillis     int       `json:"taken_millis"`
	ReviewKind      int       `json:"review_kind"`
	Checksum        string    `json:"checksum"`
	ModifiedAt      time.Time `json:"modified_at"`
	LastClientID    string    `json:"last_client_id"`
	LastClientLabel string    `json:"last_client_label"`
}

// StudyDay is a portable version of Anki's collection-relative deck counters.
// Day is an epoch-day (collection crt day + Anki scheduler day).
type StudyDay struct {
	ID                  string    `json:"id,omitempty"`
	UserID              string    `json:"user_id,omitempty"`
	Day                 int64     `json:"day"`
	DeckName            string    `json:"deck_name"`
	NewStudied          int       `json:"new_studied"`
	ReviewStudied       int       `json:"review_studied"`
	LearningStudied     int       `json:"learning_studied"`
	MillisecondsStudied int64     `json:"milliseconds_studied"`
	ModifiedAt          time.Time `json:"modified_at"`
	LastClientID        string    `json:"last_client_id,omitempty"`
	LastClientLabel     string    `json:"last_client_label,omitempty"`
}

type Media struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	Filename     string    `json:"filename"`
	SizeBytes    int64     `json:"size_bytes"`
	StorageKey   *string   `json:"storage_key"`
	RefCount     int       `json:"ref_count"`
	ModifiedAt   time.Time `json:"modified_at"`
	LastClientID string    `json:"last_client_id,omitempty"`
}

type Tombstone struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	Type         string    `json:"type"`
	ResourceID   string    `json:"resource_id"`
	DeletedAt    time.Time `json:"deleted_at"`
	LastClientID *string   `json:"last_client_id"`
	NoteGUID     string    `json:"note_guid,omitempty"`
	CardOrd      *int      `json:"card_ord,omitempty"`
}

// ManifestEntry is the lightweight summary returned by GET /v2/sync/manifest.
type ManifestEntry struct {
	GUID       string    `json:"guid,omitempty"`
	CardID     int64     `json:"card_id,omitempty"`
	ReviewID   int64     `json:"review_id,omitempty"`
	NotetypeID int64     `json:"notetype_id,omitempty"`
	Name       string    `json:"name,omitempty"`
	Filename   string    `json:"filename,omitempty"`
	SizeBytes  int64     `json:"size_bytes,omitempty"`
	Checksum   string    `json:"checksum,omitempty"`
	ModifiedAt time.Time `json:"modified_at"`
	// Source timestamp lets two-source clients resolve a strictly newer copy.
	// Card logical identity is also exposed so clients can compare scheduling
	// without a batch pull.
	NoteGUID                        string    `json:"note_guid,omitempty"`
	DeckName                        string    `json:"deck_name,omitempty"`
	Ord                             int       `json:"ord"`
	ClientModifiedAt                time.Time `json:"client_modified_at,omitempty"`
	StudyState                      string    `json:"study_state,omitempty"`
	StudyStateModifiedAt            time.Time `json:"study_state_modified_at,omitempty"`
	StudyStateClientModifiedAt      time.Time `json:"study_state_client_modified_at,omitempty"`
	ScheduleResetThroughReviewID    int64     `json:"schedule_reset_through_review_id,omitempty"`
	ScheduleResetModifiedAt         time.Time `json:"schedule_reset_modified_at,omitempty"`
	ScheduleResetClientModifiedAt   time.Time `json:"schedule_reset_client_modified_at,omitempty"`
	DueDateOverrideMS               int64     `json:"due_date_override_ms,omitempty"`
	DueDateOverrideModifiedAt       time.Time `json:"due_date_override_modified_at,omitempty"`
	DueDateOverrideClientModifiedAt time.Time `json:"due_date_override_client_modified_at,omitempty"`
	Flag                            *CardFlag `json:"flag,omitempty"`
	ProjectionStatus                string    `json:"projection_status,omitempty"`
	DesiredHistoryHash              string    `json:"desired_history_hash,omitempty"`
	DesiredThroughReviewID          int64     `json:"desired_through_review_id,omitempty"`
	DesiredProfileVersion           int64     `json:"desired_profile_version,omitempty"`
	AppliedHistoryHash              string    `json:"applied_history_hash,omitempty"`
	AppliedThroughReviewID          *int64    `json:"applied_through_review_id,omitempty"`
	AppliedProfileVersion           *int64    `json:"applied_profile_version,omitempty"`
	LastClientID                    string    `json:"last_client_id,omitempty"`
	Mark                            *NoteMark `json:"mark,omitempty"`
}

type ReviewManifestEntry struct {
	ReviewID     int64     `json:"review_id"`
	Checksum     string    `json:"checksum"`
	DeckName     string    `json:"deck_name,omitempty"`
	ModifiedAt   time.Time `json:"modified_at"`
	LastClientID string    `json:"last_client_id,omitempty"`
}

// ReviewRetraction is an append-only intent that makes an immutable review
// inactive without deleting or rewriting its history row.
type ReviewRetraction struct {
	ReviewID         int64     `json:"review_id"`
	IntentID         string    `json:"intent_id"`
	ClientModifiedAt time.Time `json:"client_modified_at"`
	ModifiedAt       time.Time `json:"modified_at"`
	LastClientID     string    `json:"last_client_id,omitempty"`
	LastClientLabel  string    `json:"last_client_label,omitempty"`
}

type Manifest struct {
	Capabilities      []string              `json:"capabilities,omitempty"`
	Notes             []ManifestEntry       `json:"notes"`
	Cards             []ManifestEntry       `json:"cards"`
	Reviews           []ReviewManifestEntry `json:"reviews"`
	ReviewRetractions []ReviewRetraction    `json:"review_retractions"`
	StudyDays         []StudyDay            `json:"study_days"`
	Notetypes         []ManifestEntry       `json:"notetypes"`
	Decks             []ManifestEntry       `json:"decks"`
	Media             []ManifestEntry       `json:"media"`
	Tombstones        []Tombstone           `json:"tombstones"`
	ServerTime        time.Time             `json:"server_time"`
}

// ConflictResponse is returned as 409 when a push conflicts.
type ConflictResponse struct {
	Error  string `json:"error"`
	Server any    `json:"server"`
	Client any    `json:"client"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}
