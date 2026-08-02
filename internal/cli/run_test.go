package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpAndSiteList(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"site", "list"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "tjupt") || errOut.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestCredentialMustNotBeAcceptedAsArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"site", "status", "tjupt"}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--cookie-stdin") {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
}
