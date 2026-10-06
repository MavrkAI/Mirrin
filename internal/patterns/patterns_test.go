package patterns

import "testing"

func TestFindNewsTheme(t *testing.T) {
	reqs := []string{
		"what's happening?",
		"what is the top story on the BBC website right now?",
		"set a reminder for three to call the accountant",
		"any news on the BBC this evening?",
		"what's the top news story right now?",
		"give me the news headlines please",
		"remember that I live in Melbourne",
	}
	cs := Find(reqs, 3)
	if len(cs) == 0 {
		t.Fatal("expected a news cluster")
	}
	if cs[0].Key != "new" && cs[0].Key != "bbc" && cs[0].Key != "story" && cs[0].Key != "news" {
		t.Fatalf("unexpected key %q for %v", cs[0].Key, cs[0].Requests)
	}
	if len(cs[0].Requests) < 3 {
		t.Fatalf("cluster too small: %v", cs[0].Requests)
	}
	if cs := Find([]string{"a", "b", "c"}, 3); len(cs) != 0 {
		t.Fatal("no content words should mean no clusters")
	}
}

func TestFindCountsWordsInAnyScript(t *testing.T) {
	w := words("a café, Кафе и погода")
	for _, want := range []string{"café", "кафе", "погода"} {
		if !w[want] {
			t.Fatalf("%q not counted: %v", want, w)
		}
	}
	cs := Find([]string{"book the café", "which café is open", "café hours today"}, 3)
	if len(cs) != 1 || cs[0].Key != "café" {
		t.Fatalf("want a café cluster, got %+v", cs)
	}
	cs = Find([]string{"какая погода завтра", "погода сегодня", "погода в Москве"}, 3)
	if len(cs) != 1 || cs[0].Key != "погода" {
		t.Fatalf("want a Cyrillic cluster, got %+v", cs)
	}
}
