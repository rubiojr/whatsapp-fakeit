package generators

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/brianvoe/gofakeit/v7"
)

// KeyID returns a 32-character uppercase hex string, matching the
// shape of WhatsApp message key ids (see `message.key_id`).
func KeyID(f *gofakeit.Faker) string {
	return strings.ToUpper(HexN(f, 16))
}

// countryCodes is a small pool of realistic phone country prefixes
// (US, ES, UK, DE, FR, IT, PT, AU, BR, MX, IN, JP). Intentionally
// short and weighted by inclusion (not frequency).
var countryCodes = []string{
	"1", "34", "44", "49", "33", "39", "351", "61", "55", "52", "91", "81",
}

// PhoneDigits returns an E.164-ish phone number as a digits-only
// string: a country code from countryCodes followed by 9 or 10 random
// digits.
func PhoneDigits(f *gofakeit.Faker) string {
	cc := f.RandomString(countryCodes)
	return cc + f.DigitN(uint(9+f.IntN(2)))
}

// JIDIdentity derives a (user, server) pair given the original
// (originalUser, originalServer). The server is preserved unchanged;
// the user is regenerated in a format appropriate to that server:
//
//   - "s.whatsapp.net" -> phone-digits (E.164-ish)
//   - "g.us" with a '-' in the original user -> "<phone>-<unix-ts>"
//   - "g.us" without a '-' -> 18 random digits
//   - "lid"               -> 15 random digits
//   - "newsletter"        -> 18 random digits
//   - "status"            -> literal "status"
//   - "broadcast"         -> original user preserved
//   - anything else with non-empty user -> phone-digits
//
// If both inputs are empty the result is two empty strings.
func JIDIdentity(f *gofakeit.Faker, originalUser, originalServer string) (user, server string) {
	if originalUser == "" && originalServer == "" {
		return "", ""
	}
	server = originalServer
	switch originalServer {
	case "s.whatsapp.net":
		user = PhoneDigits(f)
	case "g.us":
		if strings.Contains(originalUser, "-") {
			user = fmt.Sprintf("%s-%d", PhoneDigits(f), 1_600_000_000+f.Number(0, 199_999_999))
		} else {
			user = f.DigitN(18)
		}
	case "lid":
		user = f.DigitN(15)
	case "broadcast":
		user = originalUser
	case "newsletter":
		user = f.DigitN(18)
	case "status":
		user = "status"
	default:
		if originalUser == "" {
			user = ""
		} else {
			user = PhoneDigits(f)
		}
	}
	return user, server
}

// ChatSubject returns a chat subject line. Group chats (isGroup=true)
// get one of six themed patterns (company name, team, city+word,
// family, project, club). Non-group chats get an empty string,
// matching real msgstore.db behaviour where 1:1 chats rarely have a
// subject set.
func ChatSubject(f *gofakeit.Faker, isGroup bool) string {
	if !isGroup {
		return ""
	}
	switch f.IntN(6) {
	case 0:
		return f.Company()
	case 1:
		return f.JobTitle() + " Team"
	case 2:
		return f.City() + " " + f.Word()
	case 3:
		return f.LastName() + " Family"
	case 4:
		return "Project " + f.Word()
	default:
		return f.Word() + " Club"
	}
}

// MessageText returns a fake text payload for a message of the given
// message_type. originalLen is the length-bucket hint of the source
// row's text_data:
//
//   - 0          -> "" (source had no text)
//   - <20        -> short phrase
//   - <80        -> single sentence
//   - <300       -> short paragraph
//   - otherwise  -> longer paragraph
//
// Non-text message types use shorter payloads (message_type 7 =
// system / group subject change uses a company name; everything else
// gets a short sentence suitable as a media caption).
func MessageText(f *gofakeit.Faker, msgType int64, originalLen int) string {
	if originalLen == 0 {
		return ""
	}
	switch msgType {
	case 0: // text
		switch {
		case originalLen < 20:
			return f.Phrase()
		case originalLen < 80:
			return f.Sentence(8)
		case originalLen < 300:
			return f.Paragraph(1, 2, 8, " ")
		default:
			return f.Paragraph(2, 4, 10, " ")
		}
	case 7: // group subject change / system text
		return f.Company()
	default:
		return f.Sentence(6)
	}
}

// previewDomains is the pool of host names used in fake link previews.
// All resolve to example.* sinkhole zones so the urls never leak.
var previewDomains = []string{
	"example.com", "news.example.org", "blog.example.net",
	"shop.example.com", "video.example.io", "photos.example.app",
}

// LinkPreview returns a synthetic (description, title, url) triple
// suitable for a message_text row. The url always uses an example.*
// host and a slugified sentence as its path; the title is a
// title-cased sentence; the description is a short paragraph.
func LinkPreview(f *gofakeit.Faker) (desc, title, url string) {
	d := f.RandomString(previewDomains)
	slug := strings.ReplaceAll(strings.ToLower(f.Sentence(5)), " ", "-")
	slug = strings.TrimRight(slug, ".,!?")
	if len(slug) > 60 {
		slug = slug[:60]
	}
	url = "https://" + d + "/" + slug
	title = titleCase(f.Sentence(4))
	desc = f.Paragraph(1, 1, 5, " ")
	return
}

// titleCase capitalises the first rune of each whitespace-separated
// word in s. It is a minimal stand-in for strings.Title (deprecated
// in Go 1.18) that avoids depending on golang.org/x/text. The
// behaviour is intentionally simple: no smart handling of Unicode
// punctuation or apostrophes, which is fine for the lorem-ipsum
// sentences gofakeit produces.
func titleCase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	atWordStart := true
	for _, r := range s {
		if unicode.IsSpace(r) {
			b.WriteRune(r)
			atWordStart = true
			continue
		}
		if atWordStart {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(r)
		}
		atWordStart = false
	}
	return b.String()
}

// MediaInfo returns (mime, ext, width, height, duration_sec) for a
// media row based on message_type:
//
//   - 1  image    -> image/jpeg, .jpg, 720..2000x720..2000
//   - 2  audio    -> audio/ogg; codecs=opus, .opus, 1..120s
//   - 3  video    -> video/mp4, .mp4, 720x1280, 5..185s
//   - 9  document -> application/octet-stream + random extension
//   - 13 gif      -> video/mp4, .mp4, 480x480, 2..10s
//   - 20 sticker  -> image/webp, .webp, 512x512
//   - default     -> image/jpeg, .jpg, 1080x1080
func MediaInfo(f *gofakeit.Faker, msgType int64) (mime, ext string, w, h, dur int64) {
	switch msgType {
	case 1: // image
		return "image/jpeg", ".jpg",
			int64(720 + f.IntN(1280)),
			int64(720 + f.IntN(1280)),
			0
	case 2: // audio
		return "audio/ogg; codecs=opus", ".opus", 0, 0, int64(1 + f.IntN(120))
	case 3: // video
		return "video/mp4", ".mp4", 720, 1280, int64(5 + f.IntN(180))
	case 9: // document
		exts := []string{".pdf", ".docx", ".xlsx", ".txt", ".zip"}
		return "application/octet-stream", f.RandomString(exts), 0, 0, 0
	case 13: // gif
		return "video/mp4", ".mp4", 480, 480, int64(2 + f.IntN(8))
	case 20: // sticker
		return "image/webp", ".webp", 512, 512, 0
	default:
		return "image/jpeg", ".jpg", 1080, 1080, 0
	}
}
