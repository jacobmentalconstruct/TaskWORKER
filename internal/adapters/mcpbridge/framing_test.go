package mcpbridge

import (
	"bytes"
	"strings"
	"testing"
)

// The SDK ends the session (exit 1, no reply) on an envelope it
// cannot decode and answers an id above 2^53 with a different number, so both
// are screened before the SDK sees the line.
func TestEnvelopeProblem(t *testing.T) {
	deep := func(n int) string {
		return `{"jsonrpc":"2.0","id":1,"method":"m","params":` + strings.Repeat("[", n) + strings.Repeat("]", n) + `}`
	}
	accepted := []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{}}`,
		`{"jsonrpc":"2.0","id":0,"method":"m"}`,
		`{"jsonrpc":"2.0","id":-1,"method":"m"}`,
		`{"jsonrpc":"2.0","id":9007199254740991,"method":"m"}`,
		`{"jsonrpc":"2.0","id":-9007199254740991,"method":"m"}`,
		`{"jsonrpc":"2.0","id":"9007199254740993","method":"m"}`, // a string id is never rounded
		`{"jsonrpc":"2.0","id":null,"method":"m"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":3,"result":{}}`,
		`{"jsonrpc":"2.0","id":3,"error":{"code":-1,"message":"x"}}`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"n"}]`,
		`{"jsonrpc":"2.0","id":1,"method":"m","params":{"s":"[[[[ } \" ]]]]","t":"\\"}}`, // brackets in strings are not nesting
		deep(100),
		deep(511), // 511 arrays + the enclosing object = 512
	}
	for _, line := range accepted {
		if p := envelopeProblem([]byte(line)); p != "" {
			t.Errorf("must pass to the SDK (%s): %.100s", p, line)
		}
	}
	rejected := map[string]string{
		`{}`:                          "empty object",
		`{"jsonrpc":"2.0"}`:           "no method or id",
		`{"jsonrpc":"2.0","id":null}`: "response-shaped with a null id (fatal in the SDK)",
		`{"jsonrpc":"2.0","id":null,"result":{}}`:                            "response with a null id (fatal in the SDK)",
		`{"jsonrpc":"2.0","id":null,"error":{"code":-1,"message":"x"}}`:      "error response with a null id",
		`{"jsonrpc":"1.0","id":1,"method":"m"}`:                              "wrong version",
		`{"jsonrpc":2.0,"id":1,"method":"m"}`:                                "version not a string",
		`{"id":1,"method":"m"}`:                                              "no version",
		`{"jsonrpc":"2.0","id":1,"method":7}`:                                "method not a string",
		`{"jsonrpc":"2.0","id":1,"method":null}`:                             "method null",
		`{"jsonrpc":"2.0","id":{"a":1},"method":"m"}`:                        "object id",
		`{"jsonrpc":"2.0","id":[1],"method":"m"}`:                            "array id",
		`{"jsonrpc":"2.0","id":true,"method":"m"}`:                           "boolean id",
		`{"jsonrpc":"2.0","id":1.5,"method":"m"}`:                            "fractional id (the SDK answers 1)",
		`{"jsonrpc":"2.0","id":1e2,"method":"m"}`:                            "exponent id",
		`{"jsonrpc":"2.0","id":1.0,"method":"m"}`:                            "id written with a fraction",
		`{"jsonrpc":"2.0","id":-0,"method":"m"}`:                             "negative zero id",
		`{"jsonrpc":"2.0","id":9007199254740992,"method":"m"}`:               "2^53",
		`{"jsonrpc":"2.0","id":9007199254740993,"method":"m"}`:               "2^53+1 (the SDK answers ...992)",
		`{"jsonrpc":"2.0","id":-9007199254740992,"method":"m"}`:              "-2^53",
		`{"jsonrpc":"2.0","id":9223372036854775808,"method":"m"}`:            "2^63 (the SDK answers int64 min)",
		`{"jsonrpc":"2.0","id":123456789012345678901234567890,"method":"m"}`: "id beyond int64",
		`[]`:   "empty batch",
		`[{}]`: "batch with an empty object",
		`[1]`:  "batch of a non-object",
		`[{"jsonrpc":"2.0","id":1,"method":"m"},{"jsonrpc":"2.0"}]`: "one bad message spoils the batch",
		deep(512):  "513 levels",
		deep(5000): "5000 levels",
	}
	for line, why := range rejected {
		if envelopeProblem([]byte(line)) == "" {
			t.Errorf("must be rejected (%s): %.100s", why, line)
		}
	}
}

func TestFramerDropsResponsesOfCancelledRequestsOnly(t *testing.T) {
	var out bytes.Buffer
	f := newFramer(&out)
	f.track([]byte(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{}}`))
	f.track([]byte(`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{}}`))
	f.track([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":10}}`))
	// A cancel for an unknown/finished id must not poison a later reuse of it.
	f.track([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":99}}`))
	f.Write([]byte(`{"jsonrpc":"2.0","id":10,"result":{}}` + "\n"))
	f.Write([]byte(`{"jsonrpc":"2.0","id":"a","result":{}}` + "\n"))
	f.track([]byte(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{}}`))
	f.Write([]byte(`{"jsonrpc":"2.0","id":99,"result":{}}` + "\n"))
	// id reuse after the dropped response is a fresh request.
	f.track([]byte(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{}}`))
	f.Write([]byte(`{"jsonrpc":"2.0","id":10,"result":{"again":true}}` + "\n"))
	got := out.String()
	if strings.Count(got, "\n") != 3 || strings.Contains(got, `"id":10,"result":{}`) || !strings.Contains(got, `"again":true`) {
		t.Fatalf("%q", got)
	}
	if len(f.inflight) != 0 || len(f.cancelled) != 0 {
		t.Fatal("tracking state must drain", f.inflight, f.cancelled)
	}
}
