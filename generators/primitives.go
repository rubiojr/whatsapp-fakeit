// Package generators provides typed fake-data generators that compose
// gofakeit primitives. It is split into:
//
//   - primitives.go: domain-agnostic helpers (HexN, Bytes, Base64N,
//     WeightedPick) that fill gaps in gofakeit's built-ins.
//   - whatsapp.go: WhatsApp-specific generators (KeyID, PhoneDigits,
//     JIDIdentity, ChatSubject, MessageText, LinkPreview, MediaInfo).
//
// All generators take a *gofakeit.Faker and return concrete typed
// values. They are plain helper functions; nothing is registered with
// gofakeit's AddFuncLookup machinery (which is designed for struct-tag
// / template / CLI dispatch — none of which we use here).
package generators

import "github.com/brianvoe/gofakeit/v7"

// WeightedInt is an int64 value paired with a non-negative integer
// weight, used by WeightedPick to describe a discrete distribution.
type WeightedInt struct {
	Value  int64
	Weight int
}

// WeightedPick returns one Value from items, sampled proportional to
// each item's Weight. It returns 0 when items is empty or every weight
// is zero. Negative weights are not supported and will skew the
// distribution.
//
// This is a typed alternative to gofakeit.Faker.Weighted, which
// returns (any, error) and requires a type assertion at every call
// site.
func WeightedPick(f *gofakeit.Faker, items []WeightedInt) int64 {
	total := 0
	for _, it := range items {
		total += it.Weight
	}
	if total == 0 {
		return 0
	}
	r := f.IntN(total)
	sum := 0
	for _, it := range items {
		sum += it.Weight
		if r < sum {
			return it.Value
		}
	}
	return items[len(items)-1].Value
}

// HexN returns a lowercase hexadecimal string of 2*byteLen characters
// (encoding byteLen random bytes worth of entropy).
//
// gofakeit's HexUint prepends "0x" and is parameterised by bit-size,
// which is awkward for our use case where we want raw hex with a
// specific character count.
func HexN(f *gofakeit.Faker, byteLen int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, byteLen*2)
	for i := range out {
		out[i] = hex[f.IntN(16)]
	}
	return string(out)
}

// Bytes returns n random bytes.
func Bytes(f *gofakeit.Faker, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(f.IntN(256))
	}
	return b
}

// Base64N returns n random characters from the standard base64
// alphabet (A-Z, a-z, 0-9, +, /). The result is not base64-encoded
// data — it just looks like base64 for fields where the consumer only
// needs a syntactically plausible value.
func Base64N(f *gofakeit.Faker, n int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, n)
	for i := range out {
		out[i] = charset[f.IntN(len(charset))]
	}
	return string(out)
}
