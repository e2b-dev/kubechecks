package argo_client

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

// containsGlob reports whether the path includes glob meta-characters that
// filepath.Glob would expand (*, ?, or a character class).
func containsGlob(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

// copyGlobValueFiles expands a glob-style Helm value file path (resolved
// relative to srcAppPath) and copies each matching file into destAppDir,
// preserving its location relative to the source app directory. The original
// glob entry is left in source.Helm.ValueFiles so the Argo CD repo server can
// expand it once it receives the package.
//
// repoRoot anchors the source side and destDir anchors the destination side.
// The function rejects absolute valueFile paths and any expanded match or
// computed destination that escapes its respective root, so a maliciously
// crafted Application spec cannot read or write files outside the repo / temp
// package directory.
func copyGlobValueFiles(repoRoot, destDir, srcAppPath, destAppDir string, source v1alpha1.ApplicationSource, valueFile string) error {
	if filepath.IsAbs(valueFile) {
		return fmt.Errorf("absolute value file paths are not permitted: %q", valueFile)
	}

	pattern := filepath.Join(srcAppPath, valueFile)
	if inside, err := writtenInside(pattern, repoRoot); err != nil {
		return err
	} else if !inside {
		return fmt.Errorf("glob value file %q escapes repo root", valueFile)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return errors.Wrapf(err, "failed to expand glob %q", valueFile)
	}

	if len(matches) == 0 {
		if source.Helm != nil && source.Helm.IgnoreMissingValueFiles {
			log.Debug().Caller().
				Str("valueFile", valueFile).
				Msg("glob value file matched no files, skipping (IgnoreMissingValueFiles is true)")
			return nil
		}
		return fmt.Errorf("glob value file %q matched no files", valueFile)
	}

	resolvedRepoRoot, err := resolveExisting(repoRoot)
	if err != nil {
		return errors.Wrap(err, "failed to resolve repo root")
	}
	absDestDir, err := absClean(destDir)
	if err != nil {
		return errors.Wrap(err, "failed to resolve package dir")
	}

	for _, match := range matches {
		resolvedMatch, err := resolveExisting(match)
		if err != nil {
			return errors.Wrapf(err, "failed to resolve %q", match)
		}
		if !isWithin(resolvedMatch, resolvedRepoRoot) {
			return fmt.Errorf("globbed value file %q escapes repo root", match)
		}

		relFromAppPath, err := filepath.Rel(srcAppPath, match)
		if err != nil {
			return errors.Wrapf(err, "failed to compute relative path for %q", match)
		}

		dst := filepath.Join(destAppDir, relFromAppPath)
		absDst, err := absClean(dst)
		if err != nil {
			return errors.Wrapf(err, "failed to resolve destination %q", dst)
		}
		if !isWithin(absDst, absDestDir) {
			return fmt.Errorf("destination for %q escapes package dir", match)
		}

		if err := copyFile(match, dst); err != nil {
			if !ignoreValuesFileCopyError(source, match, err) {
				return errors.Wrapf(err, "failed to copy globbed value file %q", match)
			}
		}
	}
	return nil
}

// resolveExisting returns an absolute, symlink-resolved path. The path must
// exist on disk; this is used on the source side so symlinks planted inside
// the repo cannot redirect us outside it.
func resolveExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// absClean returns the cleaned absolute form of path without resolving
// symlinks. Used on the destination side, where the target path may not exist
// yet and where we control the tree (so symlink redirection is not a concern).
func absClean(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// insideRepo returns an error unless path lies inside the repository clone at
// root: as written and, when it exists, with its symbolic links resolved.
func insideRepo(path, root string) error {
	inside, err := writtenInside(path, root)
	if err != nil {
		return err
	}
	if !inside {
		return fmt.Errorf("%q is outside the repository", path)
	}

	resolvedPath, err := resolveExisting(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "failed to resolve %q", path)
	}
	resolvedRoot, err := resolveExisting(root)
	if err != nil {
		return errors.Wrapf(err, "failed to resolve %q", root)
	}
	if !isWithin(resolvedPath, resolvedRoot) {
		return fmt.Errorf("%q is outside the repository", path)
	}

	return nil
}

// linksInsideRepo returns an error if dir, a directory in the repository clone
// at root, holds a symbolic link that leads outside the clone. Copying follows
// links, so this is checked before a directory is copied.
func linksInsideRepo(dir, root string) error {
	// Walking does not enter a directory through a link, so walk what dir
	// names, and compare what is found there with what root names.
	resolvedDir, err := resolveExisting(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil // the copy that follows reports what is missing
	}
	if err != nil {
		return err
	}
	resolvedRoot, err := resolveExisting(root)
	if err != nil {
		return err
	}

	return filepath.WalkDir(resolvedDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.Type()&fs.ModeSymlink == 0 {
			return err
		}

		return insideRepo(path, resolvedRoot)
	})
}

// packageRoots are the two directories packaging works between: the repository
// clone it reads and the package directory it fills.
type packageRoots struct {
	repo string
	pkg  string
}

// check returns an error unless src lies inside the clone and dst inside the
// package directory. Each is checked on its own: a path can leave the clone
// and come back to it by name, and the same path then leaves the package.
func (r packageRoots) check(src, dst string) error {
	if err := insideRepo(src, r.repo); err != nil {
		return err
	}

	inside, err := writtenInside(dst, r.pkg)
	if err != nil {
		return err
	}
	if !inside {
		return fmt.Errorf("%q is outside the package directory", dst)
	}

	return nil
}

// writtenInside reports whether path, as written, lies inside root.
func writtenInside(path, root string) (bool, error) {
	absPath, err := absClean(path)
	if err != nil {
		return false, err
	}
	absRoot, err := absClean(root)
	if err != nil {
		return false, err
	}

	return isWithin(absPath, absRoot), nil
}

// isWithin reports whether child is identical to parent or sits inside it.
// Both arguments must be cleaned absolute paths.
func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
