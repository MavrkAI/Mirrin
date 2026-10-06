package logs

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// IssuesURL is where a problem report goes, once its owner has read it.
const IssuesURL = "https://github.com/MavrkAI/Mirrin/issues/new?template=bug.yml"

// Report is a problem report: what someone helping needs to see (versions,
// self-checks, settings, recent log lines) and nothing private. The owner
// reads it before sharing it; Text takes secrets out and masks personal
// details, whatever the sections hold.
type Report struct {
	Made     time.Time
	Version  string
	Sections []Section
}

// Section is one part of a report.
type Section struct {
	Title string
	Lines []string
}

// Add appends a section.
func (r *Report) Add(title string, lines ...string) {
	r.Sections = append(r.Sections, Section{Title: title, Lines: lines})
}

// Text renders the report with every secret hidden and personal details
// (email addresses, phone numbers, the account name in paths) masked.
func (r *Report) Text(red *Redactor) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Mirrin problem report\nMade %s by mirrin %s.\n\n", r.Made.Format("Mon 2 Jan 2006 15:04 MST"), r.Version)
	b.WriteString(strings.Join([]string{
		"Nothing in this file has been sent anywhere. Read it before you share it.",
		"Keys, tokens and passwords are replaced by [hidden]. No messages, facts or",
		"memories are included, only counts. Email addresses and phone numbers are",
		"partly masked. Delete anything else you'd rather not share.",
	}, "\n"))
	b.WriteString("\n")
	for _, s := range r.Sections {
		fmt.Fprintf(&b, "\n== %s\n", s.Title)
		for _, l := range s.Lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	return Scrub(b.String(), red)
}

var (
	reEmail = regexp.MustCompile(`([A-Za-z0-9._%+-])[A-Za-z0-9._%+-]*@([A-Za-z0-9.-]+\.[A-Za-z]{2,})`)
	// A run of nine or more digits is a phone number or an account id; the
	// last two stay so the owner can tell which. Dates and times are shorter.
	rePhone = regexp.MustCompile(`\+?\d{7,}(\d{2})\b`)
)

// Scrub hides secrets and masks personal details in text meant to leave the
// machine.
func Scrub(s string, red *Redactor) string {
	s = red.String(s)
	s = reEmail.ReplaceAllString(s, "${1}•••@${2}")
	s = rePhone.ReplaceAllString(s, "•••••${1}")
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

// personalKeys are settings that say who someone is or where they are, and
// the backup's keys: anyone holding backup.recipient can plant snapshots
// (it is kept out of an identity export for that), and recovery_pub names
// the backup's folder.
var personalKeys = map[string]bool{
	"about": true, "owner": true, "account": true, "account_sid": true, "address": true,
	"from": true, "from_name": true, "email": true, "username": true, "user_id": true,
	"nick": true, "latitude": true, "longitude": true, "public_url": true,
	"honorific": true, "recipient": true, "recovery_pub": true,
}

// personalLists are lists of names: the people and places the owner taught
// voice recognition to expect, say.
var personalLists = map[string]bool{"vocabulary": true, "owners": true, "owner": true}

// ConfigText is the config as YAML with every secret and personal detail
// replaced by [hidden] (settings that only name where a secret is kept, such
// as api_key_env, are shown). Empty settings are left out to keep it short.
func ConfigText(cfg *config.Config) (string, error) {
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	if len(doc.Content) > 0 {
		hideNode(doc.Content[0], nil)
		pruneEmpty(doc.Content[0])
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// hideNode walks a YAML mapping, replacing secret and personal values.
func hideNode(n *yaml.Node, path []string) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i].Value, n.Content[i+1]
			p := append(append([]string(nil), path...), key)
			switch {
			case key == "env" && len(path) > 0: // an MCP server's environment: all of it
				hideAll(val)
			case key == "name" && len(path) == 1 && path[0] == "user":
				hideScalar(val)
			case val.Kind == yaml.ScalarNode && (SecretName(key) || personalKeys[key]):
				hideScalar(val)
			case val.Kind == yaml.SequenceNode && personalLists[key]:
				hideAll(val)
			case val.Kind == yaml.SequenceNode && key == "args": // an MCP server's command line
				hideSecretArgs(val)
			default:
				hideNode(val, p)
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			hideNode(c, path)
		}
	}
}

// hideSecretArgs hides the secrets in a command line: the value after an
// option named like one ("--token abc"), and the value part of
// "--api-key=abc" or "NAME_SECRET=abc".
func hideSecretArgs(n *yaml.Node) {
	next := false
	for _, c := range n.Content {
		if c.Kind != yaml.ScalarNode {
			continue
		}
		if next && !strings.HasPrefix(c.Value, "-") {
			hideScalar(c)
			next = false
			continue
		}
		next = false
		name, value, hasValue := strings.Cut(c.Value, "=")
		if !secretArg(name) {
			continue
		}
		if !hasValue {
			next = strings.HasPrefix(name, "-")
		} else if value != "" {
			c.Value, c.Tag, c.Style = name+"="+Hidden, "!!str", 0
		}
	}
}

// secretArg reports whether a command-line option or variable name is one a
// secret is passed in.
func secretArg(name string) bool {
	n := strings.TrimLeft(name, "-")
	return n != "" && SecretName(strings.ReplaceAll(n, "-", "_"))
}

func hideScalar(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.Value != "" && n.Value != "0" && n.Tag != "!!bool" {
		n.Value, n.Tag, n.Style = Hidden, "!!str", 0
	}
}

func hideAll(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode {
		hideScalar(n)
		return
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			continue // keep the names, hide the values
		}
		hideAll(c)
	}
}

// pruneEmpty drops empty strings, empty lists and maps, and mappings left
// empty by that, so the report shows what was actually set.
func pruneEmpty(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.MappingNode:
		kept := n.Content[:0]
		for i := 0; i+1 < len(n.Content); i += 2 {
			if !pruneEmpty(n.Content[i+1]) {
				kept = append(kept, n.Content[i], n.Content[i+1])
			}
		}
		n.Content = kept
		return len(n.Content) == 0
	case yaml.SequenceNode:
		kept := n.Content[:0]
		for _, c := range n.Content {
			if !pruneEmpty(c) {
				kept = append(kept, c)
			}
		}
		n.Content = kept
		return len(n.Content) == 0
	case yaml.ScalarNode:
		return n.Value == "" || n.Tag == "!!null"
	}
	return false
}
