package argo_client

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapier/kubechecks/pkg/git"
)

const (
	cloneURL       = "https://example.com/app.git"
	outsideContent = "outside: true\n"

	outsideRepository = "outside the repository"
	outsidePackage    = "outside the package directory"
)

// cloneBesideOtherFiles lays out, below one test directory:
//
//	z/work/clone/                   the repository clone, holding files and links
//	z/work/outside.yaml             a file beside the clone
//	z/work/common/values.yaml       a directory beside the clone
//	z/work/common/kustomization.yaml
//	z/tmp/                          the temporary directory, where packages are made
//
// Nothing a case does, on any version of the code, leaves the test directory.
// It returns the clone and the temporary directory.
func cloneBesideOtherFiles(t *testing.T, files, links map[string]string) (*git.Repo, string) {
	t.Helper()

	work := filepath.Join(t.TempDir(), "z", "work")
	write := func(path, content string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	clone := filepath.Join(work, "clone")
	require.NoError(t, os.MkdirAll(clone, 0o755))
	for name, content := range files {
		write(filepath.Join(clone, name), content)
	}
	for name, target := range links {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(clone, name)), 0o755))
		require.NoError(t, os.Symlink(target, filepath.Join(clone, name)))
	}
	write(filepath.Join(work, "outside.yaml"), outsideContent)
	write(filepath.Join(work, "common", "values.yaml"), outsideContent)
	write(filepath.Join(work, "common", "kustomization.yaml"), outsideContent)

	tmp := filepath.Join(filepath.Dir(work), "tmp")
	require.NoError(t, os.Mkdir(tmp, 0o755))
	t.Setenv("TMPDIR", tmp)

	return &git.Repo{Directory: clone, CloneURL: cloneURL}, tmp
}

func TestPackageAppRefusesPathsOutsideTheRepository(t *testing.T) {
	const chart = "name: app\n"
	helm := func(valueFiles ...string) *v1alpha1.ApplicationSourceHelm {
		return &v1alpha1.ApplicationSourceHelm{ValueFiles: valueFiles, IgnoreMissingValueFiles: true}
	}
	dependsOn := func(repository string) string {
		return "dependencies:\n- name: common\n  version: 1.0.0\n  repository: " + repository + "\n"
	}
	ref := []v1alpha1.ApplicationSource{{Ref: "values", RepoURL: cloneURL}}

	testCases := map[string]struct {
		files  map[string]string
		links  map[string]string
		source v1alpha1.ApplicationSource
		refs   []v1alpha1.ApplicationSource
		want   string
	}{
		"application path": {
			source: v1alpha1.ApplicationSource{Path: ".."},
			want:   outsideRepository,
		},
		"application path that leaves and returns": {
			files:  map[string]string{"app/Chart.yaml": chart},
			source: v1alpha1.ApplicationSource{Path: "../clone/app"},
			want:   outsidePackage,
		},
		"application directory that is a link": {
			files:  map[string]string{"real/Chart.yaml": chart},
			links:  map[string]string{"real/values.yaml": "../../outside.yaml", "app": "real"},
			source: v1alpha1.ApplicationSource{Path: "app"},
			want:   outsideRepository,
		},
		"chart dependency": {
			files:  map[string]string{"app/Chart.yaml": dependsOn("file://../../common")},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm()},
			want:   outsideRepository,
		},
		"chart dependency that does not exist": {
			files:  map[string]string{"app/Chart.yaml": dependsOn("file://../../missing")},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm()},
			want:   outsideRepository,
		},
		"chart dependency that leaves and returns": {
			files:  map[string]string{"app/Chart.yaml": dependsOn("file://../../clone/shared"), "shared/values.yaml": "shared: true\n"},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm()},
			want:   outsidePackage,
		},
		"kustomize patch": {
			files:  map[string]string{"app/kustomization.yaml": "patches:\n- path: ../../outside.yaml\n"},
			source: v1alpha1.ApplicationSource{Path: "app"},
			want:   outsideRepository,
		},
		"chart dependency that holds a link": {
			files:  map[string]string{"app/Chart.yaml": dependsOn("file://../shared"), "shared/Chart.yaml": chart},
			links:  map[string]string{"shared/values.yaml": "../../outside.yaml"},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm()},
			want:   outsideRepository,
		},
		"kustomize base through a link": {
			files:  map[string]string{"app/kustomization.yaml": "resources:\n- ../base\n"},
			links:  map[string]string{"base": "../common"},
			source: v1alpha1.ApplicationSource{Path: "app"},
			want:   "failed to process kustomization dependencies",
		},
		"value file": {
			files:  map[string]string{"app/Chart.yaml": chart},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm("../../outside.yaml")},
			want:   outsideRepository,
		},
		"value file that leaves and returns": {
			files:  map[string]string{"app/Chart.yaml": chart, "shared/values.yaml": "shared: true\n"},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm("../../clone/shared/values.yaml")},
			want:   outsidePackage,
		},
		"value files matched by a pattern": {
			files:  map[string]string{"app/Chart.yaml": chart},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm("../../nothing-*.yaml")},
			want:   "escapes repo root",
		},
		"value file of a ref": {
			files:  map[string]string{"app/Chart.yaml": chart},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm("$values/../outside.yaml")},
			refs:   ref,
			want:   outsideRepository,
		},
		"value file of a ref that leaves and returns": {
			files:  map[string]string{"app/Chart.yaml": chart, "shared/values.yaml": "shared: true\n"},
			source: v1alpha1.ApplicationSource{Path: "app", Helm: helm("$values/../../../z/work/clone/shared/values.yaml")},
			refs:   ref,
			want:   outsidePackage,
		},
		"symbolic link": {
			files:  map[string]string{"app/Chart.yaml": chart},
			links:  map[string]string{"app/values.yaml": "../../outside.yaml"},
			source: v1alpha1.ApplicationSource{Path: "app"},
			want:   outsideRepository,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			repo, tmp := cloneBesideOtherFiles(t, tc.files, tc.links)

			_, err := packageApp(t.Context(), tc.source, tc.refs, repo, nil)
			require.ErrorContains(t, err, tc.want)

			entries, err := os.ReadDir(tmp)
			require.NoError(t, err)
			for _, entry := range entries {
				assert.True(t, strings.HasPrefix(entry.Name(), "package-"), "%s was written beside the package", entry.Name())
			}

			require.NoError(t, filepath.WalkDir(tmp, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}

				content, err := os.ReadFile(path)
				assert.NotEqual(t, outsideContent, string(content), "%s was packaged from outside the repository", path)

				return err
			}))
		})
	}
}

func TestPackageAppFollowsLinksInsideTheRepository(t *testing.T) {
	repo, _ := cloneBesideOtherFiles(t,
		map[string]string{"app/Chart.yaml": "name: app\n", "shared/values.yaml": "shared: true\n"},
		map[string]string{"app/values.yaml": "../shared/values.yaml"})

	path, err := packageApp(t.Context(), v1alpha1.ApplicationSource{Path: "app"}, nil, repo, nil)
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(path, "app", "values.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "shared: true\n", string(content))
}

func TestPackageAppReportsAMissingApplicationPathAsACopyFailure(t *testing.T) {
	repo, _ := cloneBesideOtherFiles(t, nil, nil)

	_, err := packageApp(t.Context(), v1alpha1.ApplicationSource{Path: "missing"}, nil, repo, nil)
	require.ErrorContains(t, err, "failed to copy base directory")
}

func TestPackageAppSkipsAPatternThatCannotMatch(t *testing.T) {
	repo, _ := cloneBesideOtherFiles(t, map[string]string{"app/Chart.yaml": "name: app\n", "app/values.yaml": "a: 1\n"}, nil)
	source := v1alpha1.ApplicationSource{Path: "app", Helm: &v1alpha1.ApplicationSourceHelm{
		ValueFiles:              []string{"values.yaml/*.yaml"},
		IgnoreMissingValueFiles: true,
	}}

	_, err := packageApp(t.Context(), source, nil, repo, nil)
	require.NoError(t, err)
}
