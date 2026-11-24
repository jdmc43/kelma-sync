// Package ankiimport reads bounded Anki deck packages without trusting their
// mutable scheduling projections.
package ankiimport

const (
	MaxPackageBytes   = 64 << 20
	MaxDatabaseBytes  = 128 << 20
	MaxMediaBytes     = 64 << 20
	MaxMediaFileBytes = 32 << 20
	MaxArchiveEntries = 65_535
)

type Collection struct {
	Decks     []string
	Notetypes []Notetype
	Notes     []Note
	Cards     []Card
	Reviews   []Review
	Media     []Media
	Warnings  []string
}

type Notetype struct {
	SourceID   int64
	Name       string
	Definition map[string]any
}

type Note struct {
	SourceID   int64
	GUID       string
	NotetypeID int64
	Fields     []string
	Tags       []string
}

type Card struct {
	SourceID     int64
	NoteSourceID int64
	DeckName     string
	Ordinal      int
}

type Review struct {
	ReviewID     int64
	CardSourceID int64
	Ease         int
	Interval     int
	LastInterval int
	Factor       int
	TakenMillis  int
	ReviewKind   int
}

type Media struct {
	SourceName string
	Filename   string
	Bytes      []byte
}
