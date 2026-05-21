// Package main generates a fake WhatsApp msgstore.db with the same schema
// as a real WhatsApp msgstore.db but populated with synthetic data.
//
// The tool is fully self-contained: it requires no input database. Both
// the schema (via //go:embed schema.sql) and the distribution constants
// for row counts, JID types, message types and side-table probabilities
// are baked into the binary.
//
// Usage:
//
//	whatsapp-fakeit [-scale=N] <output.db>
//
// Where -scale multiplies every row count (default 1.0 produces a DB of
// roughly the same size as a real ~24MB msgstore.db; 0.1 -> ~2.4MB; 2.0
// -> ~50MB).
//
// The tool:
//  1. Creates a new database using the schema embedded in the binary.
//  2. Synthesises distributions for jids, chats, messages and ~12
//     side tables, preserving foreign-key consistency throughout.
//  3. Generates fake field values via gofakeit while honouring the
//     statistical shape of a real msgstore.db (message-type ratios,
//     JID-type ratios, per-message has-text/has-media probabilities,
//     from_me ratio, status distribution, etc.).
package main

import (
	"database/sql"
	_ "embed"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	_ "github.com/mattn/go-sqlite3"

	"whatsapp-fakeit/generators"
)

//go:embed schema.sql
var embeddedSchema string

// ---------------------------------------------------------------------------
// Globals
// ---------------------------------------------------------------------------

var (
	faker *gofakeit.Faker
	rng   *mrand.Rand

	// ID mapping: source_id -> fake_id for each table that's referenced.
	jidIDMap     = map[int64]int64{}
	chatIDMap    = map[int64]int64{}
	messageIDMap = map[int64]int64{}
	labelIDMap   = map[int64]int64{}

	// Distinct fake JIDs keyed by type so we can pick a sender appropriately.
	jidsByType = map[int64][]int64{}

	// One stable "me" jid for from_me=1 messages.
	meJIDID int64
)

// ---------------------------------------------------------------------------
// Source-sample structures
// ---------------------------------------------------------------------------

type jidSample struct {
	ID        int64
	User      string
	Server    string
	Agent     int64
	Device    int64
	Type      int64
	RawString string
}

type chatSample struct {
	ID          int64
	JIDRowID    int64
	Subject     string
	Created     int64
	SortTS      int64
	ChatOrigin  sql.NullString
	GroupType   int64
	JIDType     int64
	UnreadCount int64
}

type messageSample struct {
	ID            int64
	ChatRowID     int64
	FromMe        int64
	SenderJIDID   sql.NullInt64
	Status        int64
	Broadcast     sql.NullInt64
	Origin        sql.NullInt64
	OrigFlags     sql.NullInt64
	Timestamp     int64
	Received      sql.NullInt64
	MessageType   int64
	TextData      string
	Starred       sql.NullInt64
	SortID        int64
	HasText       bool
	HasMedia      bool
	HasQuoted     bool
	HasEphemeral  bool
	HasMentions   bool
	HasAddOnFlags sql.NullInt64
}

type sourceData struct {
	jids     []jidSample
	chats    []chatSample
	messages []messageSample

	// per-table row counts so we can match volume
	tableRowCount map[string]int64

	// primaryPhone is the digits-only (no '+') phone number to use for
	// the "me" jid (jid.user where type=0 / server=s.whatsapp.net).
	// Empty means: pick a random phone like every other jid.
	primaryPhone string

	// meJIDSourceID is the _id of the jid that represents "me". It is
	// always set after generateSyntheticData runs (to the first phone
	// jid if no -phone was given, or to the fixed ID assigned to the
	// primary phone if -phone was given).
	meJIDSourceID int64
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	scale := flag.Float64("scale", 1.0,
		"scale factor for row counts (1.0 ~= a real ~24MB msgstore.db, 0.1 ~= 2.4MB, 2.0 ~= 50MB)")
	seed := flag.Int64("seed", 0,
		"PRNG seed; 0 = use current time (non-reproducible)")
	phone := flag.String("phone", "",
		"primary phone number for the 'me' jid in E.164 format with leading '+' (e.g. +12025550123); empty = random")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-scale=N] [-seed=N] [-phone=+CC...] <output.db>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	if *scale <= 0 {
		fmt.Fprintln(os.Stderr, "error: -scale must be > 0")
		os.Exit(1)
	}
	primaryPhone, err := parsePrimaryPhone(*phone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: -phone %q: %v\n", *phone, err)
		os.Exit(1)
	}
	dstPath := flag.Arg(0)

	s := *seed
	if s == 0 {
		s = time.Now().UnixNano()
	}
	rng = mrand.New(mrand.NewSource(s))
	faker = gofakeit.NewFaker(rng, true)

	if primaryPhone != "" {
		log.Printf("Generating synthetic data (scale=%.2f, seed=%d, phone=+%s)...", *scale, s, primaryPhone)
	} else {
		log.Printf("Generating synthetic data (scale=%.2f, seed=%d)...", *scale, s)
	}
	data := generateSyntheticData(*scale, primaryPhone)

	log.Printf("  generated: %d jids, %d chats, %d messages",
		len(data.jids), len(data.chats), len(data.messages))

	log.Printf("Creating destination database: %s", dstPath)
	_ = os.Remove(dstPath)
	dst, err := sql.Open("sqlite3", dstPath)
	must(err)
	defer dst.Close()

	_, _ = dst.Exec("PRAGMA foreign_keys = OFF;")
	_, _ = dst.Exec("PRAGMA journal_mode = WAL;")
	_, _ = dst.Exec("PRAGMA synchronous = NORMAL;")

	log.Printf("Applying embedded schema (%d bytes)...", len(embeddedSchema))
	must(applyEmbeddedSchema(dst))

	log.Printf("Generating fake data...")
	tx, err := dst.Begin()
	must(err)

	must(genJIDs(tx, data))
	must(genChats(tx, data))
	must(genMessages(tx, data))
	must(genMessageText(tx, data))
	must(genMessageMedia(tx, data))
	must(genMessageQuoted(tx, data))
	must(genMessageEphemeral(tx, data))
	must(genMessageRevoked(tx, data))
	must(genMessageMentions(tx, data))
	must(genMessageAddOn(tx, data))
	must(genReceipts(tx, data))
	must(genStatusTable(tx, data))
	must(genCallLogs(tx, data))
	must(genLabels(tx, data))
	must(genFavorites(tx, data))
	must(genFrequent(tx, data))
	must(genGroupParticipants(tx, data))
	must(genJIDMap(tx, data))
	must(genAndroidMetadata(tx))

	must(tx.Commit())

	log.Printf("Running ANALYZE and VACUUM...")
	_, _ = dst.Exec("ANALYZE;")
	_, _ = dst.Exec("VACUUM;")

	log.Printf("Done. Fake database written to %s", dstPath)
	printSummary(dst)
}

// ---------------------------------------------------------------------------
// Schema (embedded from schema.sql)
// ---------------------------------------------------------------------------

// applyEmbeddedSchema executes the embedded schema.sql script.
//
// The script is produced by `sqlite3 source.db .dump` with data rows
// stripped (see Makefile / regenerate-schema target). The .dump format
// is the canonical SQLite-blessed way to reconstruct a database because
// it handles every edge case that a naive `.schema` dump cannot:
//
//   - sqlite_sequence: not emitted as CREATE TABLE (the name is
//     reserved); SQLite auto-creates it for any AUTOINCREMENT table.
//   - FTS shadow tables (*_fts_content, *_fts_segments, ...): emitted
//     as plain CREATE TABLE *before* the parent CREATE VIRTUAL TABLE,
//     so the FTS constructor finds them already in place.
//   - FTS virtual tables: registered via
//     PRAGMA writable_schema=ON;
//     INSERT INTO sqlite_schema(type,name,tbl_name,rootpage,sql) VALUES(...);
//     PRAGMA writable_schema=OFF;
//     which bypasses the constructor entirely and avoids the "shadow
//     tables already exist" conflict.
//
// The mattn/go-sqlite3 driver only executes the first statement per
// Exec call, so we split on `;` (quote- and comment-aware) and run each
// statement individually. Any failure is fatal — the dump is expected
// to load cleanly into an empty database.
func applyEmbeddedSchema(db *sql.DB) error {
	stmts := splitSQLStatements(embeddedSchema)
	applied := 0
	for _, s := range stmts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("apply %s: %w", firstObjectName(s), err)
		}
		applied++
	}
	log.Printf("  applied %d schema statements", applied)
	return nil
}

// splitSQLStatements splits a SQL script into individual statements on ';'
// boundaries while respecting:
//   - single-quoted strings
//   - SQL line comments (-- ...)
//   - C-style block comments (/* ... */)
func splitSQLStatements(script string) []string {
	var out []string
	var cur strings.Builder

	inSingle := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(script); i++ {
		c := script[i]
		next := byte(0)
		if i+1 < len(script) {
			next = script[i+1]
		}

		switch {
		case inLineComment:
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
		case inBlockComment:
			cur.WriteByte(c)
			if c == '*' && next == '/' {
				cur.WriteByte(next)
				i++
				inBlockComment = false
			}
		case inSingle:
			cur.WriteByte(c)
			if c == '\'' {
				// Handle escaped single-quote ''
				if next == '\'' {
					cur.WriteByte(next)
					i++
				} else {
					inSingle = false
				}
			}
		case c == '\'':
			cur.WriteByte(c)
			inSingle = true
		case c == '-' && next == '-':
			cur.WriteByte(c)
			cur.WriteByte(next)
			i++
			inLineComment = true
		case c == '/' && next == '*':
			cur.WriteByte(c)
			cur.WriteByte(next)
			i++
			inBlockComment = true
		case c == ';':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if rest := strings.TrimSpace(cur.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

// firstObjectName extracts a "TYPE name" label from a CREATE statement for
// diagnostic logging (e.g. "TABLE chat", "INDEX idx_message_chat_row_id").
func firstObjectName(stmt string) string {
	s := strings.TrimSpace(stmt)
	const cre = "CREATE "
	if idx := strings.Index(strings.ToUpper(s), cre); idx >= 0 {
		s = s[idx+len(cre):]
	}

	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "<unknown>"
	}

	objType := fields[0]                          // TABLE / INDEX / VIEW / VIRTUAL ...
	rest := strings.TrimSpace(s[len(fields[0]):]) // everything after the type
	upper := strings.ToUpper(rest)

	// "TEMP TABLE foo", "TEMPORARY TABLE foo", "UNIQUE INDEX foo", etc.
	if strings.HasPrefix(upper, "TABLE ") {
		objType += " TABLE"
		rest = strings.TrimSpace(rest[len("TABLE "):])
		upper = strings.ToUpper(rest)
	}
	// "VIRTUAL TABLE foo USING ..."
	// (objType is already "VIRTUAL TABLE" via the branch above)

	// Skip optional "IF NOT EXISTS"
	if strings.HasPrefix(upper, "IF NOT EXISTS ") {
		rest = strings.TrimSpace(rest[len("IF NOT EXISTS "):])
	}

	// First identifier in `rest` is the object name. Strip quotes, brackets
	// and trailing punctuation (parens, commas, etc.).
	name := rest
	for i, c := range rest {
		if c == ' ' || c == '\t' || c == '\n' || c == '(' {
			name = rest[:i]
			break
		}
	}
	name = strings.Trim(name, "`\"'[]();,")

	return objType + " " + name
}

// ---------------------------------------------------------------------------
// Synthetic data generation
// ---------------------------------------------------------------------------
//
// All distribution constants below were captured from a real ~24MB WhatsApp
// msgstore.db so that scale=1.0 produces a database with the same row counts,
// type ratios, and per-message side-table probabilities. No real data is
// embedded — only aggregate statistics.

// Reference row counts at scale=1.0.
const (
	refJids     = 22638
	refChats    = 1815
	refMessages = 68837
)

// refSideCounts: per-table row counts at scale=1.0 for tables whose
// volume is driven by `tableRowCount` rather than per-message flags.
var refSideCounts = map[string]int64{
	"message_revoked":        285,
	"message_add_on":         11862,
	"receipt_user":           29036,
	"receipt_device":         99023,
	"call_log":               139,
	"status":                 0,
	"labels":                 4,
	"labeled_jid":            0,
	"favorite":               1,
	"frequent":               45,
	"group_participant_user": 2468,
	"jid_map":                2321,
}

// Per-message side-table probabilities (P(message has a row in this table)).
const (
	pFromMe       = 0.167
	pHasText      = 0.0193
	pHasMedia     = 0.375
	pHasQuoted    = 0.134
	pHasEphemeral = 0.0092
	pHasMentions  = 0.0136
)

// JID type distribution observed in the reference DB.
// 19 = extra device (linked), 17 = device, 0 = phone (s.whatsapp.net),
// 18 = LID, 1 = group (g.us), others = rare specialty types.
var jidTypeWeights = []generators.WeightedInt{
	w(19, 9040), w(17, 8762), w(0, 2396), w(18, 2329), w(1, 96),
	w(21, 5), w(25, 2), w(24, 2), w(11, 2), w(3, 2), w(7, 1), w(5, 1),
}

// Server string per JID type. Empty server -> no user/raw_string.
var serverByJIDType = map[int64]string{
	0:  "s.whatsapp.net",
	1:  "g.us",
	17: "s.whatsapp.net",
	18: "lid",
	19: "lid",
	21: "newsletter",
	24: "hosted",
	25: "hosted.lid",
	11: "status",
}

// Chat-JID-type distribution observed in the reference DB.
// 0 = phone (1:1), 18 = LID (1:1), 1 = group, others rare.
var chatJIDTypeWeights = []generators.WeightedInt{
	w(0, 734), w(18, 979), w(1, 96), w(21, 4), w(5, 1), w(7, 1),
}

// Message type distribution observed in the reference DB.
// 0 = text, 1 = image, 3 = video, 7 = system, 20 = sticker,
// 99 = other/unknown, 13 = gif, 2 = audio, 9 = document, etc.
var msgTypeWeights = []generators.WeightedInt{
	w(0, 38998), w(1, 19672), w(3, 3602), w(7, 2601), w(20, 1417),
	w(99, 898), w(13, 420), w(2, 367), w(15, 280), w(9, 231),
	w(5, 84), w(36, 52), w(90, 45), w(11, 38), w(66, 24), w(116, 22),
	w(16, 21), w(4, 20), w(28, 15),
}

// Status field distribution observed in the reference DB.
var msgStatusWeights = []generators.WeightedInt{
	w(0, 41669), w(16, 10682), w(5, 5908), w(17, 4782), w(6, 2647),
}

// w is a compact constructor for generators.WeightedInt used by the
// distribution tables above. It exists purely so those tables stay
// readable as dense lists; switching to keyed struct literals
// (required by go vet across packages) would triple their width.
func w(value int64, weight int) generators.WeightedInt {
	return generators.WeightedInt{Value: value, Weight: weight}
}

// scaledCount returns max(1, round(ref*scale)) when ref > 0, else 0.
func scaledCount(ref int64, scale float64) int64 {
	if ref == 0 {
		return 0
	}
	n := max(int64(float64(ref)*scale), 1)
	return n
}

// generateSyntheticData builds a *sourceData with row plans for jids, chats,
// messages and side tables. All FK references (chat.jid_row_id,
// message.chat_row_id, etc.) point to existing synthetic rows.
//
// If primaryPhone is non-empty, the first jid (ID=1) is reserved as
// the "me" jid with that user and server="s.whatsapp.net". Its
// generated user value is preserved verbatim by genJIDs (no
// randomisation). data.meJIDSourceID is set to that jid's ID.
//
// If primaryPhone is empty, the first phone-type jid encountered
// during generation becomes "me" (its user value will still be
// randomised by genJIDs via generators.JIDIdentity).
func generateSyntheticData(scale float64, primaryPhone string) *sourceData {
	data := &sourceData{
		tableRowCount: map[string]int64{},
		primaryPhone:  primaryPhone,
	}

	// ---- Side-table row counts (scaled) ------------------------------------
	for k, v := range refSideCounts {
		data.tableRowCount[k] = int64(float64(v) * scale)
	}

	// ---- JIDs --------------------------------------------------------------
	nJids := int(scaledCount(refJids, scale))
	data.jids = make([]jidSample, 0, nJids)

	// If a primary phone was supplied, reserve ID=1 for the "me" jid so
	// it's guaranteed to exist with the requested user/server, regardless
	// of how the weighted-type sampling pans out. We still count it
	// towards nJids so the total jid row count stays calibrated.
	startIdx := 1
	if primaryPhone != "" && nJids >= 1 {
		data.jids = append(data.jids, jidSample{
			ID:        1,
			User:      primaryPhone,
			Server:    "s.whatsapp.net",
			Type:      0,
			RawString: primaryPhone + "@s.whatsapp.net",
		})
		data.meJIDSourceID = 1
		startIdx = 2
	}

	for i := startIdx; i <= nJids; i++ {
		jt := generators.WeightedPick(faker, jidTypeWeights)
		server := serverByJIDType[jt]
		var user string
		switch server {
		case "s.whatsapp.net":
			user = generators.PhoneDigits(faker)
		case "g.us":
			user = fmt.Sprintf("%s-%d", generators.PhoneDigits(faker),
				1_600_000_000+rng.Int63n(200_000_000))
		case "lid":
			user = faker.DigitN(15)
		case "newsletter":
			user = faker.DigitN(18)
		case "hosted", "hosted.lid":
			user = faker.DigitN(12)
		case "status":
			user = "status"
		}
		raw := ""
		if server != "" && user != "" {
			raw = user + "@" + server
		}
		data.jids = append(data.jids, jidSample{
			ID:        int64(i),
			User:      user,
			Server:    server,
			Type:      jt,
			RawString: raw,
		})
	}

	// If no primary phone was supplied, fall back to the first phone-type
	// jid as "me". If there are no phone-type jids at all (very small
	// scales), use the first jid.
	if data.meJIDSourceID == 0 {
		for _, j := range data.jids {
			if j.Type == 0 {
				data.meJIDSourceID = j.ID
				break
			}
		}
		if data.meJIDSourceID == 0 && len(data.jids) > 0 {
			data.meJIDSourceID = data.jids[0].ID
		}
	}

	// Index jids by type so chat/message generation can pick valid FKs.
	jidsByT := map[int64][]int64{}
	for _, j := range data.jids {
		jidsByT[j.Type] = append(jidsByT[j.Type], j.ID)
	}

	// Time window for timestamps: last ~3 years of activity ending now.
	now := time.Now().UnixMilli()
	const msPerDay = int64(86_400_000)
	windowStart := now - 3*365*msPerDay

	// ---- Chats -------------------------------------------------------------
	//
	// chat.jid_row_id has a UNIQUE constraint, so we must sample jids
	// without replacement. We pre-shuffle each per-type pool and pop ids
	// off the front. If a type runs out, we fall back to the phone (0)
	// pool, then to any remaining unused jid.
	nChats := int(scaledCount(refChats, scale))
	data.chats = make([]chatSample, 0, nChats)

	// Per-type shuffled pools we can drain.
	availByT := map[int64][]int64{}
	for t, ids := range jidsByT {
		cp := append([]int64(nil), ids...)
		rng.Shuffle(len(cp), func(i, j int) { cp[i], cp[j] = cp[j], cp[i] })
		availByT[t] = cp
	}
	usedJID := map[int64]bool{}

	pickChatJID := func(t int64) (int64, int64, bool) {
		// Try the requested type first.
		pool := availByT[t]
		for len(pool) > 0 {
			j := pool[len(pool)-1]
			pool = pool[:len(pool)-1]
			availByT[t] = pool
			if !usedJID[j] {
				usedJID[j] = true
				return j, t, true
			}
		}
		// Fallback: phone JIDs (most chats are 1:1 phone anyway).
		pool = availByT[0]
		for len(pool) > 0 {
			j := pool[len(pool)-1]
			pool = pool[:len(pool)-1]
			availByT[0] = pool
			if !usedJID[j] {
				usedJID[j] = true
				return j, 0, true
			}
		}
		return 0, 0, false
	}

	// Always reserve chat _id=1 as Notes-to-Self (a 1:1 chat with the me
	// jid). Real WhatsApp users almost always have one once they've ever
	// sent themselves a message. With -phone, this guarantees the chat
	// targets the operator-supplied number; without -phone, it targets
	// the first phone-type jid (the implicit "me").
	if data.meJIDSourceID != 0 && nChats >= 1 {
		usedJID[data.meJIDSourceID] = true
		created := windowStart + rng.Int63n(now-windowStart)
		sortTS := created + rng.Int63n(now-created+1)
		data.chats = append(data.chats, chatSample{
			ID:          1,
			JIDRowID:    data.meJIDSourceID,
			Created:     created,
			SortTS:      sortTS,
			GroupType:   0,
			JIDType:     0,
			UnreadCount: 0,
		})
	}

	for i := len(data.chats) + 1; i <= nChats; i++ {
		ct := generators.WeightedPick(faker, chatJIDTypeWeights)
		jidID, actualType, ok := pickChatJID(ct)
		if !ok {
			break // exhausted all jids
		}
		created := windowStart + rng.Int63n(now-windowStart)
		sortTS := created + rng.Int63n(now-created+1)
		var origin sql.NullString
		if rng.Intn(2) == 0 {
			origin = sql.NullString{String: "general", Valid: true}
		}
		var groupType int64
		if actualType == 1 {
			groupType = 1
		}
		data.chats = append(data.chats, chatSample{
			ID:          int64(i),
			JIDRowID:    jidID,
			Created:     created,
			SortTS:      sortTS,
			ChatOrigin:  origin,
			GroupType:   groupType,
			JIDType:     actualType,
			UnreadCount: int64(rng.Intn(5)),
		})
	}

	// ---- Messages ----------------------------------------------------------
	nMessages := int(scaledCount(refMessages, scale))
	if len(data.chats) == 0 {
		return data
	}
	data.messages = make([]messageSample, 0, nMessages)
	chatIDs := make([]int64, len(data.chats))
	for i, c := range data.chats {
		chatIDs[i] = c.ID
	}

	for i := 1; i <= nMessages; i++ {
		var fromMe int64
		if rng.Float64() < pFromMe {
			fromMe = 1
		}
		mt := generators.WeightedPick(faker, msgTypeWeights)
		ts := windowStart + rng.Int63n(now-windowStart)

		m := messageSample{
			ID:          int64(i),
			ChatRowID:   chatIDs[rng.Intn(len(chatIDs))],
			FromMe:      fromMe,
			Status:      generators.WeightedPick(faker, msgStatusWeights),
			Broadcast:   sql.NullInt64{Int64: 0, Valid: true},
			Timestamp:   ts,
			MessageType: mt,
			SortID:      ts / 1000,
		}

		// text_data is only consumed by generators.MessageText which keys off
		// emptiness: give text-type messages a non-empty marker so they get
		// real fake text generated later.
		if mt == 0 || mt == 7 {
			m.TextData = "x"
		}

		// Per-table existence flags. Each probability is calibrated to
		// produce a globally-correct row count for the corresponding
		// side table at scale=1.0.
		if rng.Float64() < pHasText {
			m.HasText = true
		}
		if rng.Float64() < pHasMedia {
			m.HasMedia = true
		}
		if rng.Float64() < pHasQuoted {
			m.HasQuoted = true
		}
		if rng.Float64() < pHasEphemeral {
			m.HasEphemeral = true
		}
		if rng.Float64() < pHasMentions {
			m.HasMentions = true
		}

		data.messages = append(data.messages, m)
	}
	return data
}

// ---------------------------------------------------------------------------
// Fake data generators
// ---------------------------------------------------------------------------

func genJIDs(tx *sql.Tx, data *sourceData) error {
	log.Printf("  jids (%d rows)...", len(data.jids))

	stmt, err := tx.Prepare(`
		INSERT INTO jid(_id, user, server, agent, device, type, raw_string)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, j := range data.jids {
		var fakeUser, fakeServer string
		if data.primaryPhone != "" && j.ID == data.meJIDSourceID {
			// Preserve the operator-supplied phone for the me jid.
			fakeUser, fakeServer = j.User, j.Server
		} else {
			fakeUser, fakeServer = generators.JIDIdentity(faker, j.User, j.Server)
		}
		fakeRaw := fakeUser + "@" + fakeServer
		if j.Server == "" {
			fakeRaw = ""
		}
		if _, err := stmt.Exec(j.ID, fakeUser, fakeServer, j.Agent, j.Device, j.Type, fakeRaw); err != nil {
			return fmt.Errorf("insert jid %d: %w", j.ID, err)
		}
		jidIDMap[j.ID] = j.ID // keep the same id; the *values* are fake
		jidsByType[j.Type] = append(jidsByType[j.Type], j.ID)
	}

	// Use the "me" jid id determined during generation.
	meJIDID = data.meJIDSourceID
	return nil
}

func genChats(tx *sql.Tx, data *sourceData) error {
	log.Printf("  chats (%d rows)...", len(data.chats))

	stmt, err := tx.Prepare(`
		INSERT INTO chat(
			_id, jid_row_id, hidden, subject, created_timestamp,
			sort_timestamp, mod_tag, gen, spam_detection,
			unseen_earliest_message_received_time, unseen_message_count,
			unseen_missed_calls_count, unseen_row_count, plaintext_disabled,
			vcard_ui_dismissed, change_number_notified_message_row_id,
			show_group_description, ephemeral_expiration,
			ephemeral_setting_timestamp, ephemeral_displayed_exemptions,
			unseen_important_message_count, group_type,
			chat_origin, participation_status, chat_encryption_state,
			group_member_count, is_contact
		) VALUES (?, ?, 0, ?, ?, ?, 1, 0, 0, 0, ?, 0, 0, 1, 1, 0, 1, 0, 0, 0, 0, ?, ?, 2, 2, ?, 1)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range data.chats {
		subject := generators.ChatSubject(faker, c.JIDType == 1 || c.GroupType > 0)
		origin := "general"
		if c.ChatOrigin.Valid {
			origin = c.ChatOrigin.String
		}
		members := int64(0)
		if c.JIDType == 1 { // group
			members = int64(3 + rng.Intn(50))
		}
		if _, err := stmt.Exec(
			c.ID, c.JIDRowID, subject, c.Created, c.SortTS,
			c.UnreadCount, c.GroupType, origin, members,
		); err != nil {
			return fmt.Errorf("insert chat %d: %w", c.ID, err)
		}
		chatIDMap[c.ID] = c.ID
	}
	return nil
}

func genMessages(tx *sql.Tx, data *sourceData) error {
	log.Printf("  messages (%d rows)...", len(data.messages))

	stmt, err := tx.Prepare(`
		INSERT INTO message(
			_id, chat_row_id, from_me, key_id, sender_jid_row_id,
			status, broadcast, recipient_count, participant_hash,
			origination_flags, origin, timestamp, received_timestamp,
			receipt_server_timestamp, message_type, text_data,
			starred, lookup_tables, message_add_on_flags, view_mode, sort_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, 0, NULL, ?, ?, ?, ?, -1, ?, ?, ?, 0, ?, 0, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// Group sender pools by chat (for group chats use multiple senders)
	chatSenders := map[int64][]int64{}
	for _, c := range data.chats {
		if c.JIDType == 1 || c.GroupType > 0 {
			// Pick up to 10 random phone-number jids as group members
			phones := jidsByType[0]
			n := minInt(len(phones), 5+rng.Intn(10))
			pool := make([]int64, 0, n+1)
			pool = append(pool, meJIDID)
			perm := rng.Perm(len(phones))
			for i := 0; i < n && i < len(perm); i++ {
				pool = append(pool, phones[perm[i]])
			}
			chatSenders[c.ID] = pool
		} else {
			// 1:1 chats: sender is either me or the contact jid
			chatSenders[c.ID] = []int64{meJIDID, c.JIDRowID}
		}
	}

	for _, m := range data.messages {
		// Map sender
		sender := m.SenderJIDID
		if m.FromMe == 1 {
			sender = sql.NullInt64{Int64: meJIDID, Valid: true}
		} else if pool, ok := chatSenders[m.ChatRowID]; ok && len(pool) > 1 {
			// Pick random non-me sender from pool
			candidates := pool[1:]
			sender = sql.NullInt64{Int64: candidates[rng.Intn(len(candidates))], Valid: true}
		}

		keyID := generators.KeyID(faker)
		textData := generators.MessageText(faker, m.MessageType, len(m.TextData))

		if _, err := stmt.Exec(
			m.ID, m.ChatRowID, m.FromMe, keyID, sender,
			m.Status, m.Broadcast,
			m.OrigFlags, m.Origin, m.Timestamp, m.Received,
			m.MessageType, textData, m.Starred, m.HasAddOnFlags, m.SortID,
		); err != nil {
			return fmt.Errorf("insert message %d: %w", m.ID, err)
		}
		messageIDMap[m.ID] = m.ID
	}
	return nil
}

func genMessageText(tx *sql.Tx, data *sourceData) error {
	// Count how many messages have message_text rows.
	var count int
	for _, m := range data.messages {
		if m.HasText {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	log.Printf("  message_text (%d rows)...", count)

	stmt, err := tx.Prepare(`
		INSERT INTO message_text(
			message_row_id, description, page_title, url,
			font_style, text_color, background_color, preview_type
		) VALUES (?, ?, ?, ?, 0, 0, 0, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range data.messages {
		if !m.HasText {
			continue
		}
		desc, title, url := generators.LinkPreview(faker)
		previewType := int64(0)
		if url == "" {
			previewType = -1
		}
		if _, err := stmt.Exec(m.ID, desc, title, url, previewType); err != nil {
			return fmt.Errorf("insert message_text %d: %w", m.ID, err)
		}
	}
	return nil
}

func genMessageMedia(tx *sql.Tx, data *sourceData) error {
	var count int
	for _, m := range data.messages {
		if m.HasMedia {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	log.Printf("  message_media (%d rows)...", count)

	stmt, err := tx.Prepare(`
		INSERT INTO message_media(
			message_row_id, chat_row_id, autotransfer_retry_enabled, transferred,
			file_path, file_size, media_key, media_key_timestamp,
			width, height, direct_path, mime_type, file_length,
			media_name, file_hash, media_duration, enc_file_hash,
			media_caption, media_source_type
		) VALUES (?, ?, 0, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range data.messages {
		if !m.HasMedia {
			continue
		}
		mime, ext, w, h, dur := generators.MediaInfo(faker, m.MessageType)
		size := int64(50_000 + rng.Intn(8_000_000))
		filePath := fmt.Sprintf("Media/WhatsApp/%s-%d-%s%s",
			faker.LoremIpsumWord(), m.Timestamp, generators.HexN(faker, 8), ext)
		name := fmt.Sprintf("%s%s", generators.HexN(faker, 12), ext)
		direct := "/v/t62/0/" + generators.HexN(faker, 16) + ext
		caption := ""
		if rng.Intn(3) == 0 {
			caption = faker.Sentence(6)
		}

		if _, err := stmt.Exec(
			m.ID, m.ChatRowID, filePath, size,
			generators.Bytes(faker, 32), m.Timestamp/1000,
			w, h, direct, mime, size,
			name, generators.Base64N(faker, 32), dur, generators.Base64N(faker, 32),
			caption,
		); err != nil {
			return fmt.Errorf("insert message_media %d: %w", m.ID, err)
		}
	}
	return nil
}

func genMessageQuoted(tx *sql.Tx, data *sourceData) error {
	// Check if message_quoted exists
	if !tableExists(tx, "message_quoted") {
		return nil
	}
	var count int
	for _, m := range data.messages {
		if m.HasQuoted {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	log.Printf("  message_quoted (%d rows)...", count)

	stmt, err := tx.Prepare(`
		INSERT INTO message_quoted(message_row_id, chat_row_id,
			parent_message_chat_row_id, from_me, sender_jid_row_id, key_id,
			timestamp, message_type, origin, text_data, lookup_tables)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`)
	if err != nil {
		return fmt.Errorf("prepare message_quoted: %w", err)
	}
	defer stmt.Close()

	for _, m := range data.messages {
		if !m.HasQuoted {
			continue
		}
		phones := jidsByType[0]
		sender := meJIDID
		if len(phones) > 0 {
			sender = phones[rng.Intn(len(phones))]
		}
		fromMe := int64(rng.Intn(2))
		if fromMe == 1 {
			sender = meJIDID
		}
		if _, err := stmt.Exec(
			m.ID, m.ChatRowID, m.ChatRowID,
			fromMe, sender, generators.KeyID(faker),
			m.Timestamp-int64(1000+rng.Intn(60000)),
			0, 0, faker.Sentence(6),
		); err != nil {
			return fmt.Errorf("insert message_quoted %d: %w", m.ID, err)
		}
	}
	return nil
}

func genMessageEphemeral(tx *sql.Tx, data *sourceData) error {
	var count int
	for _, m := range data.messages {
		if m.HasEphemeral {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	log.Printf("  message_ephemeral (%d rows)...", count)

	stmt, err := tx.Prepare(`
		INSERT INTO message_ephemeral(
			message_row_id, duration, expire_timestamp, keep_in_chat
		) VALUES (?, ?, ?, 0)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range data.messages {
		if !m.HasEphemeral {
			continue
		}
		durations := []int64{86400, 604800, 7776000} // 1d, 7d, 90d
		dur := durations[rng.Intn(len(durations))]
		expire := m.Timestamp + dur*1000
		if _, err := stmt.Exec(m.ID, dur, expire); err != nil {
			return fmt.Errorf("insert message_ephemeral %d: %w", m.ID, err)
		}
	}
	return nil
}

func genMessageRevoked(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["message_revoked"]
	if n == 0 {
		return nil
	}
	log.Printf("  message_revoked (%d rows)...", n)

	stmt, err := tx.Prepare(`
		INSERT INTO message_revoked(message_row_id, revoked_key_id, revoke_timestamp)
		VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// Pick random messages to mark as revoked
	if len(data.messages) == 0 {
		return nil
	}
	perm := rng.Perm(len(data.messages))
	for i := int64(0); i < n && i < int64(len(perm)); i++ {
		m := data.messages[perm[i]]
		_, _ = stmt.Exec(m.ID, generators.KeyID(faker), m.Timestamp+int64(rng.Intn(3600000)))
	}
	return nil
}

func genMessageMentions(tx *sql.Tx, data *sourceData) error {
	var count int
	for _, m := range data.messages {
		if m.HasMentions {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	log.Printf("  message_mentions (%d rows)...", count)

	stmt, err := tx.Prepare(`
		INSERT INTO message_mentions(message_row_id, jid_row_id, display_name)
		VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	phones := jidsByType[0]
	if len(phones) == 0 {
		return nil
	}
	for _, m := range data.messages {
		if !m.HasMentions {
			continue
		}
		nmen := 1 + rng.Intn(3)
		for range nmen {
			jid := phones[rng.Intn(len(phones))]
			_, _ = stmt.Exec(m.ID, jid, faker.FirstName())
		}
	}
	return nil
}

func genMessageAddOn(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["message_add_on"]
	if n == 0 || len(data.messages) == 0 {
		return nil
	}
	log.Printf("  message_add_on (%d rows)...", n)

	stmt, err := tx.Prepare(`
		INSERT INTO message_add_on(
			chat_row_id, from_me, key_id, sender_jid_row_id,
			parent_message_row_id, timestamp, status, message_add_on_type,
			received_timestamp
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	reactStmt, err := tx.Prepare(`
		INSERT INTO message_add_on_reaction(
			message_add_on_row_id, sender_timestamp, reaction
		) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer reactStmt.Close()

	emojis := []string{"❤", "👍", "😂", "😮", "😢", "🙏", "🔥", "🎉", "💯"}
	phones := jidsByType[0]
	if len(phones) == 0 {
		phones = []int64{meJIDID}
	}

	for range n {
		m := data.messages[rng.Intn(len(data.messages))]
		fromMe := int64(rng.Intn(2))
		sender := phones[rng.Intn(len(phones))]
		if fromMe == 1 {
			sender = meJIDID
		}
		ts := m.Timestamp + int64(1000+rng.Intn(3600000))
		res, err := stmt.Exec(m.ChatRowID, fromMe, generators.KeyID(faker), sender,
			m.ID, ts, 0, 2 /* reaction */, ts)
		if err != nil {
			continue
		}
		addOnID, _ := res.LastInsertId()
		_, _ = reactStmt.Exec(addOnID, ts, emojis[rng.Intn(len(emojis))])
	}
	return nil
}

func genReceipts(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["receipt_user"]
	if n == 0 {
		return nil
	}
	log.Printf("  receipt_user (%d rows)...", n)

	stmt, err := tx.Prepare(`
		INSERT INTO receipt_user(
			message_row_id, receipt_user_jid_row_id,
			receipt_timestamp, read_timestamp, played_timestamp
		) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	phones := jidsByType[0]
	if len(phones) == 0 || len(data.messages) == 0 {
		return nil
	}

	// Build pool of from_me messages (receipts are only on outgoing messages)
	var outgoing []messageSample
	for _, m := range data.messages {
		if m.FromMe == 1 {
			outgoing = append(outgoing, m)
		}
	}
	if len(outgoing) == 0 {
		outgoing = data.messages
	}

	for range n {
		m := outgoing[rng.Intn(len(outgoing))]
		recipient := phones[rng.Intn(len(phones))]
		base := m.Timestamp + int64(1000+rng.Intn(60000))
		read := base + int64(rng.Intn(120000))
		var readNS, playedNS sql.NullInt64
		if rng.Intn(2) == 0 {
			readNS = sql.NullInt64{Int64: read, Valid: true}
		}
		if rng.Intn(4) == 0 {
			playedNS = sql.NullInt64{Int64: read + int64(rng.Intn(60000)), Valid: true}
		}
		if _, err := stmt.Exec(m.ID, recipient, base, readNS, playedNS); err != nil {
			return fmt.Errorf("insert receipt_user: %w", err)
		}
	}

	// receipt_device
	nDev := data.tableRowCount["receipt_device"]
	if nDev > 0 {
		log.Printf("  receipt_device (%d rows)...", nDev)
		devStmt, err := tx.Prepare(`
			INSERT INTO receipt_device(message_row_id, receipt_device_jid_row_id, receipt_device_timestamp)
			VALUES (?, ?, ?)`)
		if err == nil {
			defer devStmt.Close()
			for range nDev {
				m := outgoing[rng.Intn(len(outgoing))]
				recipient := phones[rng.Intn(len(phones))]
				ts := m.Timestamp + int64(rng.Intn(60000))
				_, _ = devStmt.Exec(m.ID, recipient, ts)
			}
		}
	}
	return nil
}

func genStatusTable(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["status"]
	if n == 0 {
		return nil
	}
	log.Printf("  status (%d rows)...", n)
	stmt, err := tx.Prepare(`
		INSERT INTO status(jid_row_id, timestamp, unseen_count, total_count, unseen_count_close_friends)
		VALUES (?, ?, ?, ?, 0)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	phones := jidsByType[0]
	if len(phones) == 0 {
		return nil
	}
	used := map[int64]bool{}
	for range n {
		if len(used) >= len(phones) {
			break
		}
		var jid int64
		for {
			jid = phones[rng.Intn(len(phones))]
			if !used[jid] {
				used[jid] = true
				break
			}
		}
		ts := time.Now().UnixMilli() - int64(rng.Intn(86400000))
		_, _ = stmt.Exec(jid, ts, rng.Intn(5), rng.Intn(10)+1)
	}
	return nil
}

func genCallLogs(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["call_log"]
	if n == 0 {
		return nil
	}
	log.Printf("  call_log (%d rows)...", n)
	stmt, err := tx.Prepare(`
		INSERT INTO call_log(jid_row_id, from_me, call_id, transaction_id,
			timestamp, video_call, duration, call_result, bytes_transferred,
			group_jid_row_id, is_joinable_group_call, call_creator_device_jid_row_id, call_random_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		// schema differs across versions; try a minimal insert
		stmt, err = tx.Prepare(`INSERT INTO call_log(jid_row_id, from_me, timestamp) VALUES (?, ?, ?)`)
		if err != nil {
			return nil
		}
		defer stmt.Close()
		phones := jidsByType[0]
		if len(phones) == 0 {
			return nil
		}
		for range n {
			jid := phones[rng.Intn(len(phones))]
			ts := time.Now().UnixMilli() - int64(rng.Intn(86400000*30))
			_, _ = stmt.Exec(jid, rng.Intn(2), ts)
		}
		return nil
	}
	defer stmt.Close()

	phones := jidsByType[0]
	if len(phones) == 0 {
		return nil
	}
	for range n {
		jid := phones[rng.Intn(len(phones))]
		ts := time.Now().UnixMilli() - int64(rng.Intn(86400000*30))
		fromMe := int64(rng.Intn(2))
		duration := int64(rng.Intn(1800))
		result := int64(rng.Intn(6))
		_, _ = stmt.Exec(jid, fromMe, generators.HexN(faker, 16), generators.HexN(faker, 8),
			ts, rng.Intn(2), duration, result, rng.Intn(1_000_000),
			0, 0, meJIDID, rng.Int63())
	}
	return nil
}

func genLabels(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["labels"]
	if n == 0 {
		return nil
	}
	log.Printf("  labels (%d rows)...", n)
	stmt, err := tx.Prepare(`
		INSERT INTO labels(_id, type, label_name, predefined_id, color_id, sort_id, hidden, is_immutable)
		VALUES (?, 0, ?, ?, ?, ?, 0, 0)`)
	if err != nil {
		// schema differs across versions; skip
		log.Printf("  warn: skipping labels: %v", err)
		return nil
	}
	defer stmt.Close()

	names := []string{"Work", "Family", "Friends", "VIP", "Pending", "Done",
		"Important", "Personal", "Travel", "Bills", "Receipts"}
	for i := int64(0); i < n && int(i) < len(names); i++ {
		if _, err := stmt.Exec(i+1, names[i], 0, rng.Intn(10), i); err != nil {
			return fmt.Errorf("insert labels %d: %w", i+1, err)
		}
		labelIDMap[i+1] = i + 1
	}

	// labeled_jid links labels to jids
	if nLink := data.tableRowCount["labeled_jid"]; nLink > 0 {
		linkStmt, err := tx.Prepare(`INSERT INTO labeled_jid(label_id, jid_row_id) VALUES (?, ?)`)
		if err == nil {
			defer linkStmt.Close()
			phones := jidsByType[0]
			if len(phones) > 0 && len(labelIDMap) > 0 {
				labelIDs := make([]int64, 0, len(labelIDMap))
				for k := range labelIDMap {
					labelIDs = append(labelIDs, k)
				}
				slices.Sort(labelIDs)
				for range nLink {
					_, _ = linkStmt.Exec(
						labelIDs[rng.Intn(len(labelIDs))],
						phones[rng.Intn(len(phones))],
					)
				}
			}
		}
	}
	return nil
}

func genFavorites(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["favorite"]
	if n == 0 {
		return nil
	}
	log.Printf("  favorite (%d rows)...", n)
	stmt, err := tx.Prepare(`INSERT INTO favorite(jid_row_id, favorite_type, sort_order) VALUES (?, ?, ?)`)
	if err != nil {
		return nil
	}
	defer stmt.Close()
	phones := jidsByType[0]
	if len(phones) == 0 {
		return nil
	}
	used := map[int64]bool{}
	for i := int64(0); i < n && len(used) < len(phones); i++ {
		var jid int64
		for {
			jid = phones[rng.Intn(len(phones))]
			if !used[jid] {
				used[jid] = true
				break
			}
		}
		_, _ = stmt.Exec(jid, 0, i)
	}
	return nil
}

func genFrequent(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["frequent"]
	if n == 0 {
		return nil
	}
	log.Printf("  frequent (%d rows)...", n)
	stmt, err := tx.Prepare(`INSERT INTO frequent(jid_row_id, type, message_count) VALUES (?, ?, ?)`)
	if err != nil {
		return nil
	}
	defer stmt.Close()
	phones := jidsByType[0]
	if len(phones) == 0 {
		return nil
	}
	for i := int64(0); i < n && int(i) < len(phones); i++ {
		_, _ = stmt.Exec(phones[i], 1, 5+rng.Intn(500))
	}
	return nil
}

func genGroupParticipants(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["group_participant_user"]
	if n == 0 {
		return nil
	}
	log.Printf("  group_participant_user (%d rows)...", n)
	stmt, err := tx.Prepare(`
		INSERT INTO group_participant_user(group_jid_row_id, user_jid_row_id, rank, pending, add_timestamp)
		VALUES (?, ?, ?, 0, ?)`)
	if err != nil {
		return nil
	}
	defer stmt.Close()

	groups := jidsByType[1]
	phones := jidsByType[0]
	if len(groups) == 0 || len(phones) == 0 {
		return nil
	}

	// "me" must appear in every group it participates in. Exclude the me
	// jid from the random-member pool so it can be added once per group
	// with a deterministic role.
	otherPhones := make([]int64, 0, len(phones))
	for _, p := range phones {
		if p != meJIDID {
			otherPhones = append(otherPhones, p)
		}
	}

	// Distribute participants across groups. Reserve one slot per group
	// for "me" so the average headcount stays close to the calibrated
	// total.
	per := int(n)/len(groups) + 1
	if per > 1 {
		per--
	}
	for _, g := range groups {
		members := minInt(per, len(otherPhones))
		perm := rng.Perm(len(otherPhones))
		for i := range members {
			rank := 0
			if i == 0 {
				rank = 2 // admin/owner
			}
			_, _ = stmt.Exec(g, otherPhones[perm[i]], rank, time.Now().UnixMilli())
		}
		// Always add the me jid to every group as a regular member.
		if meJIDID != 0 {
			_, _ = stmt.Exec(g, meJIDID, 0, time.Now().UnixMilli())
		}
	}
	return nil
}

func genJIDMap(tx *sql.Tx, data *sourceData) error {
	n := data.tableRowCount["jid_map"]
	if n == 0 {
		return nil
	}
	log.Printf("  jid_map (%d rows)...", n)
	stmt, err := tx.Prepare(`INSERT INTO jid_map(lid_row_id, jid_row_id) VALUES (?, ?)`)
	if err != nil {
		return nil
	}
	defer stmt.Close()
	lids := jidsByType[18]
	phones := jidsByType[0]
	if len(lids) == 0 || len(phones) == 0 {
		return nil
	}
	used := map[int64]bool{}
	for i := int64(0); i < n && len(used) < len(lids); i++ {
		var lid int64
		for {
			lid = lids[rng.Intn(len(lids))]
			if !used[lid] {
				used[lid] = true
				break
			}
		}
		_, _ = stmt.Exec(lid, phones[rng.Intn(len(phones))])
	}
	return nil
}

func genAndroidMetadata(tx *sql.Tx) error {
	if !tableExists(tx, "android_metadata") {
		return nil
	}
	_, _ = tx.Exec(`INSERT INTO android_metadata(locale) VALUES ('en_US')`)
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// parsePrimaryPhone validates a -phone flag value and returns the
// digits-only form (with the leading '+' stripped).
//
// An empty input is valid and returns ("", nil) — meaning "no
// primary phone specified, use a random one for the me jid".
//
// A non-empty input must:
//   - start with a single '+'
//   - contain 7 to 15 ASCII digits after the '+'
//     (matches E.164's recommended range)
//   - contain no other characters
func parsePrimaryPhone(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, "+") {
		return "", fmt.Errorf("must start with '+'")
	}
	digits := s[1:]
	if len(digits) < 7 || len(digits) > 15 {
		return "", fmt.Errorf("must have 7-15 digits after '+' (got %d)", len(digits))
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("contains non-digit character %q", c)
		}
	}
	return digits, nil
}

func tableExists(tx *sql.Tx, name string) bool {
	var n int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0
}

func printSummary(db *sql.DB) {
	tables := []string{"jid", "chat", "message", "message_text", "message_media",
		"message_quoted", "message_ephemeral", "message_revoked",
		"message_mentions", "message_add_on", "receipt_user", "status",
		"call_log", "labels", "favorite", "frequent"}
	fmt.Println("\nRow counts in fake database:")
	for _, t := range tables {
		var n int64
		err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", t)).Scan(&n)
		if err != nil {
			continue
		}
		fmt.Printf("  %-25s %d\n", t, n)
	}
}
