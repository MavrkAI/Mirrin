package memory

import (
	"context"
	"fmt"
	"testing"
)

func TestPromptFactsKeepsEverythingWhileItFits(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for i := 0; i < 5; i++ {
		if _, err := s.Remember(ctx, "user", fmt.Sprintf("Fact number %d.", i), "t"); err != nil {
			t.Fatal(err)
		}
	}
	got, total, err := s.PromptFacts(ctx, "hello", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(got) != 5 || got[0].Content != "Fact number 0." {
		t.Fatalf("want all five, oldest first; got %d of %d: %+v", len(got), total, got)
	}
}

func TestPromptFactsPrefersRelevantAndRecentPastTheBudget(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	allergy, _ := s.Remember(ctx, "family", "Mia is allergic to peanuts.", "t")
	old, _ := s.Remember(ctx, "home", "The user lives in Melbourne.", "t")
	for i := 0; i < 400; i++ {
		if _, err := s.Remember(ctx, "work", fmt.Sprintf("Project note %d about the quarterly roadmap.", i), "t"); err != nil {
			t.Fatal(err)
		}
	}
	moved, _ := s.Remember(ctx, "home", "The user moved to Sydney in September.", "t")

	cases := []struct {
		name, query string
		want, not   []int64
	}{
		{"newest fact always makes it", "what's on today", []int64{moved}, []int64{allergy, old}},
		{"an old fact related to the message comes back", "can Mia have a peanut butter sandwich?", []int64{allergy, moved}, nil},
		{"plural and singular meet", "any allergies I should know about", []int64{allergy}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, total, err := s.PromptFacts(ctx, c.query, 4000)
			if err != nil {
				t.Fatal(err)
			}
			if total != 403 || len(got) >= total {
				t.Fatalf("expected a subset of 403 facts, got %d of %d", len(got), total)
			}
			have := map[int64]bool{}
			size := 0
			for i, f := range got {
				have[f.ID] = true
				size += factCost(f)
				if i > 0 && got[i-1].ID > f.ID {
					t.Fatal("facts should be oldest first")
				}
			}
			if size > 4000 {
				t.Fatalf("over budget: %d", size)
			}
			for _, id := range c.want {
				if !have[id] {
					t.Errorf("fact #%d missing", id)
				}
			}
			for _, id := range c.not {
				if have[id] {
					t.Errorf("fact #%d should have made way for newer ones", id)
				}
			}
		})
	}
}

func TestRecallRanksByMeaningfulWords(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	bday, _ := s.Remember(ctx, "family", "Tony's mum Maria has her birthday on 3 May.", "t")
	for i := 0; i < 30; i++ {
		_, _ = s.Remember(ctx, "work", fmt.Sprintf("My team meets on Tuesdays (note %d).", i), "t")
	}
	got, err := s.Recall(ctx, "what's my mum's birthday?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != bday {
		t.Fatalf("want only the birthday fact, not every fact with \"my\" in it: %+v", got)
	}
	if got, _ := s.Recall(ctx, "mari", 5); len(got) != 1 || got[0].ID != bday {
		t.Fatalf("part of a word should still find it: %+v", got)
	}
	if got, _ := s.Recall(ctx, "the", 5); len(got) != 0 {
		t.Fatalf("a word no fact holds should find nothing, not the newest facts: %+v", got)
	}
	if got, _ := s.Recall(ctx, "my", 50); len(got) != 30 {
		t.Fatalf("a query of nothing but a little word still searches for it: %d found", len(got))
	}
	if got, _ := s.Recall(ctx, "", 5); len(got) != 5 || got[0].Content != "My team meets on Tuesdays (note 29)." {
		t.Fatalf("an empty query lists the newest facts: %+v", got)
	}
}

func TestRecallFindsShortNamesAndEverydayWords(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	ed, _ := s.Remember(ctx, "people", "Ed is Tony's older brother.", "t")
	hb, _ := s.Remember(ctx, "health", "Tony's resting heartbeat is 52.", "t")
	task, _ := s.Remember(ctx, "work", "The quarterly task list lives in Notion.", "t")
	_, _ = s.Remember(ctx, "home", "Tony married Sam in 2019 and they moved to Sydney.", "t")
	_, _ = s.Remember(ctx, "work", "Tony's manager is Priya.", "t")

	cases := []struct {
		query string
		want  []int64
	}{
		{"Ed", []int64{ed}},
		{"who is Ed?", []int64{ed}},
		{"heartbeat", []int64{hb}},
		{"task", []int64{task}},
		{"today", nil},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			got, err := s.Recall(ctx, c.query, 20)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, f := range got {
				ids = append(ids, f.ID)
			}
			if fmt.Sprint(ids) != fmt.Sprint(c.want) {
				t.Fatalf("recall(%q) = %v, want %v", c.query, ids, c.want)
			}
		})
	}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
