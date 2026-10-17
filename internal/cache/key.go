package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// keyVersion names the layout of the bytes Key hashes. It changes with the
// layout, so a key made under an old one can never be mistaken for a new
// one's.
const keyVersion = "v1"

// MaxRequestBody is the largest request body Key keys: 4 MiB. Decoding and
// re-encoding a body costs about six times its size in memory (27 MB for a
// 5 MB image), and the cache runs before the rate limiter, so without a cap a
// tenant whose tokens are used up could still make the gateway pay that on
// every request. 4 MiB still holds a text prompt of about a million tokens,
// or a typical image. (MaxBody, by contrast, caps the response a Store keeps.)
const MaxRequestBody = 4 << 20

// Key returns the cache key of a request, or ok == false when the request
// can't be cached. r is the request as the proxy will forward it, after
// StripPrefix, and body is the body it will forward.
//
// The key is "cache:{<tenantID>}:" and the 64 hex digits of a SHA-256 over
// everything that can change a provider's answer:
//   - the tenant and the provider's name
//   - the escaped path
//   - the query, sorted by parameter name and without the credential
//     parameters Provider.Apply strips (provider.CredentialParams)
//   - the Anthropic-Version and Anthropic-Beta headers, as the client sent
//     them
//   - the body, as a JSON value
//
// Two bodies get the same key when they are the same JSON value: whitespace,
// the order of an object's keys at any depth, and how a string is escaped
// don't matter. With duplicate keys the last one wins, as it does at the
// providers that accept them. A number keeps its spelling, so 1 and 1.0 get
// different keys. That can lose a hit but never serves a wrong one, and it
// keeps apart integers too large for a float64 to tell apart.
//
// A request isn't cacheable (ok is false) when two different requests could
// otherwise get the same key:
//   - The query doesn't parse (a bad %-escape, a ';' separator). The parser
//     would drop the bad part, and so merge two queries.
//   - The body isn't exactly one JSON value. That includes an empty body, and
//     anything but whitespace after the value.
//   - The body has invalid UTF-8, or a \u escape of half a surrogate pair.
//     Decoding replaces either with U+FFFD, so two different bodies would
//     decode alike.
//
// Nor is a request whose body is over MaxRequestBody, whatever it holds. That
// check comes first, so such a body is never decoded.
func Key(tenantID, providerName string, r *http.Request, body []byte) (key string, ok bool) {
	if len(body) > MaxRequestBody {
		return "", false
	}

	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", false
	}
	for _, name := range provider.CredentialParams {
		query.Del(name)
	}

	doc, ok := decodeBody(body)
	if !ok {
		return "", false
	}

	// Every part is written behind its length, so no two sets of parts can
	// run together into the same bytes: tenant "ab" with provider "c" differs
	// from tenant "a" with provider "bc". The body is last and needs no
	// length, since nothing follows it.
	head := appendPart(nil, keyVersion)
	head = appendPart(head, tenantID)
	head = appendPart(head, providerName)
	// The escaped path, not the decoded one: RequireMetered matches the
	// decoded path, so /v1/chat%2Fcompletions passes it, yet the proxy
	// forwards it escaped, and the provider may take it for another endpoint.
	head = appendPart(head, r.URL.EscapedPath())
	// Encode sorts by parameter name. A repeated parameter's values keep their
	// order, on purpose: their order can matter.
	head = appendPart(head, query.Encode())
	// A header can be absent, empty, or sent on several lines, and "a, b" on
	// one line is not "a" and "b" on two. The count of values, then each
	// value, keeps all of those apart.
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
		values := r.Header.Values(name)
		head = binary.BigEndian.AppendUint64(head, uint64(len(values)))
		for _, value := range values {
			head = appendPart(head, value)
		}
	}

	// Go note: a hash.Hash is an io.Writer. Write adds bytes to it (and never
	// fails), and Sum returns the digest of all the bytes written so far. So
	// the encoder can write the canonical body straight into the hash. Unlike
	// json.Marshal, which returns a new slice as big as the body, an Encoder
	// builds its output in a buffer from a pool, and gives the buffer back
	// afterwards. A busy gateway reuses a few buffers instead of allocating
	// one for every request.
	h := sha256.New()
	h.Write(head)
	if err := json.NewEncoder(h).Encode(doc); err != nil {
		// Encoding what the decoder produced can't fail. But a body that was
		// only half hashed could share its key with another request, so
		// there is no key at all.
		return "", false
	}

	// The braces make the tenant ID the key's Redis Cluster hash tag, as in
	// ratelimit: all of a tenant's entries live in one slot.
	return "cache:{" + tenantID + "}:" + hex.EncodeToString(h.Sum(nil)), true
}

// appendPart appends s to b behind its length, as 8 bytes, most significant
// first.
//
// Go note: the append functions return the grown slice, which the caller must
// keep, as with the built-in append. append(b, s...) takes a string as well
// as a slice, and adds the string's bytes.
func appendPart(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint64(b, uint64(len(s)))
	return append(b, s...)
}

// decodeBody decodes body into the JSON value it holds: maps, slices, strings,
// json.Numbers, bools and nil. It reports ok == false when body can't be keyed
// safely (see Key).
func decodeBody(body []byte) (doc any, ok bool) {
	if !utf8.Valid(body) {
		return nil, false
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	// Go note: decoding JSON into an `any` makes every number a float64, which
	// holds an integer exactly only up to 2^53: 9007199254740993 and
	// 9007199254740992 would both come out as ...992. UseNumber makes the
	// decoder return each number as a json.Number instead, a string of its
	// digits as written, which the encoder writes back unchanged.
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil { // an empty body is io.EOF
		return nil, false
	}
	// Decode stops after the first value. Token reports io.EOF only when
	// nothing but whitespace is left. dec.More wouldn't do: it asks whether
	// the array or object being read goes on, and at the top level it says no
	// before a stray ] or }.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	if hasLoneSurrogate(body) {
		return nil, false
	}
	return doc, true
}

// hasLoneSurrogate reports whether body has a \u escape of a UTF-16 surrogate
// that isn't one half of a valid pair. encoding/json turns each into U+FFFD.
// body must be valid JSON, so every backslash starts an escape, and \u is
// followed by four hex digits.
func hasLoneSurrogate(body []byte) bool {
	for {
		i := bytes.IndexByte(body, '\\')
		if i < 0 {
			return false
		}
		body = body[i:] // body[0] is a backslash
		if body[1] != 'u' {
			body = body[2:] // \n, \", \\ and the other one-character escapes
			continue
		}
		r := hex4(body[2:])
		body = body[6:]
		if !utf16.IsSurrogate(r) {
			continue
		}
		// A surrogate belongs to a pair: a high one, then a low one, at once.
		// DecodeRune returns U+FFFD for anything else, as the decoder does.
		if len(body) < 6 || body[0] != '\\' || body[1] != 'u' ||
			utf16.DecodeRune(r, hex4(body[2:])) == utf8.RuneError {
			return true
		}
		body = body[6:]
	}
}

// hex4 reads the four hex digits at the start of b, which the JSON decoder has
// already checked are there.
func hex4(b []byte) rune {
	n, _ := strconv.ParseUint(string(b[:4]), 16, 16)
	return rune(n)
}
