package preupgrade

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapier/kubechecks/pkg"
	"github.com/zapier/kubechecks/pkg/config"
	"github.com/zapier/kubechecks/pkg/container"
	"github.com/zapier/kubechecks/pkg/msg"
)

// A store that knows one API, removed before the version the check looks at,
// and a manifest that uses it: the check warns only if it read this store.
const (
	store  = `[{"group":"example.k8s.io","version":"v1beta1","kind":"Widget","deprecated_version":{"version_major":1,"version_minor":20},"removed_version":{"version_major":1,"version_minor":25}}]`
	widget = "apiVersion: example.k8s.io/v1beta1\nkind: Widget\nmetadata:\n  name: widget\n"
)

func checkWithStore(t *testing.T, location string) (msg.Result, error) {
	t.Helper()

	ctr := container.Container{Config: config.ServerConfig{KubepugGeneratedStore: location}}

	return checkApp(t.Context(), ctr, "app", "1.30.0", []string{widget})
}

func TestCheckAppLeavesNoTemporaryFiles(t *testing.T) {
	for name, status := range map[string]int{
		"store downloaded":                     http.StatusOK,
		"store downloaded with another status": http.StatusNonAuthoritativeInfo,
		"download refused":                     http.StatusNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(store))
			}))
			t.Cleanup(server.Close)

			result, err := checkWithStore(t, server.URL+"/data.json")
			if status == http.StatusNotFound {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, pkg.StateWarning, result.State)
			}

			left, err := os.ReadDir(os.TempDir())
			require.NoError(t, err)
			assert.Empty(t, left)
		})
	}
}

func TestCheckAppKeepsALocalStore(t *testing.T) {
	local := filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(local, []byte(store), 0o600))

	result, err := checkWithStore(t, local)
	require.NoError(t, err)
	assert.Equal(t, pkg.StateWarning, result.State)

	kept, err := os.ReadFile(local)
	require.NoError(t, err)
	assert.Equal(t, store, string(kept))
}
