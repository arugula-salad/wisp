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
	// One nested in another is allowed: a host belongs to the most specific.
	if got, err := parseURLDomains("arugula.test,games.arugula.test"); err != nil || len(got) != 2 {
		t.Fatalf("nested: got %v, %v", got, err)
	}
	for _, bad := range []string{"", " , ", "x.test,x.test", "x.test,X.test."} {
		if _, err := parseURLDomains(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
