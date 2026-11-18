# e2b downstream branch

This fork carries patches that are not yet in `zapier/kubechecks` and builds the kubechecks image e2b runs. Three
kinds of branch keep that separate from upstreaming.

| Branch | What it is | Rules |
|---|---|---|
| `main` | A mirror of upstream's `main`. | Fast-forward from upstream only. Never commit to it, and never add fork-only files (this one, the image workflow) to it. |
| `feat/*`, `fix/*` | One change each, cut from upstream `main`. | Cut from `main` so the same branch opens a clean upstream PR. |
| `downstream` | `main` plus the feature branches e2b runs. | Merge branches in with merge commits, not squashes, so each branch's commits stay identical to what was sent upstream. Fork-only files live here. |

## Taking a change

1. `git checkout -b fix/thing upstream/main`, then make and push the change. Open the upstream PR from it.
2. To run it before upstream merges, merge it into `downstream`:
   `git checkout downstream && git merge --no-ff upstream/fix/thing` (or `origin/fix/thing` if the branch lives in this fork).
3. When upstream merges its own version (usually squashed), bring `main` forward and merge it into `downstream`: the
   content is identical, so it is normally clean. Resolve by hand if upstream changed the patch in review.

A branch for upstream must not include the `downstream`-only files.

## Staying current

Fast-forward `main` from upstream, then merge `main` into `downstream`:

```
git fetch upstream            # git remote add upstream https://github.com/zapier/kubechecks.git
git checkout main && git merge --ff-only upstream/main && git push origin main
git checkout downstream && git merge main
```

## Releasing an image

Tag a commit that is on `downstream` as `vX.Y.Z-e2b.N`, where `X.Y.Z` is the upstream release `downstream` is based
on and `N` counts our builds on top of it (`v3.7.0-e2b.1`, then `v3.7.0-e2b.2`, ...):

```
git checkout downstream && git tag -s v3.7.0-e2b.1 && git push origin v3.7.0-e2b.1
```

`.github/workflows/e2b-image.yaml` then builds the same distroless multi-arch (amd64, arm64) image upstream's
release workflow does and pushes `ghcr.io/e2b-dev/kubechecks:<tag>`. It refuses a tag whose commit is not on
`downstream`. Upstream's helm chart works unchanged with it: set `deployment.image.name` to
`ghcr.io/e2b-dev/kubechecks` and `deployment.image.tag` to the release.

The workflow logs in to GitHub Container Registry with its own `GITHUB_TOKEN`, so it needs no secrets or variables.
The package GitHub creates on the first push starts private: after the first release, open its settings
(github.com/orgs/e2b-dev/packages) and set its visibility to Public, and confirm the repository has write access
under "Manage Actions access".

## Upstream workflows

Upstream's workflows are not gated to `zapier/kubechecks`, and some of them publish (chart-releaser on pushes to
`main`, images on releases and PRs). They are disabled in this fork's Actions settings, leaving only
`e2b image build` enabled. Re-check that after upstream adds a workflow.
