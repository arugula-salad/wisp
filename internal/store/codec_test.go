package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// legacySprite is store.Sprite as it was before the split into Record and
// SpriteMeta (origin/main at 2a8a523), copied verbatim: what every
// sprite.json on disk today was written by, and what an older wispd reads.
type legacySprite struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Config        Config            `json:"config"`
	Environment   map[string]string `json:"environment,omitempty"`
	URLSettings   URLSettings       `json:"url_settings"`
	Labels        []string          `json:"labels,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	LastRunningAt *time.Time        `json:"last_running_at,omitempty"`
	LastWarmingAt *time.Time        `json:"last_warming_at,omitempty"`
	// NetIndex is this sprite's host number within the sprite network (whose
	// prefix belongs to the host bridge, not to us). 0 means unassigned.
	NetIndex int `json:"net_index"`
	// BootIP is the address the guest configured at its last cold boot. A warm
	// snapshot taken under a different address is useless and gets discarded.
	BootIP         string       `json:"boot_ip,omitempty"`
	Checkpoints    []Checkpoint `json:"checkpoints,omitempty"`
	NextCheckpoint int          `json:"next_checkpoint"`
	// NextAuto numbers auto-<n> checkpoints, which never consume a v<n>.
	NextAuto int `json:"next_auto,omitempty"`
	// Lineage is the History a checkpoint taken now would get: the checkpoint
	// the live filesystem was last saved as or restored from, then its ancestors.
	Lineage []string `json:"lineage,omitempty"`

	// Image is the container image the sprite's disk was made from, pinned
	// by digest where the registry gave one; empty for the base image or a clone.
	Image string `json:"image,omitempty"`

	// Mounts maps a checkpoint slot to the checkpoint whose image backs it, for as
	// long as the VM (or its warm snapshot, which records the drive paths) lives.
	Mounts map[int]string `json:"mounts,omitempty"`

	// NetworkRules is the egress policy as the client wrote it. Empty means unrestricted.
	NetworkRules []NetworkRule `json:"network_rules,omitempty"`

	// A nil policy is upstream's default: unrestricted.
	Privileges *PrivilegesPolicy `json:"privileges_policy,omitempty"`
	Resources  *ResourcesPolicy  `json:"resources_policy,omitempty"`

	// URLDomain is the domain this sprite's URL is under (<name>.<URLDomain>),
	// one of wispd's --url-domain list. Empty is the first of them, which is
	// what every sprite made before there could be several has.
	URLDomain string `json:"url_domain,omitempty"`

	// ParentID is the sprite that created this one from inside. An ID rather than
	// a name, because names are reusable after a delete.
	ParentID string `json:"parent_id,omitempty"`
	// A nil spawn policy is the default: the sprite cannot create sprites.
	Spawn *SpawnPolicy `json:"spawn_policy,omitempty"`

	// Domains are custom hostnames served as this sprite's URL (domains.go). A
	// domain belongs to at most one sprite; clones do not inherit them.
	Domains []string `json:"domains,omitempty"`

	// ExpiresAt is the workspace lease: when it passes, the sprite is deleted,
	// disk and all. nil is the default and means the sprite lives until someone
	// deletes it, because losing a workspace to an expiry nobody asked for would
	// be worse than leaving a stale one on the volume. Persisted like the rest,
	// so a lease outlives the daemon that granted it.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Protected holds off that deletion without forgetting the deadline, for the
	// sprite somebody turns out to still be using.
	Protected bool `json:"protected,omitempty"`
}

// fill sets every field reachable from v to a value that is not its zero, so
// that a field the codec forgets shows up in a comparison. Maps and slices get
// one element.
func fill(v reflect.Value, seed *int) {
	*seed++
	n := *seed
	switch v.Kind() {
	case reflect.String:
		v.SetString("s" + string(rune('a'+n%26)) + time.Duration(n).String())
	case reflect.Int, reflect.Int64:
		if v.Type() == reflect.TypeOf(time.Duration(0)) {
			v.SetInt(int64(n) * int64(time.Second))
		} else {
			v.SetInt(int64(n))
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), seed)
	case reflect.Slice:
		if v.Type() == reflect.TypeOf(json.RawMessage{}) {
			v.SetBytes([]byte(`{"k":1}`))
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), seed)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, seed)
		fill(e, seed)
		v.SetMapIndex(k, e)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, n, time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			fill(v.Field(i), seed)
		}
	default:
		panic("fill: unhandled kind " + v.Kind().String())
	}
}

func jsonFields(t reflect.Type, into map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			jsonFields(f.Type, into)
			continue
		}
		into[f.Tag.Get("json")] = f.Type
	}
}

// The on-disk layout lists every field of both halves, once, with the same
// tag and type, so nothing is dropped on the way to disk or back.
func TestSpriteJSONFieldsMatch(t *testing.T) {
	halves, flat := map[string]reflect.Type{}, map[string]reflect.Type{}
	jsonFields(reflect.TypeOf(Sprite{}), halves)
	jsonFields(reflect.TypeOf(spriteJSON{}), flat)
	if !reflect.DeepEqual(halves, flat) {
		t.Fatalf("Record+SpriteMeta fields\n %v\ndiffer from spriteJSON's\n %v", halves, flat)
	}
	// And the legacy struct is spriteJSON without the fields added since.
	legacy := map[string]reflect.Type{}
	jsonFields(reflect.TypeOf(legacySprite{}), legacy)
	for _, k := range []string{"api,omitempty", "hostname,omitempty", "ext,omitempty"} {
		delete(flat, k)
	}
	if !reflect.DeepEqual(legacy, flat) {
		t.Fatalf("legacy fields\n %v\ndiffer from spriteJSON's\n %v", legacy, flat)
	}
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A file the old type wrote, every field set, decodes into the new type with
// the same values and encodes back to the same bytes; and the old type reads
// what the new one writes.
func TestSpriteJSONMatchesLegacy(t *testing.T) {
	var old legacySprite
	seed := 0
	fill(reflect.ValueOf(&old).Elem(), &seed)
	for _, tc := range []struct {
		name string
		old  legacySprite
	}{{"full", old}, {"empty", legacySprite{}}, {"minimal", legacySprite{ID: "abc", Name: "n", NetIndex: 2}}} {
		t.Run(tc.name, func(t *testing.T) {
			onDisk := encode(t, tc.old)
			var sp Sprite
			if err := json.Unmarshal(onDisk, &sp); err != nil {
				t.Fatal(err)
			}
			if again := encode(t, sp); !bytes.Equal(again, onDisk) {
				t.Fatalf("re-encoded differently:\n%s\nwant\n%s", again, onDisk)
			}
			var back legacySprite
			if err := json.Unmarshal(encode(t, sp), &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, tc.old) {
				t.Fatalf("old type read back %+v\nwant %+v", back, tc.old)
			}
			if sp.Name != tc.old.Name || sp.ID != tc.old.ID || !reflect.DeepEqual(sp.Checkpoints, tc.old.Checkpoints) ||
				!reflect.DeepEqual(sp.Spawn, tc.old.Spawn) || !reflect.DeepEqual(sp.ExpiresAt, tc.old.ExpiresAt) {
				t.Fatalf("decoded %+v from %+v", sp, tc.old)
			}
			if sp.Hostname != tc.old.Name {
				t.Fatalf("hostname = %q, want the name %q", sp.Hostname, tc.old.Name)
			}
		})
	}
}

// Every field of the new type survives a round trip, the ones only another
// API sets included.
func TestSpriteJSONRoundTrip(t *testing.T) {
	var sp Sprite
	seed := 0
	fill(reflect.ValueOf(&sp).Elem(), &seed)
	var back Sprite
	if err := json.Unmarshal(encode(t, sp), &back); err != nil {
		t.Fatal(err)
	}
	// An Ext value comes back as the indented form it was written in.
	for k, v := range back.Ext {
		var c bytes.Buffer
		json.Compact(&c, v)
		back.Ext[k] = c.Bytes()
	}
	if !reflect.DeepEqual(back, sp) {
		t.Fatalf("round trip\n %+v\nwant\n %+v", back, sp)
	}
}

// A sprite's hostname is its name, and is not written out as such; one set to
// something else is.
func TestSpriteHostname(t *testing.T) {
	sp := Sprite{Record: Record{ID: "a", Hostname: "web"}, SpriteMeta: SpriteMeta{Name: "web"}}
	if b := encode(t, sp); bytes.Contains(b, []byte("hostname")) {
		t.Fatalf("default hostname written: %s", b)
	}
	sp.Hostname = "other"
	var back Sprite
	json.Unmarshal(encode(t, sp), &back)
	if back.Hostname != "other" {
		t.Fatalf("hostname = %q", back.Hostname)
	}
	// Another API's record has no name to fall back on.
	e2b := Sprite{Record: Record{ID: "b", API: "e2b"}}
	json.Unmarshal(encode(t, e2b), &back)
	if back.Hostname != "" || back.API != "e2b" {
		t.Fatalf("e2b record read back as %+v", back.Record)
	}
}
