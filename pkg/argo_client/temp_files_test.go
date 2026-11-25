package argo_client

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/argoproj/argo-cd/v3/pkg/apiclient"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient/cluster"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient/project"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient/settings"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/zapier/kubechecks/pkg/config"
	"github.com/zapier/kubechecks/pkg/git"
	"github.com/zapier/kubechecks/pkg/vcs"
)

// argoUntilProject answers the calls a render makes before it asks for the
// project client, and refuses that one: by then the render has written its
// files.
type argoUntilProject struct {
	apiclient.Client

	filesWritten []os.DirEntry
}

func (*argoUntilProject) NewClusterClient() (io.Closer, cluster.ClusterServiceClient, error) {
	return io.NopCloser(nil), someCluster{}, nil
}

func (*argoUntilProject) NewSettingsClient() (io.Closer, settings.SettingsServiceClient, error) {
	return io.NopCloser(nil), someSettings{}, nil
}

func (a *argoUntilProject) NewProjectClient() (io.Closer, project.ProjectServiceClient, error) {
	a.filesWritten, _ = os.ReadDir(os.TempDir())

	return nil, nil, errors.New("no project client")
}

type someCluster struct{ cluster.ClusterServiceClient }

func (someCluster) Get(context.Context, *cluster.ClusterQuery, ...grpc.CallOption) (*v1alpha1.Cluster, error) {
	return &v1alpha1.Cluster{}, nil
}

type someSettings struct{ settings.SettingsServiceClient }

func (someSettings) Get(context.Context, *settings.SettingsQuery, ...grpc.CallOption) (*settings.Settings, error) {
	return &settings.Settings{}, nil
}

// repoWithManifest returns a repository clone that holds one manifest, and the
// manifest's path.
func repoWithManifest(t *testing.T) (*git.Repo, string) {
	t.Helper()

	repo := &git.Repo{Directory: t.TempDir(), CloneURL: "https://example.com/app.git"}
	manifest := filepath.Join(repo.Directory, "configmap.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("kind: ConfigMap"), 0o600))

	return repo, manifest
}

func TestGenerateManifestsLeavesNoTemporaryFiles(t *testing.T) {
	// A packaged app writes its package directory and the tarball's directory;
	// a repository sent whole writes only the tarball's.
	for name, mode := range map[string]struct {
		sendFullRepository bool
		written            int
	}{
		"packaged app":    {sendFullRepository: false, written: 2},
		"full repository": {sendFullRepository: true, written: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			repo, manifest := repoWithManifest(t)

			argo := &argoUntilProject{}
			client := &ArgoClient{client: argo, cfg: config.ServerConfig{ArgoCDSendFullRepository: mode.sendFullRepository}}

			_, err := client.generateManifests(t.Context(), v1alpha1.Application{}, v1alpha1.ApplicationSource{}, nil, vcs.PullRequest{},
				func(context.Context, string, string) (*git.Repo, error) { return repo, nil })
			require.ErrorContains(t, err, "no project client")
			require.Len(t, argo.filesWritten, mode.written)

			left, err := os.ReadDir(os.TempDir())
			require.NoError(t, err)
			assert.Empty(t, left)
			assert.FileExists(t, manifest)
		})
	}
}

func TestPackageAppRemovesItsDirectoryWhenItFails(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	repo, manifest := repoWithManifest(t)

	_, err := packageApp(t.Context(), v1alpha1.ApplicationSource{Path: "missing"}, nil, repo, nil)
	require.ErrorContains(t, err, "failed to copy base directory")

	left, err := os.ReadDir(os.TempDir())
	require.NoError(t, err)
	assert.Empty(t, left)
	assert.FileExists(t, manifest)
}

// A worker recovers a panic and the process lives on, so a panic must not
// leave the directory behind either.
func TestPackageAppRemovesItsDirectoryWhenItPanics(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	repo, manifest := repoWithManifest(t)
	source := v1alpha1.ApplicationSource{Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/values.yaml"}}}
	refs := []v1alpha1.ApplicationSource{{Ref: "values", RepoURL: "https://example.com/values.git"}}

	assert.Panics(t, func() {
		_, _ = packageApp(t.Context(), source, refs, repo, func(context.Context, string, string) (*git.Repo, error) {
			panic("clone panicked")
		})
	})

	left, err := os.ReadDir(os.TempDir())
	require.NoError(t, err)
	assert.Empty(t, left)
	assert.FileExists(t, manifest)
}
