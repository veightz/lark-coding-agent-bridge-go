// Package media downloads message attachments (images/files) into the
// profile's media cache so the local agent can read them from disk.
package media

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"lark-coding-agent-bridge-go/internal/lark"
)

// Resource is one attachment reference extracted from a message.
type Resource struct {
	FileKey string
	Type    string // "image" or "file"
	Name    string // file name (file messages only)
}

// Attachment is a downloaded local file.
type Attachment struct {
	Path string
	Kind string
	Name string
}

// Cache stores downloads under a profile media directory.
type Cache struct {
	dir string
	mu  sync.Mutex
	// name counters per message for dedupe (1.png, 2.png ... when unnamed)
	seq map[string]int
}

func NewCache(dir string) *Cache {
	return &Cache{dir: dir, seq: map[string]int{}}
}

// Download fetches all resources of a message; failures are skipped so one
// bad attachment doesn't sink the run.
func (c *Cache) Download(ctx context.Context, client *lark.Client, messageID string, resources []Resource) []Attachment {
	var out []Attachment
	for _, r := range resources {
		att, err := c.downloadOne(ctx, client, messageID, r)
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

func (c *Cache) downloadOne(ctx context.Context, client *lark.Client, messageID string, r Resource) (Attachment, error) {
	rc, fileName, err := client.DownloadResource(ctx, messageID, r.FileKey, r.Type)
	if err != nil {
		return Attachment{}, err
	}
	defer rc.Close()

	name := r.Name
	if name == "" {
		name = fileName
	}
	if name == "" {
		name = r.FileKey
		if r.Type == "image" {
			name += ".png"
		}
	}
	name = sanitizeName(name)

	c.mu.Lock()
	c.seq[messageID]++
	seq := c.seq[messageID]
	c.mu.Unlock()

	dir := filepath.Join(c.dir, messageID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Attachment{}, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s", seq, name))

	f, err := os.Create(path)
	if err != nil {
		return Attachment{}, err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		return Attachment{}, err
	}
	return Attachment{Path: path, Kind: r.Type, Name: name}, nil
}

func sanitizeName(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, "..", "_")
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "attachment"
	}
	return name
}
