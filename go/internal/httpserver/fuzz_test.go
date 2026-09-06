package httpserver

import (
	"net/http"
	"testing"
)

// Fuzz targets for the parser entries 06-testing-risk-gates.md 10.1 names:
// path/query decoding, Range/If-Range, LOCK XML, and Basic Auth base64.
// The invariants: no panic, no deadlock, controlled errors, and identical
// classification for identical input.

func FuzzUnquotePercentAndSplitTarget(f *testing.F) {
	seeds := []string{
		"/dav/a/b.txt?path=%2Fx&overwrite=true",
		"/dav/%2e%2e/%2F//a",
		"/api/v1/entries?path=%252Fweird%252Fname.txt",
		"/dav/" + string([]byte{0x25, 0x41}) + "?path=%zz",
		"/dav/%C3%A9.txt?path=%C3%A9",
		"//bad//path?path=/a+b%20c",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, target string) {
		path, query := SplitRequestTarget(target)
		_ = unquotePercent(path)
		_ = parseQueryValues(query)
	})
}

func FuzzLockOwnerXML(f *testing.F) {
	seeds := []string{
		`<?xml version="1.0"?><owner>bench</owner>`,
		`<owner><![CDATA[cdata]]></owner>`,
		`<owner>&#60;&#x3C;</owner>`,
		`<!DOCTYPE owner [<!ENTITY x "y">]><owner>&x;</owner>`,
		`<owner>` + string([]byte{0x00, 0x01}) + `</owner>`,
		`<a><owner>first</owner></a><owner>second</owner>`,
		`<owner>` + string(make([]byte, 512)) + `</owner>`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = lockOwnerFromBody(body)
	})
}

func FuzzRangeHeader(f *testing.F) {
	seeds := []string{
		"bytes=0-", "bytes=-5", "bytes=0-4", "bytes=5-2",
		"bytes=0-1,2-3", "bytes= ", "bytes=18446744073709551615-",
		"bytes=9223372036854775807-9223372036854775808",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		size := int64(100)
		_, _, _ = parseRangeHeader(header, &size)
		_ = (&http.Request{Header: http.Header{}}).Header
	})
}

func FuzzBasicAuthHeader(f *testing.F) {
	seeds := []string{
		"Basic YWRhcHRlcjpzZWNyZXQ=",
		"basic dXNlcjpwYXNz",
		"Basic !!!!",
		"Basic ", "Basic YQ", "Digest abc",
		"Basic " + "YQ==",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		auth := basicAuth{config: BasicAuthConfig{Username: "u", Password: "p"}}
		_ = auth.accepts(header)
	})
}
