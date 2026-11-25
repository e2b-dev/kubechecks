package argo_client

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/zapier/kubechecks/pkg/config"
)

func TestArgoSettingsStartsItsInformersOnce(t *testing.T) {
	k8s := fake.NewClientset()
	client := &ArgoClient{k8s: k8s, cfg: config.ServerConfig{ArgoCDNamespace: "argocd"}}

	// A new settings manager lists ConfigMaps and Secrets to fill its informers.
	lists := func() int {
		count := 0
		for _, action := range k8s.Actions() {
			if action.GetVerb() == "list" {
				count++
			}
		}

		return count
	}
	render := func() {
		_, argoDB := client.argoSettings()
		_, err := argoDB.ListHelmRepositories(context.Background())
		require.NoError(t, err)
	}

	render()
	afterFirst := lists()
	require.Positive(t, afterFirst)

	for range 4 {
		render()
	}

	assert.Equal(t, afterFirst, lists())
}

// A settings manager built anywhere else would start informers that nothing
// stops.
func TestOnlyArgoSettingsBuildsASettingsManager(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	var callers []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}

		source, err := os.ReadFile(path)
		for range strings.Count(string(source), "NewSettingsManager(") {
			callers = append(callers, path)
		}

		return err
	})
	require.NoError(t, err)

	assert.Equal(t, []string{filepath.Join(root, "pkg", "argo_client", "client.go")}, callers)
}
