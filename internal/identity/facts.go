package identity

import (
	"context"
	"math"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/memory"
)

type factsDoc struct {
	Portrait string     `yaml:"portrait,omitempty"`
	Facts    []factItem `yaml:"facts"`
}

type factItem struct {
	Subject string `yaml:"subject"`
	Content string `yaml:"content"`
}

// allFacts is "no limit" for AllFacts: a twin's memory travels whole.
const allFacts = math.MaxInt32

// exportFacts serialises long-term memory (facts + portrait) without conversations.
func exportFacts(dbPath string) ([]byte, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}
	store, err := memory.Open(filepath.Dir(dbPath))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	ctx := context.Background()
	facts, err := store.AllFacts(ctx, allFacts)
	if err != nil {
		return nil, err
	}
	doc := factsDoc{}
	if p, err := store.GetPortrait(ctx); err == nil {
		doc.Portrait = p.Text
	}
	for _, f := range facts {
		doc.Facts = append(doc.Facts, factItem{f.Subject, f.Content})
	}
	return yaml.Marshal(doc)
}

// parseFacts reads an exported facts document.
func parseFacts(b []byte) (factsDoc, error) {
	var doc factsDoc
	err := yaml.Unmarshal(b, &doc)
	return doc, err
}

// importFacts merges facts and the portrait into the memory in dataDir (no duplicates).
func importFacts(dataDir string, doc factsDoc) error {
	store, err := memory.Open(dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	existing, err := store.AllFacts(ctx, allFacts)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, f := range existing {
		have[f.Subject+"|"+f.Content] = true
	}
	for _, f := range doc.Facts {
		if !have[f.Subject+"|"+f.Content] {
			if _, err := store.Remember(ctx, f.Subject, f.Content, "imported"); err != nil {
				return err
			}
			have[f.Subject+"|"+f.Content] = true
		}
	}
	if doc.Portrait != "" {
		return store.SetPortrait(ctx, doc.Portrait)
	}
	return nil
}
