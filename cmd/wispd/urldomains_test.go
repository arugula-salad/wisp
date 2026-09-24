package main

import (
	"slices"
	"testing"
)

func TestParseURLDomains(t *testing.T) {
	got, err := parseURLDomains(" widgets.test , Arugula.Test. ")
	if err != nil || !slices.Equal(got, []string{"widgets.test", "arugula.test"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", " , ", "widgets.test,a.widgets.test", "x.test,x.test"} {
		if _, err := parseURLDomains(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
