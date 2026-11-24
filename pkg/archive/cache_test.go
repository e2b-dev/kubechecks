package archive

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An archive no check has taken for longer than the TTL is removed with its
// commit's directory and nothing else, whether its files sit in a top-level
// directory or not, and one taken since is kept. Nothing releases an archive,
// so the reference count every entry carries must not hold it.
func TestCleanupStaleArchives_RemovesByAgeAlone(t *testing.T) {
	baseDir := t.TempDir()
	c := &Cache{entries: make(map[string]*CacheEntry), baseDir: baseDir, ttl: time.Hour}
	add := func(sha, topLevelDir string, lastUsed time.Time) string {
		extracted := filepath.Join(baseDir, sha, topLevelDir)
		require.NoError(t, os.MkdirAll(extracted, 0o755))
		c.entries[sha] = &CacheEntry{extractedPath: extracted, lastUsed: lastUsed, refCount: 1}

		return extracted
	}
	add("stale", "owner-repo-stale", time.Now().Add(-2*time.Hour))
	add("stale-flat", "", time.Now().Add(-2*time.Hour))
	fresh := add("fresh", "owner-repo-fresh", time.Now().Add(-time.Minute))

	c.cleanupStaleArchives()

	assert.NoDirExists(t, filepath.Join(baseDir, "stale"))
	assert.NoDirExists(t, filepath.Join(baseDir, "stale-flat"))
	assert.DirExists(t, fresh)
	assert.Equal(t, []string{"fresh"}, slices.Collect(maps.Keys(c.entries)))
}
