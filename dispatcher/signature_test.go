package dispatcher

import "testing"

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	good := sign(t, testSecret, body)

	if !VerifySignature(testSecret, body, good) {
		t.Error("a correct signature must verify")
	}
	if VerifySignature(testSecret, append(append([]byte{}, body...), ' '), good) {
		t.Error("a signature over a different body must not verify")
	}
	if VerifySignature(testSecret, body, "") {
		t.Error("a missing signature must not verify")
	}
}

func TestVerifySignatureRejectsMalformedAndForeignHeaders(t *testing.T) {
	body := []byte(`{"a":1}`)
	good := sign(t, testSecret, body)
	hexOnly := good[len("sha256="):]

	cases := map[string]string{
		"wrong secret":      sign(t, "another-secret-another-secret", body),
		"no scheme prefix":  hexOnly,
		"sha1 scheme":       "sha1=" + hexOnly,
		"uppercase scheme":  "SHA256=" + hexOnly,
		"truncated":         good[:len(good)-2],
		"trailing newline":  good + "\n",
		"just the prefix":   "sha256=",
		"hex in upper case": "sha256=" + upperHex(hexOnly),
	}
	for name, header := range cases {
		if VerifySignature(testSecret, body, header) {
			t.Errorf("%s: must not verify", name)
		}
	}
}

func upperHex(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'f' {
			out[i] = c - 'a' + 'A'
		}
	}
	return string(out)
}
