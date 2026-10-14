package backend

import (
	"strings"
	"testing"
	"time"
)

// TestSignV4AWSExample verifies our signer against the worked example in the
// AWS "Signature Version 4" documentation (GET Object with a Range header).
func TestSignV4AWSExample(t *testing.T) {
	tm := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	headers := map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"x-amz-date":           "20130524T000000Z",
	}
	signedHeaders := []string{"host", "range", "x-amz-content-sha256", "x-amz-date"}
	auth := signV4(
		"GET",
		"/test.txt",
		"",
		headers,
		signedHeaders,
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		tm,
		"us-east-1",
		"s3",
		"AKIAIOSFODNN7EXAMPLE",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	)
	want := "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if !strings.Contains(auth, "Signature="+want) {
		t.Fatalf("signature mismatch\n got: %s\nwant: %s", auth, want)
	}
	if !strings.Contains(auth, "Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request") {
		t.Fatalf("bad credential scope: %s", auth)
	}
}

func TestAWSUriEncode(t *testing.T) {
	cases := map[string]string{
		"test.txt":     "test.txt",
		"a b":          "a%20b",
		"a/b":          "a/b",
		"a+b":          "a%2Bb",
		"folder/x y z": "folder/x%20y%20z",
	}
	for in, want := range cases {
		if got := awsURIEncode(in, false); got != want {
			t.Errorf("awsURIEncode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := awsURIEncode("a/b", true); got != "a%2Fb" {
		t.Errorf("encodeSlash: got %q", got)
	}
}
