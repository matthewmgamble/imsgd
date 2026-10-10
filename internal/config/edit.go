package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// NewAPIKey returns a random API key.
func NewAPIKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "imsgd_" + base64.RawURLEncoding.EncodeToString(b)
}

// loadNode reads the config file as a YAML node tree (preserving comments),
// or returns a fresh mapping if the file does not exist.
func loadNode(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(bytes.TrimSpace(data)) == 0) {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: top level must be a mapping", path)
	}
	return &doc, nil
}

// keysNode returns the api_keys sequence, creating it if needed.
func keysNode(doc *yaml.Node) *yaml.Node {
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "api_keys" {
			seq := root.Content[i+1]
			if seq.Kind != yaml.SequenceNode {
				*seq = yaml.Node{Kind: yaml.SequenceNode}
			}
			return seq
		}
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "api_keys"}, seq)
	return seq
}

func entryName(n *yaml.Node) string {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "name" {
			return n.Content[i+1].Value
		}
	}
	return ""
}

// writeNode writes the document atomically with owner-only permissions.
func writeNode(path string, doc *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AddKey appends a new named API key to the config file and returns it.
func AddKey(path, name string) (string, error) {
	if name == "" {
		return "", errors.New("key name is required")
	}
	doc, err := loadNode(path)
	if err != nil {
		return "", err
	}
	seq := keysNode(doc)
	for _, e := range seq.Content {
		if entryName(e) == name {
			return "", fmt.Errorf("a key named %q already exists", name)
		}
	}
	key := NewAPIKey()
	seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "name"}, {Kind: yaml.ScalarNode, Value: name},
		{Kind: yaml.ScalarNode, Value: "key"}, {Kind: yaml.ScalarNode, Value: key, Style: yaml.DoubleQuotedStyle},
	}})
	return key, writeNode(path, doc)
}

// RemoveKey deletes the named API key from the config file.
func RemoveKey(path, name string) error {
	doc, err := loadNode(path)
	if err != nil {
		return err
	}
	seq := keysNode(doc)
	for i, e := range seq.Content {
		if entryName(e) == name {
			seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
			return writeNode(path, doc)
		}
	}
	return fmt.Errorf("no key named %q", name)
}

// KeyNames lists the configured key names.
func KeyNames(path string) ([]string, error) {
	doc, err := loadNode(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range keysNode(doc).Content {
		names = append(names, entryName(e))
	}
	return names, nil
}
