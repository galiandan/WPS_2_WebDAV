package wps

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestDecodePYValuePreservesOrderAndDuplicates(t *testing.T) {
	decoded, ok := decodePYValue([]byte(`{"a":1,"b":{"c":2},"a":3,"d":[true,null,1.5]}`))
	if !ok {
		t.Fatal("decode failed")
	}
	object, isObject := decoded.(*pyObject)
	if !isObject {
		t.Fatalf("decoded %T, want *pyObject", decoded)
	}
	if got := object.keys; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "d" {
		t.Fatalf("keys = %v, want [a b d] with the first position kept", got)
	}
	if object.values["a"] != json.Number("3") {
		t.Fatalf("duplicate key must keep the last value, got %v", object.values["a"])
	}
	nested := object.values["b"].(*pyObject)
	if nested.values["c"] != json.Number("2") {
		t.Fatalf("nested = %v", nested.values)
	}
	items := object.values["d"].([]any)
	if items[0] != true || items[1] != nil || items[2] != json.Number("1.5") {
		t.Fatalf("items = %v", items)
	}

	if _, ok := decodePYValue([]byte(`{broken`)); ok {
		t.Fatal("broken JSON must not decode")
	}
	if _, ok := decodePYValue([]byte(`{} trailing`)); ok {
		t.Fatal("trailing data must not decode")
	}
	if _, ok := decodePYValue([]byte(``)); ok {
		t.Fatal("empty body must not decode")
	}
}

func TestDumpPYValueMatchesPythonJSONDumps(t *testing.T) {
	decoded, ok := decodePYValue([]byte(`{"a":"中文😀","b":1.0,"c":[true,null],"d":{"e":"q\"x\t"}}`))
	if !ok {
		t.Fatal("decode failed")
	}
	dumped, err := dumpPYValue(decoded)
	if err != nil {
		t.Fatalf("dump failed: %v", err)
	}
	want := `{"a":"\u4e2d\u6587\ud83d\ude00","b":1.0,"c":[true,null],"d":{"e":"q\"x\t"}}`
	if string(dumped) != want {
		t.Fatalf("dumped = %q, want %q", dumped, want)
	}

	decoded, _ = decodePYValue([]byte(`{"z":1,"a":2}`))
	dumped, err = dumpPYValue(decoded)
	if err != nil {
		t.Fatalf("dump failed: %v", err)
	}
	if string(dumped) != `{"z":1,"a":2}` {
		t.Fatalf("key order not preserved: %q", dumped)
	}
}

func TestRefreshJSONBody(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want []byte // nil means unchanged
	}{
		{"empty body", nil, nil},
		{"empty bytes", []byte{}, nil},
		{"not json", []byte(`junk`), nil},
		{"array body", []byte(`[1,2]`), nil},
		{"missing field", []byte(`{"a":1}`), nil},
		{"non-string field", []byte(`{"csrfmiddlewaretoken":5}`), nil},
		{"nested field", []byte(`{"x":{"csrfmiddlewaretoken":"old"}}`), nil},
		{
			"replaces csrf and keeps order",
			[]byte(`{"groupid":1,"csrfmiddlewaretoken":"old","name":"中"}`),
			[]byte(`{"groupid":1,"csrfmiddlewaretoken":"new","name":"\u4e2d"}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := refreshJSONBody(test.body, "new")
			want := test.want
			if want == nil {
				want = test.body
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
	if string(refreshJSONBody([]byte(`{"csrfmiddlewaretoken":"old"}`), "")) != `{"csrfmiddlewaretoken":"old"}` {
		t.Fatal("empty csrf token must leave the body untouched")
	}
}
