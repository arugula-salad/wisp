package main

import (
	"os/exec"
	"reflect"
	"testing"
)

func TestParseSudo(t *testing.T) {
	cases := []struct {
		args []string
		want sudoOpts
	}{
		{[]string{"apt-get", "install", "-y", "git"}, sudoOpts{user: "root", cmd: []string{"apt-get", "install", "-y", "git"}}},
		{[]string{"-u", "node", "npm", "-v"}, sudoOpts{user: "node", cmd: []string{"npm", "-v"}}},
		{[]string{"-unode", "id"}, sudoOpts{user: "node", cmd: []string{"id"}}},
		{[]string{"--user=node", "id"}, sudoOpts{user: "node", cmd: []string{"id"}}},
		{[]string{"-nE", "--", "-x"}, sudoOpts{user: "root", keepEnv: true, cmd: []string{"-x"}}},
		{[]string{"-i"}, sudoOpts{user: "root", login: true}},
		{[]string{"-s", "echo", "hi"}, sudoOpts{user: "root", shell: true, cmd: []string{"echo", "hi"}}},
		{[]string{"-v"}, sudoOpts{user: "root", justCheck: true}},
		{[]string{"-k"}, sudoOpts{user: "root", justCheck: true}},
		{[]string{"-l"}, sudoOpts{user: "root", list: true, justCheck: true}},
		{[]string{"-H", "ls", "-la"}, sudoOpts{user: "root", cmd: []string{"ls", "-la"}}},
	}
	for _, c := range cases {
		got, err := parseSudo(c.args)
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q = %+v, want %+v", c.args, got, c.want)
		}
	}
	for _, bad := range [][]string{{"-g", "x", "id"}, {"-u"}, {"--chroot=/", "id"}, {"-b", "id"}} {
		if _, err := parseSudo(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestShellQuoteRoundTrips(t *testing.T) {
	args := []string{"printf", "%s|", "a b", "it's", "$HOME", "", "--x=1", "`id`"}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	got, err := exec.Command("sh", "-c", shellQuote(args)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "a b|it's|$HOME||--x=1|`id`|"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
